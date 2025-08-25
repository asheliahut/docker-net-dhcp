package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/docker/docker/api/types/network"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/asheliahut/docker-net-dhcp/pkg/dhcp"
	"github.com/asheliahut/docker-net-dhcp/pkg/util"
)

// CLIOptionsKey is the key used in create network options by the CLI for custom options
const CLIOptionsKey string = "com.docker.network.generic"

// Implementations of the endpoints described in
// https://github.com/moby/libnetwork/blob/master/docs/remote.md

// CreateNetwork "creates" a new DHCP network (just checks if the provided bridge exists and the null IPAM driver is
// used)
func (p *Plugin) CreateNetwork(r CreateNetworkRequest) error {
	log.WithField("options", r.Options).Debug("CreateNetwork options")

	opts, err := decodeOpts(r.Options[util.OptionsKeyGeneric])
	if err != nil {
		return fmt.Errorf("failed to decode network options: %w", err)
	}

	if opts.Bridge == "" {
		return util.ErrBridgeRequired
	}

	for _, d := range r.IPv4Data {
		if d.AddressSpace != "null" || d.Pool != "0.0.0.0/0" {
			return util.ErrIPAM
		}
	}

	link, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to lookup interface %v: %w", opts.Bridge, err)
	}
	if link.Type() != "bridge" {
		return util.ErrNotBridge
	}

	if !opts.IgnoreConflicts {
		v4Addrs, err := netlink.AddrList(link, unix.AF_INET)
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv4 addresses for %v: %w", opts.Bridge, err)
		}
		v6Addrs, err := netlink.AddrList(link, unix.AF_INET6)
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv6 addresses for %v: %w", opts.Bridge, err)
		}
		bridgeAddrs := append(v4Addrs, v6Addrs...)

		nets, err := p.docker.NetworkList(context.Background(), network.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to retrieve list of networks from Docker: %w", err)
		}

		// Make sure the addresses on this bridge aren't used by another network
		for _, n := range nets {
			if IsDHCPPlugin(n.Driver) {
				otherOpts, err := decodeOpts(n.Options)
				if err != nil {
					log.
						WithField("network", n.Name).
						WithError(err).
						Warn("Failed to parse other DHCP network's options")
				} else if otherOpts.Bridge == opts.Bridge {
					return util.ErrBridgeUsed
				}
			}
			if n.IPAM.Driver == "null" {
				// Null driver networks will have 0.0.0.0/0 which covers any address range!
				continue
			}

			for _, c := range n.IPAM.Config {
				_, dockerCIDR, err := net.ParseCIDR(c.Subnet)
				if err != nil {
					return fmt.Errorf("failed to parse subnet %v on Docker network %v: %w", c.Subnet, n.ID, err)
				}
				if bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 32)) || bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 128)) {
					// Last check to make sure the network isn't 0.0.0.0/0 or ::/0 (which would always pass the check below)
					continue
				}

				for _, bridgeAddr := range bridgeAddrs {
					if bridgeAddr.IPNet.Contains(dockerCIDR.IP) || dockerCIDR.Contains(bridgeAddr.IP) {
						return util.ErrBridgeUsed
					}
				}
			}
		}
	}

	log.WithFields(log.Fields{
		"network": r.NetworkID,
		"bridge":  opts.Bridge,
		"ipv6":    opts.IPv6,
	}).Info("Network created")

	return nil
}

// DeleteNetwork "deletes" a DHCP network (does nothing, the bridge is managed by the user)
func (p *Plugin) DeleteNetwork(r DeleteNetworkRequest) error {
	log.WithField("network", r.NetworkID).Info("Network deleted")
	return nil
}

func vethPairNames(id string) (string, string) {
	return "dh-" + id[:12], id[:12] + "-dh"
}

