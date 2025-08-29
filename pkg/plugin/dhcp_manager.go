package plugin

import (
	"context"
	"fmt"
	"net"
	"time"

	docker "github.com/docker/docker/client"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/asheliahut/docker-net-dhcp/pkg/dhcp"
)

const pollTime = 100 * time.Millisecond

// UnifiedDHCPManager handles all network interface creation and DHCP management
type UnifiedDHCPManager struct {
	docker       *docker.Client
	networkID    string
	endpointID   string
	opts         DHCPNetworkOptions
	hostname     string
	requiredMAC  net.HardwareAddr // Optional required MAC address
	awaitTimeout time.Duration

	// Network interface state
	hostVethName string
	ctrVethName  string
	hostLink     netlink.Link
	ctrLink      netlink.Link
	bridgeLink   netlink.Link
	finalMAC     net.HardwareAddr

	// IP addresses
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	gateway string

	// Single persistent DHCP clients (make two requests: initial + namespace)
	dhcpClient   *dhcp.DHCPClient
	dhcpClientV6 *dhcp.DHCPClient

	// Compatibility fields for plugin.go
	joinReq JoinRequest // For compatibility with plugin lifecycle methods

	// Lifecycle management
	stopChan     chan struct{}
	errChan      chan error
	errChanV6    chan error
	cleanupTimer *time.Timer
	isStarted    bool
}

// NewUnifiedDHCPManager creates a new unified manager for network interface and DHCP management
func NewUnifiedDHCPManager(docker *docker.Client, networkID, endpointID string, opts DHCPNetworkOptions, hostname string, requiredMAC net.HardwareAddr, awaitTimeout time.Duration) *UnifiedDHCPManager {
	hostName, ctrName := vethPairNames(endpointID)
	return &UnifiedDHCPManager{
		docker:       docker,
		networkID:    networkID,
		endpointID:   endpointID,
		opts:         opts,
		hostname:     hostname,
		requiredMAC:  requiredMAC,
		awaitTimeout: awaitTimeout,
		hostVethName: hostName,
		ctrVethName:  ctrName,
		// Initialize joinReq for compatibility
		joinReq:  JoinRequest{NetworkID: networkID, EndpointID: endpointID},
		stopChan: make(chan struct{}),
	}
}

func (m *UnifiedDHCPManager) logFields(v6 bool) log.Fields {
	return log.Fields{
		"network":  m.networkID[:12],
		"endpoint": m.endpointID[:12],
		"is_ipv6":  v6,
	}
}

// CreateNetworkInterface creates the veth pair and sets up networking
func (m *UnifiedDHCPManager) CreateNetworkInterface(ctx context.Context) error {
	// Get bridge interface
	bridgeLink, err := netlink.LinkByName(m.opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to get bridge interface: %w", err)
	}
	m.bridgeLink = bridgeLink

	// Create veth pair
	la := netlink.NewLinkAttrs()
	la.Name = m.hostVethName
	newVeth := &netlink.Veth{
		LinkAttrs: la,
		PeerName:  m.ctrVethName,
	}

	// Set MAC address if required
	if m.requiredMAC != nil {
		newVeth.PeerHardwareAddr = m.requiredMAC
	}

	// Create the veth pair
	if err := netlink.LinkAdd(newVeth); err != nil {
		return fmt.Errorf("failed to create veth pair: %w", err)
	}

	// Get the created links
	m.hostLink, err = netlink.LinkByName(m.hostVethName)
	if err != nil {
		return fmt.Errorf("failed to find host side of veth pair: %w", err)
	}

	m.ctrLink, err = netlink.LinkByName(m.ctrVethName)
	if err != nil {
		return fmt.Errorf("failed to find container side of veth pair: %w", err)
	}

	// Attach host side to bridge
	if err := netlink.LinkSetMaster(m.hostLink, m.bridgeLink); err != nil {
		return fmt.Errorf("failed to attach host side to bridge: %w", err)
	}

	// Bring up both sides
	if err := netlink.LinkSetUp(m.hostLink); err != nil {
		return fmt.Errorf("failed to bring up host side: %w", err)
	}
	if err := netlink.LinkSetUp(m.ctrLink); err != nil {
		return fmt.Errorf("failed to bring up container side: %w", err)
	}

	// Refresh container link to get current MAC
	m.ctrLink, err = netlink.LinkByIndex(m.ctrLink.Attrs().Index)
	if err != nil {
		return fmt.Errorf("failed to refresh container link: %w", err)
	}

	// Determine final MAC address
	if m.requiredMAC != nil {
		m.finalMAC = m.requiredMAC
		log.WithFields(m.logFields(false)).WithField("mac", m.finalMAC.String()).Debug("Using requested MAC address")
	} else {
		m.finalMAC = m.ctrLink.Attrs().HardwareAddr
		log.WithFields(m.logFields(false)).WithField("mac", m.finalMAC.String()).Debug("Using auto-assigned MAC address")
	}

	// Ensure MAC is set correctly
	if err := netlink.LinkSetHardwareAddr(m.ctrLink, m.finalMAC); err != nil {
		return fmt.Errorf("failed to set final MAC address: %w", err)
	}

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"host_veth": m.hostVethName,
		"ctr_veth":  m.ctrVethName,
		"bridge":    m.opts.Bridge,
		"mac":       m.finalMAC.String(),
	}).Info("Network interface created successfully")

	return nil
}

