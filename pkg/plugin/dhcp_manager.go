package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/asheliahut/docker-net-dhcp/pkg/dhcp"
	"github.com/asheliahut/docker-net-dhcp/pkg/util"
)

const pollTime = 100 * time.Millisecond

type dhcpManager struct {
	docker  *docker.Client
	joinReq JoinRequest
	opts    DHCPNetworkOptions

	LastIP      *netlink.Addr
	LastIPv6    *netlink.Addr
	OriginalMAC net.HardwareAddr // MAC address from CreateEndpoint

	nsPath    string
	hostname  string
	nsHandle  netns.NsHandle
	netHandle *netlink.Handle
	ctrLink   netlink.Link

	leaseClient  *dhcp.DHCPClient // For lease handoff
	stopChan     chan struct{}
	errChan      chan error
	errChanV6    chan error
	cleanupTimer *time.Timer
}

func newDHCPManager(docker *docker.Client, r JoinRequest, opts DHCPNetworkOptions) *dhcpManager {
	return &dhcpManager{
		docker:  docker,
		joinReq: r,
		opts:    opts,

		stopChan: make(chan struct{}),
	}
}

func (m *dhcpManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  m.joinReq.NetworkID[:12],
		"endpoint": m.joinReq.EndpointID[:12],
		"sandbox":  m.joinReq.SandboxKey,
		"is_ipv6":  v6,
	}
}

func (m *dhcpManager) handleBound(v6 bool, info dhcp.Info) error {
	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}

	// Check for IP conflicts before assignment
	if err := m.checkIPConflict(ip, v6); err != nil {
		return fmt.Errorf("IP conflict detected: %w", err)
	}

	// Set the IP address on the interface with atomic operation
	if err := m.atomicAddrAdd(ip); err != nil {
		return fmt.Errorf("failed to add IP address to interface: %w", err)
	}

	// Update our stored IP address
	if v6 {
		m.LastIPv6 = ip
	} else {
		m.LastIP = ip

		// Set up default route for IPv4
		if info.Gateway != "" {
			gateway := net.ParseIP(info.Gateway)
			if err := m.atomicRouteAdd(gateway); err != nil {
				return fmt.Errorf("failed to add default route: %w", err)
			}
		}
	}

	return nil
}

func (m *dhcpManager) renew(v6 bool, info dhcp.Info) error {
	lastIP := m.LastIP
	if v6 {
		lastIP = m.LastIPv6
	}

	ip, err := netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IP address: %w", err)
	}

	// Handle case where there was no initial IP (lastIP is nil)
	if lastIP == nil {
		log.
			WithFields(m.logFields(v6)).
			WithField("new_ip", ip).
			Info("DHCP renew with no previous IP, treating as initial bind")
		return m.handleBound(v6, info)
	}

	if !ip.Equal(*lastIP) {
		// TODO: We can't deal with a different renewed IP for the same reason as described for `bound`
		log.
			WithFields(m.logFields(v6)).
			WithField("old_ip", lastIP).
			WithField("new_ip", ip).
			Warn("DHCP renew with changed IP")
	}

	if !v6 && info.Gateway != "" {
		newGateway := net.ParseIP(info.Gateway)

		routes, err := m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
			LinkIndex: m.ctrLink.Attrs().Index,
			Dst:       nil,
		}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
		if err != nil {
			return fmt.Errorf("failed to list routes: %w", err)
		}

		if len(routes) == 0 {
			log.
				WithFields(m.logFields(v6)).
				WithField("gateway", newGateway).
				Info("DHCP renew adding default route")

			if err := m.netHandle.RouteAdd(&netlink.Route{
				LinkIndex: m.ctrLink.Attrs().Index,
				Gw:        newGateway,
			}); err != nil {
				return fmt.Errorf("failed to add default route: %w", err)
			}
		} else if !newGateway.Equal(routes[0].Gw) {
			log.
				WithFields(m.logFields(v6)).
				WithField("old_gateway", routes[0].Gw).
				WithField("new_gateway", newGateway).
				Info("DHCP renew replacing default route")

			routes[0].Gw = newGateway
			if err := m.netHandle.RouteReplace(&routes[0]); err != nil {
				return fmt.Errorf("failed to replace default route: %w", err)
			}
		}
	}

	return nil
}