func (p *Plugin) netOptions(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	dummy := DHCPNetworkOptions{}
	n, err := p.docker.NetworkInspect(ctx, id, network.InspectOptions{})
	if err != nil {
		return dummy, fmt.Errorf("failed to get info from Docker: %w", err)
	}

	log.Debugf("VP>>>>>> CreateEndpoint netOptions %+v", n)
	log.Debugf("VP>>>>>> CreateEndpoint netOptions Options %+v", n.Options)
	log.Debugf("VP>>>>>> CreateEndpoint netOptions Containers %+v", n.Containers)
	log.Debugf("VP>>>>>> CreateEndpoint netOptions Labels %+v", n.Labels)
	log.Debugf("VP>>>>>> CreateEndpoint netOptions Services %+v", n.Services)
	log.Debugf("VP>>>>>> CreateEndpoint netOptions Peers %+v", n.Peers)

	opts, err := decodeOpts(n.Options)
	if err != nil {
		return dummy, fmt.Errorf("failed to parse options: %w", err)
	}

	return opts, nil
}

// CreateEndpoint creates a veth pair and uses DHCP to acquire an initial IP address on the container end. Docker will
// move the interface into the container's namespace and apply the address.
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	log.WithField("options", r.Options).Debug("CreateEndpoint options")
	log.Debugf("VP>>>>>> CreateEndpoint Request %+v", r)
	log.Debugf("VP>>>>>> CreateEndpoint Request Interface %+v", r.Interface)
	ctx, cancel := context.WithTimeout(ctx, p.awaitTimeout)
	defer cancel()
	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	if r.Interface != nil && (r.Interface.Address != "" || r.Interface.AddressIPv6 != "") {
		// TODO: Should we allow static IP's somehow?
		return res, util.ErrIPAM
	}
	var opts DHCPNetworkOptions
	var err error
	backoff := 2 * time.Second
	retries := 2
	for retries >= 0 {
		opts, err = p.netOptions(ctx, r.NetworkID)
		if err == nil {
			break
		}
		if retries == 0 {
			return res, fmt.Errorf("failed to get network options: %w", err)
		}

		log.Debugf("failed to get network options [backoff %s]: %v", backoff, err)
		retries--
		time.Sleep(backoff)
		backoff *= 2
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return res, fmt.Errorf("failed to get bridge interface: %w", err)
	}

	log.Debugf("VP>>>>>> CreateEndpoint 2")

	hostName, ctrName := vethPairNames(r.EndpointID)
	la := netlink.NewLinkAttrs()
	la.Name = hostName
	hostLink := &netlink.Veth{
		LinkAttrs: la,
		PeerName:  ctrName,
	}

	log.Debugf("VP>>>>>> CreateEndpoint 3")

	if r.Interface.MacAddress != "" {
		addr, err := net.ParseMAC(r.Interface.MacAddress)
		if err != nil {
			return res, util.ErrMACAddress
		}

		hostLink.PeerHardwareAddr = addr
	}

	if err := netlink.LinkAdd(hostLink); err != nil {
		return res, fmt.Errorf("failed to create veth pair: %w", err)
	}
	if err := func() error {
		if err := netlink.LinkSetUp(hostLink); err != nil {
			return fmt.Errorf("failed to set host side link of veth pair up: %w", err)
		}

		ctrLink, err := netlink.LinkByName(ctrName)
		if err != nil {
			return fmt.Errorf("failed to find container side of veth pair: %w", err)
		}
		if err := netlink.LinkSetUp(ctrLink); err != nil {
			return fmt.Errorf("failed to set container side link of veth pair up: %w", err)
		}

		// Store the desired MAC address before any operations that might change it
		var finalMAC net.HardwareAddr
		if r.Interface.MacAddress != "" {
			var err error
			finalMAC, err = net.ParseMAC(r.Interface.MacAddress)
			if err != nil {
				return fmt.Errorf("failed to parse provided MAC address: %w", err)
			}
		} else {
			// Use the kernel-assigned random MAC address
			finalMAC = ctrLink.Attrs().HardwareAddr
			res.Interface.MacAddress = finalMAC.String()
		}

		if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
			return fmt.Errorf("failed to attach host side link of veth peer to bridge: %w", err)
		}

		// Set MAC address after LinkSetMaster since the kernel often resets it during bridge attachment
		if err := netlink.LinkSetHardwareAddr(ctrLink, finalMAC); err != nil {
			return fmt.Errorf("failed to set container side of veth pair's MAC address: %w", err)
		}

		log.Debug("VP>>>>>> CreateEndpoint 4 - MAC address set, preparing DHCP")

		timeout := defaultLeaseTimeout
		if opts.LeaseTimeout != 0 {
			timeout = opts.LeaseTimeout
		}

		hostname := ""
		if r.Options != nil {
			if idI, ok := r.Options["hostname"]; ok {
				hostname, _ = idI.(string)
			}
		}

		// Get IPv4 address via DHCP using consistent MAC address
		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		log.WithFields(log.Fields{
			"interface": ctrName,
			"hostname":  hostname,
			"timeout":   timeout,
			"mac":       finalMAC.String(),
		}).Debug("[dbg] calling GetIP with interface details")

		// Create persistent DHCP client for lease handoff
		initialClient, err := dhcp.NewDHCPClient(ctrName, &dhcp.DHCPClientOptions{
			Hostname: hostname,
		})
		if err != nil {
			return fmt.Errorf("failed to create initial DHCP client: %w", err)
		}

		// Start the client
		events, err := initialClient.Start()
		if err != nil {
			return fmt.Errorf("failed to start initial DHCP client: %w", err)
		}

		// Wait for initial lease
		var info dhcp.Info
		select {
		case event := <-events:
			if event.Type == "bound" {
				info = event.Data
				log.WithFields(log.Fields{
					"interface": ctrName,
					"ip":        info.IP,
					"gateway":   info.Gateway,
				}).Debug("Initial DHCP client acquired lease")
			} else if event.Type == "leasefail" {
				initialClient.Finish(timeoutCtx)
				return fmt.Errorf("failed to get DHCP lease")
			} else {
				// Wait for bound event
				for {
					select {
					case nextEvent := <-events:
						if nextEvent.Type == "bound" {
							info = nextEvent.Data
							goto leaseAcquired
						} else if nextEvent.Type == "leasefail" {
							initialClient.Finish(timeoutCtx)
							return fmt.Errorf("failed to get DHCP lease")
						}
					case <-timeoutCtx.Done():
						initialClient.Finish(timeoutCtx)
						return fmt.Errorf("timeout waiting for DHCP lease")
					}
				}
			}
		case <-timeoutCtx.Done():
			initialClient.Finish(timeoutCtx)
			return fmt.Errorf("timeout waiting for DHCP lease")
		}

	leaseAcquired:
		// Store the client for lease handoff
		p.initialDHCP[r.EndpointID] = initialClient

		ipv4, err := netlink.ParseAddr(info.IP)
		if err != nil {
			initialClient.Finish(timeoutCtx)
			return fmt.Errorf("failed to parse initial IPv4 address: %w", err)
		}

		hint := p.joinHints[r.EndpointID]
		res.Interface.Address = info.IP
		hint.IPv4 = ipv4
		hint.Gateway = info.Gateway
		hint.MAC = finalMAC

		// Get IPv6 address if enabled - using native DHCPv6 client
		if opts.IPv6 {
			timeoutCtxV6, cancelV6 := context.WithTimeout(ctx, timeout)
			defer cancelV6()

			infoV6, err := dhcp.GetIP(timeoutCtxV6, ctrName, &dhcp.DHCPClientOptions{
				V6:       true,
				Hostname: hostname,
			})
			if err != nil {
				log.WithError(err).WithFields(log.Fields{
					"network":  r.NetworkID[:12],
					"endpoint": r.EndpointID[:12],
				}).Warn("Failed to get initial IPv6 address via DHCP, skipping IPv6 configuration")

				// Continue without IPv6 - container can use SLAAC or static configuration
				res.Interface.AddressIPv6 = ""
				hint.IPv6 = nil
			} else {
				ipv6, err := netlink.ParseAddr(infoV6.IP)
				if err != nil {
					log.WithError(err).WithFields(log.Fields{
						"network":  r.NetworkID[:12],
						"endpoint": r.EndpointID[:12],
						"ipv6":     infoV6.IP,
					}).Warn("Failed to parse initial IPv6 address, skipping IPv6 configuration")

					res.Interface.AddressIPv6 = ""
					hint.IPv6 = nil
				} else {
					res.Interface.AddressIPv6 = infoV6.IP
					hint.IPv6 = ipv6

					log.WithFields(log.Fields{
						"network":  r.NetworkID[:12],
						"endpoint": r.EndpointID[:12],
						"ipv6":     infoV6.IP,
					}).Info("IPv6 address acquired via DHCPv6")
				}
			}
		}

		p.joinHints[r.EndpointID] = hint

		return nil
	}(); err != nil {
		// Be sure to clean up the veth pair and initial client if any of this fails
		netlink.LinkDel(hostLink)
		
		// Clean up initial client if it was created
		if initialClient, exists := p.initialDHCP[r.EndpointID]; exists {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			initialClient.Finish(ctx)
			cancel()
			delete(p.initialDHCP, r.EndpointID)
		}
		
		return res, err
	}

	log.WithFields(log.Fields{
		"network":     r.NetworkID[:12],
		"endpoint":    r.EndpointID[:12],
		"mac_address": res.Interface.MacAddress,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     fmt.Sprintf("%#v", p.joinHints[r.EndpointID].Gateway),
		"hostname":    hostName,
	}).Info("Endpoint created")

	return res, nil
}