// AcquireInitialLeases makes first DHCP request with hostname (stage 1 - host namespace)
func (m *UnifiedDHCPManager) AcquireInitialLeases(ctx context.Context, hostname string) error {
	// First DHCP request: get IP + hostname in host namespace (no container namespace yet)
	log.WithFields(m.logFields(false)).WithField("hostname", hostname).Info("Making first DHCP request with hostname (host namespace)")
	
	info, err := dhcp.GetIP(ctx, m.ctrVethName, &dhcp.DHCPClientOptions{
		Hostname: hostname, // Include hostname in first request
		V6:       false,
		MACAddr:  m.finalMAC,
	})
	if err != nil {
		return fmt.Errorf("failed to acquire initial IPv4 DHCP lease with hostname: %w", err)
	}

	// Parse and store IPv4 info from first request
	m.IPv4, err = netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IPv4 address: %w", err)
	}
	m.gateway = info.Gateway

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"ip":       info.IP,
		"gateway":  info.Gateway,
		"domain":   info.Domain,
		"hostname": hostname,
	}).Info("First DHCP request completed - IP acquired with hostname")

	// Handle IPv6 if enabled
	if m.opts.IPv6 {
		log.WithFields(m.logFields(true)).WithField("hostname", hostname).Info("Making first IPv6 DHCP request with hostname")
		
		infoV6, err := dhcp.GetIP(ctx, m.ctrVethName, &dhcp.DHCPClientOptions{
			Hostname: hostname,
			V6:       true,
			MACAddr:  m.finalMAC,
		})
		if err != nil {
			log.WithError(err).WithFields(m.logFields(true)).Warn("Failed to acquire initial IPv6 lease, continuing without IPv6")
		} else {
			m.IPv6, err = netlink.ParseAddr(infoV6.IP)
			if err != nil {
				log.WithError(err).WithFields(m.logFields(true)).Warn("Failed to parse IPv6 address")
			} else {
				log.WithFields(m.logFields(true)).WithFields(log.Fields{
					"ipv6":     infoV6.IP,
					"hostname": hostname,
				}).Info("First IPv6 DHCP request completed - IPv6 acquired with hostname")
			}
		}
	}

	return nil
}

// RegisterHostname performs the second-stage DHCP request to register hostname with acquired IP
func (m *UnifiedDHCPManager) RegisterHostname(ctx context.Context, nsPath string, hostname string) error {
	if hostname == "" {
		log.WithFields(m.logFields(false)).Debug("No hostname to register, skipping hostname registration")
		return nil
	}

	if m.IPv4 == nil {
		log.WithFields(m.logFields(false)).Warn("No IPv4 address available for hostname registration")
		return nil
	}

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"hostname":  hostname,
		"ip":        m.IPv4.IP.String(),
		"interface": m.ctrVethName,
		"namespace": nsPath,
	}).Info("Registering hostname with DHCP server using acquired IP")

	// Use the dedicated hostname registration function
	if err := dhcp.RegisterHostnameWithNamespace(ctx, m.ctrVethName, m.IPv4.IP.String(), hostname, nsPath); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"hostname":  hostname,
			"ip":        m.IPv4.IP.String(),
			"interface": m.ctrVethName,
		}).Warn("Failed to register hostname with DHCP server")
		// Don't treat this as a fatal error - the container can still work without hostname registration
		return nil
	}

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"hostname": hostname,
		"ip":       m.IPv4.IP.String(),
	}).Info("Hostname successfully registered with DHCP server")

	return nil
}

