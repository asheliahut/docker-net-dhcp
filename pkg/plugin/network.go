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

	opts, err := decodeOpts(n.Options)
	if err != nil {
		return dummy, fmt.Errorf("failed to parse options: %w", err)
	}

	return opts, nil
}

// CreateEndpoint creates a network interface using the unified DHCP manager
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	log.WithFields(log.Fields{
		"endpoint_id": r.EndpointID,
		"network_id":  r.NetworkID,
		"address":     r.Interface.Address,
		"address_v6":  r.Interface.AddressIPv6,
		"mac_address": r.Interface.MacAddress,
		"options":     r.Options,
	}).Debug("CreateEndpoint")

	ctx, cancel := context.WithTimeout(ctx, p.awaitTimeout)
	defer cancel()

	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	if r.Interface != nil && (r.Interface.Address != "" || r.Interface.AddressIPv6 != "") {
		// TODO: Should we allow static IP's somehow?
		return res, util.ErrIPAM
	}

	// Get network options with retry logic
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

	// Extract hostname from options
	hostname := ""
	if r.Options != nil {
		if hostI, ok := r.Options["hostname"]; ok {
			hostname, _ = hostI.(string)
		}
	}

	// Parse required MAC address if provided
	var requiredMAC net.HardwareAddr
	if r.Interface.MacAddress != "" {
		requiredMAC, err = net.ParseMAC(r.Interface.MacAddress)
		if err != nil {
			return res, util.ErrMACAddress
		}
	}

	// Create unified DHCP manager
	manager := NewUnifiedDHCPManager(p.docker, r.NetworkID, r.EndpointID, opts, hostname, requiredMAC, p.awaitTimeout)

	// Create network interface
	if err := manager.CreateNetworkInterface(ctx); err != nil {
		return res, fmt.Errorf("failed to create network interface: %w", err)
	}

	// Cleanup on error
	defer func() {
		if err != nil {
			manager.Cleanup()
		}
	}()

	// Step 1: Acquire initial DHCP leases (with hostname for lease handoff)
	if err := manager.AcquireInitialLeases(ctx, hostname); err != nil {
		return res, fmt.Errorf("failed to acquire DHCP leases: %w", err)
	}

	// Get interface information for Docker
	ipv4, ipv6, macAddr, gateway := manager.GetInterfaceInfo()
	res.Interface.Address = ipv4
	res.Interface.AddressIPv6 = ipv6
	res.Interface.MacAddress = macAddr

	// Store manager for join phase
	hostName, _ := vethPairNames(r.EndpointID)
	p.unifiedDHCP[r.EndpointID] = manager

	// Store join hints for compatibility
	hint := p.joinHints[r.EndpointID]
	if manager.IPv4 != nil {
		hint.IPv4 = manager.IPv4
	}
	if manager.IPv6 != nil {
		hint.IPv6 = manager.IPv6
	}
	hint.Gateway = gateway
	hint.MAC = manager.finalMAC
	p.joinHints[r.EndpointID] = hint

	log.WithFields(log.Fields{
		"network":     r.NetworkID[:12],
		"endpoint":    r.EndpointID[:12],
		"mac_address": macAddr,
		"ip":          ipv4,
		"ipv6":        ipv6,
		"gateway":     gateway,
		"hostname":    hostName,
	}).Info("Endpoint created with unified DHCP manager")

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

// DeleteEndpoint deletes the network interface and cleans up resources
func (p *Plugin) DeleteEndpoint(r DeleteEndpointRequest) error {
	// Try unified manager first
	if manager, exists := p.unifiedDHCP[r.EndpointID]; exists {
		if err := manager.Cleanup(); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
			}).Warn("Failed to cleanup unified DHCP manager")
		}
		delete(p.unifiedDHCP, r.EndpointID)

		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Info("Endpoint deleted (unified manager)")

		// Clean up join hints
		delete(p.joinHints, r.EndpointID)
		return nil
	}

	// Fallback to legacy cleanup for backward compatibility
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
	}).Info("Endpoint deleted (legacy)")

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

	// Start persistent DHCP clients in the background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		defer cancel()

		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
			"sandbox":  r.SandboxKey,
			"hostname": hostname,
		}).Debug("[Join] Starting persistent DHCP clients")

		// Get the unified manager that was created in CreateEndpoint
		manager, exists := p.unifiedDHCP[r.EndpointID]
		if !exists {
			log.WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
				"sandbox":  r.SandboxKey,
			}).Error("No unified DHCP manager found for endpoint")
			return
		}

		// Step 2: Start persistent DHCP clients with hostname registration
		if err := manager.StartPersistentClients(ctx, r.SandboxKey, hostname); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
				"sandbox":  r.SandboxKey,
			}).Error("Failed to start persistent DHCP clients")
			return
		}

		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
			"sandbox":  r.SandboxKey,
		}).Info("Persistent DHCP clients started successfully")
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
	// Try unified manager first
	if manager, exists := p.unifiedDHCP[r.EndpointID]; exists {
		delete(p.unifiedDHCP, r.EndpointID)

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
				}).Warn("Error during unified manager stop, but continuing")
			}
		case <-cleanupCtx.Done():
			log.WithFields(log.Fields{
				"network":  r.NetworkID[:12],
				"endpoint": r.EndpointID[:12],
			}).Warn("Timeout during unified manager cleanup, forcing stop")

			// Force cleanup in background
			go manager.forceCleanup()
		}

		log.WithFields(log.Fields{
			"network":  r.NetworkID[:12],
			"endpoint": r.EndpointID[:12],
		}).Info("Sandbox left endpoint (unified manager)")

		return nil
	}

	// No unified manager found
	log.WithFields(log.Fields{
		"network":  r.NetworkID[:12],
		"endpoint": r.EndpointID[:12],
	}).Warn("No DHCP manager found for endpoint")

	return util.ErrNoSandbox
}