type operInfo struct {
	Bridge      string `mapstructure:"bridge"`
	HostVEth    string `mapstructure:"veth_host"`
	HostVEthMAC string `mapstructure:"veth_host_mac"`
}

// EndpointOperInfo retrieves some info about an existing endpoint
func (p *Plugin) EndpointOperInfo(ctx context.Context, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	hostName, _ := vethPairNames(r.EndpointID)
	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		return res, fmt.Errorf("failed to find host side of veth pair: %w", err)
	}

	info := operInfo{
		Bridge:      opts.Bridge,
		HostVEth:    hostName,
		HostVEthMAC: hostLink.Attrs().HardwareAddr.String(),
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode OperInfo: %w", err)
	}

	return res, nil
}

// DeleteEndpoint deletes the veth pair
func (p *Plugin) DeleteEndpoint(r DeleteEndpointRequest) error {
	hostName, _ := vethPairNames(r.EndpointID)
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		return fmt.Errorf("failed to lookup host veth interface %v: %w", hostName, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth pair: %w", err)
	}

	// Clean up any remaining initial DHCP client
	if initialClient, exists := p.initialDHCP[r.EndpointID]; exists {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := initialClient.Finish(ctx); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
			}).Warn("Failed to cleanly finish initial DHCP client during endpoint deletion")
		}
		cancel()
		delete(p.initialDHCP, r.EndpointID)
	}

	// Clean up join hints
	delete(p.joinHints, r.EndpointID)

	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
	}).Info("Endpoint deleted")

	return nil
}