// GetInterfaceInfo returns the interface information for Docker
func (m *UnifiedDHCPManager) GetInterfaceInfo() (string, string, string, string) {
	var ipv4, ipv6 string
	if m.IPv4 != nil {
		ipv4 = m.IPv4.String()
	}
	if m.IPv6 != nil {
		ipv6 = m.IPv6.String()
	}
	return ipv4, ipv6, m.finalMAC.String(), m.gateway
}

// Stop stops all DHCP clients and cleans up resources
func (m *UnifiedDHCPManager) Stop() error {
	if !m.isStarted {
		return nil
	}

	// Stop cleanup timer
	if m.cleanupTimer != nil {
		m.cleanupTimer.Stop()
	}

	// Signal all clients to stop
	close(m.stopChan)

	// Wait for clients to shut down
	if m.errChan != nil {
		select {
		case err := <-m.errChan:
			if err != nil {
				log.WithError(err).WithFields(m.logFields(false)).Warn("IPv4 DHCP client shutdown error")
			}
		case <-time.After(10 * time.Second):
			log.WithFields(m.logFields(false)).Warn("Timeout waiting for IPv4 DHCP client shutdown")
		}
	}

	if m.errChanV6 != nil {
		select {
		case err := <-m.errChanV6:
			if err != nil {
				log.WithError(err).WithFields(m.logFields(true)).Warn("IPv6 DHCP client shutdown error")
			}
		case <-time.After(10 * time.Second):
			log.WithFields(m.logFields(true)).Warn("Timeout waiting for IPv6 DHCP client shutdown")
		}
	}

	log.WithFields(log.Fields{
		"network":  m.networkID[:12],
		"endpoint": m.endpointID[:12],
	}).Info("DHCP manager stopped")

	return nil
}

// Cleanup destroys the veth pair and cleans up all resources
func (m *UnifiedDHCPManager) Cleanup() error {
	// First stop DHCP clients
	if err := m.Stop(); err != nil {
		log.WithError(err).Warn("Error stopping DHCP clients during cleanup")
	}

	// Delete veth pair (deleting host side removes both)
	if m.hostLink != nil {
		if err := netlink.LinkDel(m.hostLink); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":   m.networkID[:12],
				"endpoint":  m.endpointID[:12],
				"host_veth": m.hostVethName,
			}).Warn("Failed to delete veth pair")
			return fmt.Errorf("failed to delete veth pair: %w", err)
		}
	}

	log.WithFields(log.Fields{
		"network":  m.networkID[:12],
		"endpoint": m.endpointID[:12],
	}).Info("Network interface cleaned up")

	return nil
}