// setupClientFromLease creates a DHCP client from an existing lease
func (m *dhcpManager) setupClientFromLease(leaseInfo *dhcp.LeaseInfo) (chan error, error) {
	v6 := leaseInfo.IsIPv6
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.WithFields(m.logFields(v6)).Info("Starting persistent DHCP client from transferred lease")

	options := &dhcp.DHCPClientOptions{
		Hostname:  m.hostname,
		V6:        v6,
		Namespace: m.nsPath,
	}

	// Create client from existing lease
	client, err := dhcp.NewDHCPClientFromLease(m.ctrLink.Attrs().Name, options, leaseInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%s client from lease: %w", v6Str, err)
	}

	events, err := client.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start transferred DHCP%s client: %w", v6Str, err)
	}

	errChan := make(chan error)
	go func() {
		defer close(errChan)
		
		for {
			select {
			case event, ok := <-events:
				if !ok {
					// Events channel closed, DHCP client stopped
					return
				}
				
				switch event.Type {
				case "bound":
					log.WithFields(m.logFields(v6)).WithField("ip", event.Data.IP).WithField("gateway", event.Data.Gateway).Info("Transferred DHCP client confirmed lease binding")
					// No need to handle bound since we already have the IP configured
				case "renew":
					log.WithFields(m.logFields(v6)).Debug("Transferred DHCP client lease renewal")

					if err := m.renew(v6, event.Data); err != nil {
						log.WithError(err).WithFields(m.logFields(v6)).WithField("gateway", event.Data.Gateway).WithField("new_ip", event.Data.IP).Error("Failed to execute IP renewal")
					}
				case "leasefail":
					log.WithFields(m.logFields(v6)).Warn("Transferred DHCP client failed to maintain lease")
				}

			case <-m.stopChan:
				log.WithFields(m.logFields(v6)).Info("Shutting down transferred DHCP client")

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				err := client.Finish(ctx)
				errChan <- err
				return
			}
		}
	}()

	return errChan, nil
}

func (m *dhcpManager) setupClient(v6 bool) (chan error, error) {
	v6Str := ""
	if v6 {
		v6Str = "v6"
	}

	log.
		WithFields(m.logFields(v6)).
		Info("Starting persistent native DHCP client")

	options := &dhcp.DHCPClientOptions{
		Hostname:  m.hostname,
		V6:        v6,
		Namespace: m.nsPath,
	}

	// If we have an existing IP from CreateEndpoint, request the same IP for renewal
	lastIP := m.LastIP
	if v6 {
		lastIP = m.LastIPv6
	}
	if lastIP != nil {
		// Extract just the IP address without the CIDR suffix
		ipStr := lastIP.IP.String()
		options.RequestIP = ipStr
		log.WithFields(log.Fields{
			"network":    m.joinReq.NetworkID[:12],
			"endpoint":   m.joinReq.EndpointID[:12],
			"sandbox":    m.joinReq.SandboxKey,
			"is_ipv6":    v6,
			"request_ip": ipStr,
		}).Info("Configuring persistent DHCP client to request existing IP")
	}

	client, err := dhcp.NewDHCPClient(m.ctrLink.Attrs().Name, options)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHCP%v client: %w", v6Str, err)
	}

	events, err := client.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start DHCP%v client: %w", v6Str, err)
	}

	errChan := make(chan error)
	go func() {
		defer close(errChan)

		for {
			select {
			case event, ok := <-events:
				if !ok {
					// Events channel closed, DHCP client stopped
					return
				}

				switch event.Type {
				case "bound":
					log.
						WithFields(m.logFields(v6)).
						WithField("ip", event.Data.IP).
						WithField("gateway", event.Data.Gateway).
						Info("native DHCP client bound to new IP address")

					// Handle initial IP assignment when no IP was previously acquired
					if err := m.handleBound(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("gateway", event.Data.Gateway).
							WithField("new_ip", event.Data.IP).
							Error("Failed to handle IP binding")
					}
				case "renew":
					log.
						WithFields(m.logFields(v6)).
						Debug("native DHCP client renew")

					if err := m.renew(v6, event.Data); err != nil {
						log.
							WithError(err).
							WithFields(m.logFields(v6)).
							WithField("gateway", event.Data.Gateway).
							WithField("new_ip", event.Data.IP).
							Error("Failed to execute IP renewal")
					}
				case "leasefail":
					log.WithFields(m.logFields(v6)).Warn("native DHCP client failed to get a lease")
				}

			case <-m.stopChan:
				log.
					WithFields(m.logFields(v6)).
					Info("Shutting down persistent native DHCP client")

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				err := client.Finish(ctx)
				errChan <- err
				return
			}
		}
	}()

	return errChan, nil
}