func (p *Plugin) addRoutes(opts *DHCPNetworkOptions, v6 bool, bridge netlink.Link, r JoinRequest, hint joinHint, res *JoinResponse) error {
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}

	routes, err := netlink.RouteListFiltered(family, &netlink.Route{
		LinkIndex: bridge.Attrs().Index,
		Type:      unix.RTN_UNICAST,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TYPE)
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}

	logFields := log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
		"sandbox":  r.SandboxKey,
	}
	for _, route := range routes {
		if route.Dst == nil {
			// Default route
			switch family {
			case unix.AF_INET:
				if res.Gateway == "" {
					res.Gateway = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.Gateway).
						Info("[Join] Setting IPv4 gateway retrieved from bridge interface on host routing table")
				}
			case unix.AF_INET6:
				if res.GatewayIPv6 == "" {
					res.GatewayIPv6 = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.GatewayIPv6).
						Info("[Join] Setting IPv6 gateway retrieved from bridge interface on host routing table")
				}
			}

			continue
		}

		if opts.SkipRoutes {
			// Don't do static routes at all
			continue
		}

		if route.Protocol == unix.RTPROT_KERNEL ||
			(family == unix.AF_INET && hint.IPv4 != nil && route.Dst.Contains(hint.IPv4.IP)) ||
			(family == unix.AF_INET6 && hint.IPv6 != nil && route.Dst.Contains(hint.IPv6.IP)) {
			// Make sure to leave out the default on-link route created automatically for the IP(s) acquired by DHCP
			continue
		}

		staticRoute := &StaticRoute{
			Destination: route.Dst.String(),
			// Default to an on-link route
			RouteType: 1,
		}
		res.StaticRoutes = append(res.StaticRoutes, staticRoute)

		if route.Gw != nil {
			staticRoute.RouteType = 0
			staticRoute.NextHop = route.Gw.String()

			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				WithField("gateway", staticRoute.NextHop).
				Info("[Join] Adding route (via gateway) retrieved from bridge interface on host routing table")
		} else {
			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				Info("[Join] Adding on-link route retrieved from bridge interface on host routing table")
		}
	}

	return nil
}