// StartPersistentClients makes second DHCP request with hostname in container namespace (stage 2)
func (m *UnifiedDHCPManager) StartPersistentClients(ctx context.Context, sandboxKey string, hostname string) error {
	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"sandbox":  sandboxKey,
		"hostname": hostname,
	}).Info("Making second DHCP request with hostname (container namespace)")
	
	// Convert sandbox key to namespace path
	var nsPath string
	if sandboxKey != "" {
		nsPath = sandboxKey
	}
	
	// Second DHCP request: register hostname with container namespace information
	if err := m.RegisterHostname(ctx, nsPath, hostname); err != nil {
		log.WithError(err).WithFields(m.logFields(false)).Warn("Failed to make second DHCP request with hostname, continuing without it")
	}
	
	// Now start persistent clients for lease maintenance
	if m.IPv4 != nil {
		log.WithFields(m.logFields(false)).WithField("hostname", hostname).Info("Starting persistent IPv4 client for lease maintenance")
		
		client, err := dhcp.NewDHCPClient(m.ctrVethName, &dhcp.DHCPClientOptions{
			Hostname:  hostname,
			V6:        false,
			Namespace: nsPath,
			MACAddr:   m.finalMAC,
			RequestIP: m.IPv4.IP.String(), // Use same IP from first request
		})
		if err != nil {
			log.WithError(err).WithFields(m.logFields(false)).Error("Failed to create persistent IPv4 client")
		} else {
			events, err := client.Start()
			if err != nil {
				log.WithError(err).WithFields(m.logFields(false)).Error("Failed to start persistent IPv4 client")
				client.Finish(ctx)
			} else {
				m.dhcpClient = client
				m.errChan = make(chan error)
				go m.monitorDHCPEvents(events, false, m.errChan)
				log.WithFields(m.logFields(false)).WithFields(log.Fields{
					"hostname": hostname,
					"ip":       m.IPv4.IP.String(),
				}).Info("Persistent IPv4 client started for lease maintenance")
			}
		}
	}
	
	// Handle IPv6 persistent client if available
	if m.IPv6 != nil {
		log.WithFields(m.logFields(true)).WithField("hostname", hostname).Info("Starting persistent IPv6 client for lease maintenance")
		
		clientV6, err := dhcp.NewDHCPClient(m.ctrVethName, &dhcp.DHCPClientOptions{
			Hostname:  hostname,
			V6:        true,
			Namespace: nsPath,
			MACAddr:   m.finalMAC,
			RequestIP: m.IPv6.IP.String(), // Use same IP from first request
		})
		if err != nil {
			log.WithError(err).WithFields(m.logFields(true)).Error("Failed to create persistent IPv6 client")
		} else {
			eventsV6, err := clientV6.Start()
			if err != nil {
				log.WithError(err).WithFields(m.logFields(true)).Error("Failed to start persistent IPv6 client")
				clientV6.Finish(ctx)
			} else {
				m.dhcpClientV6 = clientV6
				m.errChanV6 = make(chan error)
				go m.monitorDHCPEvents(eventsV6, true, m.errChanV6)
				log.WithFields(m.logFields(true)).WithFields(log.Fields{
					"hostname": hostname,
					"ipv6":     m.IPv6.IP.String(),
				}).Info("Persistent IPv6 client started for lease maintenance")
			}
		}
	}
	
	m.isStarted = true
	log.WithFields(m.logFields(false)).Info("Second DHCP request completed and persistent clients started")
	return nil
}

// monitorDHCPEvents monitors events from persistent DHCP clients
func (m *UnifiedDHCPManager) monitorDHCPEvents(events <-chan dhcp.Event, isIPv6 bool, errChan chan error) {
	defer close(errChan)
	
	for {
		select {
		case event, ok := <-events:
			if !ok {
				// Events channel closed, DHCP client stopped
				log.WithFields(m.logFields(isIPv6)).Debug("DHCP client events channel closed")
				return
			}
			
			// Handle DHCP events (renewal, etc.)
			switch event.Type {
			case "bound":
				log.WithFields(m.logFields(isIPv6)).WithField("ip", event.Data.IP).Debug("DHCP client lease bound")
			case "renew":
				log.WithFields(m.logFields(isIPv6)).WithField("ip", event.Data.IP).Debug("DHCP client lease renewed")
			case "leasefail":
				log.WithFields(m.logFields(isIPv6)).Warn("DHCP client lease failed")
			}
			
		case <-m.stopChan:
			log.WithFields(m.logFields(isIPv6)).Info("Stopping DHCP client event monitoring")
			return
		}
	}
}

// forceCleanup performs emergency cleanup
func (m *UnifiedDHCPManager) forceCleanup() {
	log.WithFields(m.logFields(false)).Info("Performing forced cleanup of DHCP manager resources")

	// Try to delete veth pair
	if m.hostLink != nil {
		netlink.LinkDel(m.hostLink)
	}

	// Send stop signal if channels are still open
	select {
	case <-m.stopChan:
		// Already stopped
	default:
		close(m.stopChan)
	}
}