func (m *dhcpManager) Start(ctx context.Context) error {
	// Set up cleanup timer for resource management
	m.cleanupTimer = time.AfterFunc(30*time.Minute, func() {
		log.WithFields(m.logFields(false)).Warn("DHCP manager cleanup timer expired, forcing cleanup")
		m.forceCleanup()
	})
	defer func() {
		if m.cleanupTimer != nil {
			m.cleanupTimer.Stop()
		}
	}()

	var ctrID string
	if err := util.AwaitCondition(ctx, func() (bool, error) {
		dockerNet, err := m.docker.NetworkInspect(ctx, m.joinReq.NetworkID, network.InspectOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to get Docker network info: %w", err)
		}

		for id, info := range dockerNet.Containers {
			if info.EndpointID == m.joinReq.EndpointID {
				ctrID = id
				break
			}
		}
		if ctrID == "" {
			return false, util.ErrNoContainer
		}

		// Seems like Docker makes the container ID just the endpoint until it's ready
		return !strings.HasPrefix(ctrID, "ep-"), nil
	}, pollTime); err != nil {
		return err
	}

	ctr, err := util.AwaitContainerInspect(ctx, m.docker, ctrID, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get Docker container info: %w", err)
	}

	// Using the "sandbox key" directly causes issues on some platforms
	m.nsPath = fmt.Sprintf("/proc/%v/ns/net", ctr.State.Pid)
	m.hostname = ctr.Config.Hostname

	// Enhanced namespace handling with retries and validation
	m.nsHandle, err = m.safeAwaitNetNS(ctx, m.nsPath, pollTime)
	if err != nil {
		return fmt.Errorf("failed to get sandbox network namespace: %w", err)
	}

	m.netHandle, err = m.safeCreateNetHandle(m.nsHandle)
	if err != nil {
		m.safeCloseNSHandle()
		return fmt.Errorf("failed to open netlink handle in sandbox namespace: %w", err)
	}

	if err := func() error {
		hostName, oldCtrName := vethPairNames(m.joinReq.EndpointID)
		hostLink, err := netlink.LinkByName(hostName)
		if err != nil {
			return fmt.Errorf("failed to find host side of veth pair: %w", err)
		}
		hostVeth, ok := hostLink.(*netlink.Veth)
		if !ok {
			return util.ErrNotVEth
		}

		ctrIndex, err := netlink.VethPeerIndex(hostVeth)
		if err != nil {
			return fmt.Errorf("failed to get container side of veth's index: %w", err)
		}

		if err := util.AwaitCondition(ctx, func() (bool, error) {
			m.ctrLink, err = util.AwaitLinkByIndex(ctx, m.netHandle, ctrIndex, pollTime)
			if err != nil {
				return false, fmt.Errorf("failed to get link for container side of veth pair: %w", err)
			}

			return m.ctrLink.Attrs().Name != oldCtrName, nil
		}, pollTime); err != nil {
			return err
		}

		// Restore the original MAC address to ensure consistent DHCP requests
		currentMAC := m.ctrLink.Attrs().HardwareAddr
		log.WithFields(log.Fields{
			"network":     m.joinReq.NetworkID[:12],
			"endpoint":    m.joinReq.EndpointID[:12],
			"sandbox":     m.joinReq.SandboxKey,
			"current_mac": currentMAC.String(),
			"original_mac": func() string {
				if m.OriginalMAC != nil {
					return m.OriginalMAC.String()
				} else {
					return "<nil>"
				}
			}(),
		}).Info("MAC address check before persistent DHCP client")

		if m.OriginalMAC != nil {
			if !bytes.Equal(currentMAC, m.OriginalMAC) {
				log.WithFields(log.Fields{
					"network":      m.joinReq.NetworkID[:12],
					"endpoint":     m.joinReq.EndpointID[:12],
					"sandbox":      m.joinReq.SandboxKey,
					"current_mac":  currentMAC.String(),
					"original_mac": m.OriginalMAC.String(),
				}).Info("Restoring original MAC address for consistent DHCP requests")

				if err := m.netHandle.LinkSetHardwareAddr(m.ctrLink, m.OriginalMAC); err != nil {
					return fmt.Errorf("failed to restore original MAC address: %w", err)
				}

				// Verify the MAC was actually set and refresh the link handle
				if linkRefresh, err := m.netHandle.LinkByIndex(m.ctrLink.Attrs().Index); err == nil {
					m.ctrLink = linkRefresh
				}

				verifyMAC := m.ctrLink.Attrs().HardwareAddr
				log.WithFields(log.Fields{
					"network":    m.joinReq.NetworkID[:12],
					"endpoint":   m.joinReq.EndpointID[:12],
					"sandbox":    m.joinReq.SandboxKey,
					"verify_mac": verifyMAC.String(),
				}).Info("MAC address after restoration")

				// Brief delay to ensure network stack processes MAC change
				time.Sleep(100 * time.Millisecond)
			} else {
				log.WithFields(log.Fields{
					"network":  m.joinReq.NetworkID[:12],
					"endpoint": m.joinReq.EndpointID[:12],
					"sandbox":  m.joinReq.SandboxKey,
					"mac":      currentMAC.String(),
				}).Info("MAC address already matches, no restoration needed")
			}
		} else {
			log.WithFields(log.Fields{
				"network":  m.joinReq.NetworkID[:12],
				"endpoint": m.joinReq.EndpointID[:12],
				"sandbox":  m.joinReq.SandboxKey,
			}).Warn("OriginalMAC is nil, cannot restore MAC address")
		}

		// Check if we have a transferred lease to use
		if m.leaseClient != nil {
			// Transfer the lease from initial client
			leaseInfo, err := m.leaseClient.TransferLease()
			if err != nil {
				log.WithFields(m.logFields(false)).WithError(err).Warn("Failed to transfer lease, falling back to new DHCP request")
				// Fallback to normal setup
				if m.errChan, err = m.setupClient(false); err != nil {
					close(m.stopChan)
					return err
				}
			} else {
				// Use transferred lease
				if m.errChan, err = m.setupClientFromLease(leaseInfo); err != nil {
					close(m.stopChan)
					return err
				}
			}

			// Clean up the initial client
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if finishErr := m.leaseClient.Finish(ctx); finishErr != nil {
				log.WithFields(m.logFields(false)).WithError(finishErr).Warn("Failed to cleanly finish initial DHCP client")
			}
			cancel()
			m.leaseClient = nil
		} else {
			// No transferred lease, use normal setup
			if m.errChan, err = m.setupClient(false); err != nil {
				close(m.stopChan)
				return err
			}
		}

		if m.opts.IPv6 {
			if m.errChanV6, err = m.setupClient(true); err != nil {
				close(m.stopChan)
				return err
			}
		}

		return nil
	}(); err != nil {
		m.safeCleanupResources()
		return err
	}

	return nil
}