// Join passes the veth name and route information (gateway from DHCP and existing routes on the host bridge) to Docker
// and starts a persistent DHCP client to maintain the lease on the acquired IP
func (p *Plugin) Join(ctx context.Context, r JoinRequest) (JoinResponse, error) {
	log.WithField("options", r.Options).Debug("Join options")
	res := JoinResponse{}

	// Extract hostname from options
	hostname := ""
	if r.Options != nil {
		if hostI, ok := r.Options["hostname"]; ok {
			hostname, _ = hostI.(string)
		}
	}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	_, ctrName := vethPairNames(r.EndpointID)

	res.InterfaceName = InterfaceName{
		SrcName:   ctrName,
		DstPrefix: opts.Bridge,
	}

	hint, ok := p.joinHints[r.EndpointID]
	if !ok {
		return res, util.ErrNoHint
	}
	delete(p.joinHints, r.EndpointID)

	if hint.Gateway != "" {
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
			"sandbox":  r.SandboxKey,
			"gateway":  hint.Gateway,
		}).Info("[Join] Setting IPv4 gateway retrieved from initial DHCP in CreateEndpoint")
		res.Gateway = hint.Gateway
	} else {
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
			"sandbox":  r.SandboxKey,
		}).Info("[Join] No initial gateway from CreateEndpoint, persistent DHCP client will handle networking")
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return res, fmt.Errorf("failed to get bridge interface: %w", err)
	}

	if err := p.addRoutes(&opts, false, bridge, r, hint, &res); err != nil {
		return res, err
	}
	if opts.IPv6 {
		if err := p.addRoutes(&opts, true, bridge, r, hint, &res); err != nil {
			return res, err
		}
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		defer cancel()

		m := newDHCPManager(p.docker, r, opts, hostname)
		m.LastIP = hint.IPv4
		m.LastIPv6 = hint.IPv6
		m.OriginalMAC = hint.MAC

		// Transfer the initial client to the manager for lease handoff
		if initialClient, exists := p.initialDHCP[r.EndpointID]; exists {
			m.leaseClient = initialClient
			delete(p.initialDHCP, r.EndpointID)
			log.WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
				"sandbox":  r.SandboxKey,
			}).Debug("Transferred initial DHCP client to manager for lease handoff")
		} else {
			log.WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
				"sandbox":  r.SandboxKey,
			}).Warn("No initial DHCP client found for lease handoff")
		}

		if err := m.Start(ctx); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
				"sandbox":  r.SandboxKey,
			}).Error("Failed to start persistent DHCP client")
			return
		}

		p.persistentDHCP[r.EndpointID] = m
	}()

	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
		"sandbox":  r.SandboxKey,
	}).Info("Joined sandbox to endpoint")

	return res, nil
}

// Leave stops the persistent DHCP client for an endpoint
func (p *Plugin) Leave(ctx context.Context, r LeaveRequest) error {
	manager, ok := p.persistentDHCP[r.EndpointID]
	if !ok {
		return util.ErrNoSandbox
	}
	delete(p.persistentDHCP, r.EndpointID)

	// Use timeout context for cleanup
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Stop manager with timeout
	done := make(chan error, 1)
	go func() {
		done <- manager.Stop()
	}()

	select {
	case err := <-done:
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
			}).Warn("Error during manager stop, but continuing")
		}
	case <-cleanupCtx.Done():
		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Warn("Timeout during manager cleanup, forcing stop")

		// Force cleanup in background
		go manager.forceCleanup()
	}

	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
	}).Info("Sandbox left endpoint")

	return nil
}