func (m *dhcpManager) Stop() error {
	defer m.safeCleanupResources()

	// Stop cleanup timer
	if m.cleanupTimer != nil {
		m.cleanupTimer.Stop()
	}

	// Safely signal stop
	m.safeStop()

	// Wait for DHCP clients to shut down with timeout
	if err := m.awaitClientShutdown(10 * time.Second); err != nil {
		return fmt.Errorf("failed to shut down DHCP clients: %w", err)
	}

	return nil
}

// checkIPConflict verifies that the IP address is not already in use
func (m *dhcpManager) checkIPConflict(addr *netlink.Addr, v6 bool) error {
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}

	// Get all addresses on the interface
	existingAddrs, err := m.netHandle.AddrList(m.ctrLink, family)
	if err != nil {
		return fmt.Errorf("failed to list existing addresses: %w", err)
	}

	// Check for exact matches
	for _, existing := range existingAddrs {
		if existing.Equal(*addr) {
			log.WithFields(m.logFields(v6)).WithField("ip", addr.IP.String()).Debug("IP address already assigned, skipping")
			return nil // Not an error if it's already our address
		}
		if existing.IP.Equal(addr.IP) {
			return fmt.Errorf("IP %s already assigned with different prefix", addr.IP.String())
		}
	}

	// Check if IP is reachable on the network (ARP/NDP check)
	if err := m.checkIPReachability(addr.IP); err != nil {
		return fmt.Errorf("IP conflict check failed: %w", err)
	}

	return nil
}

// checkIPReachability performs basic reachability check to detect IP conflicts
func (m *dhcpManager) checkIPReachability(ip net.IP) error {
	// For IPv4, we could implement ARP check
	// For IPv6, we could implement NDP check
	// For now, we'll do a simple ping test with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1", ip.String())
	if err := cmd.Run(); err == nil {
		return fmt.Errorf("IP %s is already in use (ping successful)", ip.String())
	}

	return nil
}

// atomicAddrAdd adds an IP address with conflict resolution
func (m *dhcpManager) atomicAddrAdd(addr *netlink.Addr) error {
	// First, try to add the address
	if err := m.netHandle.AddrAdd(m.ctrLink, addr); err != nil {
		// Check if it's just a duplicate address
		if strings.Contains(err.Error(), "file exists") ||
			strings.Contains(err.Error(), "cannot assign") {
			// Verify it's actually our address
			existingAddrs, listErr := m.netHandle.AddrList(m.ctrLink, unix.AF_INET)
			if listErr != nil {
				return fmt.Errorf("failed to verify existing address: %w", listErr)
			}

			for _, existing := range existingAddrs {
				if existing.Equal(*addr) {
					log.WithFields(m.logFields(false)).WithField("ip", addr.IP.String()).Debug("Address already exists and matches")
					return nil
				}
			}
			return fmt.Errorf("address conflict: %w", err)
		}
		return fmt.Errorf("failed to add address: %w", err)
	}

	return nil
}

// atomicRouteAdd adds a route with conflict resolution
func (m *dhcpManager) atomicRouteAdd(gateway net.IP) error {
	route := &netlink.Route{
		LinkIndex: m.ctrLink.Attrs().Index,
		Gw:        gateway,
	}

	if err := m.netHandle.RouteAdd(route); err != nil {
		if strings.Contains(err.Error(), "file exists") {
			// Check if existing route matches
			existingRoutes, listErr := m.netHandle.RouteListFiltered(unix.AF_INET, &netlink.Route{
				LinkIndex: m.ctrLink.Attrs().Index,
				Dst:       nil,
			}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_DST)
			if listErr != nil {
				return fmt.Errorf("failed to verify existing route: %w", listErr)
			}

			for _, existing := range existingRoutes {
				if existing.Gw != nil && existing.Gw.Equal(gateway) {
					log.WithFields(m.logFields(false)).WithField("gateway", gateway.String()).Debug("Route already exists and matches")
					return nil
				}
			}

			// Replace existing default route
			if len(existingRoutes) > 0 {
				existingRoutes[0].Gw = gateway
				if err := m.netHandle.RouteReplace(&existingRoutes[0]); err != nil {
					return fmt.Errorf("failed to replace existing route: %w", err)
				}
				return nil
			}
		}
		return fmt.Errorf("failed to add route: %w", err)
	}

	return nil
}

// forceCleanup performs emergency cleanup of resources
func (m *dhcpManager) forceCleanup() {
	log.WithFields(m.logFields(false)).Info("Performing forced cleanup of DHCP manager resources")

	if m.netHandle != nil {
		m.netHandle.Delete()
		m.netHandle = nil
	}

	if m.nsHandle.IsOpen() {
		m.nsHandle.Close()
	}

	// Send stop signal if channels are still open
	select {
	case <-m.stopChan:
		// Already stopped
	default:
		close(m.stopChan)
	}
}

// safeAwaitNetNS safely waits for network namespace with enhanced error handling
func (m *dhcpManager) safeAwaitNetNS(ctx context.Context, nsPath string, pollInterval time.Duration) (netns.NsHandle, error) {
	retryCount := 0
	maxRetries := 10

	return util.AwaitNetNSWithRetry(ctx, nsPath, pollInterval, func(err error) bool {
		retryCount++

		log.WithFields(m.logFields(false)).WithFields(log.Fields{
			"retry":   retryCount,
			"ns_path": nsPath,
			"error":   err.Error(),
		}).Debug("Network namespace access attempt failed")

		// Continue retrying for certain errors
		if retryCount >= maxRetries {
			return false
		}

		// Retry on common transient errors
		if strings.Contains(err.Error(), "no such file") ||
			strings.Contains(err.Error(), "permission denied") ||
			strings.Contains(err.Error(), "resource temporarily unavailable") {
			time.Sleep(time.Duration(retryCount) * 100 * time.Millisecond)
			return true
		}

		return false
	})
}

// safeCreateNetHandle safely creates a netlink handle with error handling
func (m *dhcpManager) safeCreateNetHandle(nsHandle netns.NsHandle) (*netlink.Handle, error) {
	handle, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return nil, fmt.Errorf("failed to create netlink handle: %w", err)
	}

	// Validate the handle by attempting a simple operation
	if _, err := handle.LinkList(); err != nil {
		handle.Delete()
		return nil, fmt.Errorf("netlink handle validation failed: %w", err)
	}

	return handle, nil
}

// safeCloseNSHandle safely closes namespace handle
func (m *dhcpManager) safeCloseNSHandle() {
	if m.nsHandle.IsOpen() {
		if err := m.nsHandle.Close(); err != nil {
			log.WithFields(m.logFields(false)).WithError(err).Warn("Failed to close namespace handle")
		}
	}
}

// safeCleanupResources safely cleans up all resources
func (m *dhcpManager) safeCleanupResources() {
	if m.netHandle != nil {
		m.netHandle.Delete()
		m.netHandle = nil
	}
	m.safeCloseNSHandle()
}

// safeStop safely signals stop to goroutines
func (m *dhcpManager) safeStop() {
	select {
	case <-m.stopChan:
		// Already stopped
	default:
		close(m.stopChan)
	}
}

// awaitClientShutdown waits for DHCP clients to shut down with timeout
func (m *dhcpManager) awaitClientShutdown(timeout time.Duration) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Wait for IPv4 client
	if m.errChan != nil {
		select {
		case err := <-m.errChan:
			if err != nil {
				log.WithFields(m.logFields(false)).WithError(err).Warn("IPv4 DHCP client shutdown error")
			}
		case <-shutdownCtx.Done():
			return fmt.Errorf("timeout waiting for IPv4 DHCP client shutdown")
		}
	}

	// Wait for IPv6 client if enabled
	if m.opts.IPv6 && m.errChanV6 != nil {
		select {
		case err := <-m.errChanV6:
			if err != nil {
				log.WithFields(m.logFields(true)).WithError(err).Warn("IPv6 DHCP client shutdown error")
			}
		case <-shutdownCtx.Done():
			return fmt.Errorf("timeout waiting for IPv6 DHCP client shutdown")
		}
	}

	return nil
}
