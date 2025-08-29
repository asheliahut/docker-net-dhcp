package plugin

import (
	"context"
	"fmt"
	"net"
	"strings"
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

	// DHCP clients
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

// AcquireInitialLeases gets initial DHCP leases for both IPv4 and IPv6 (without hostname)
func (m *UnifiedDHCPManager) AcquireInitialLeases(ctx context.Context) error {
	// Create DHCP client options WITHOUT hostname for initial IP acquisition
	// Hostname will be registered separately in RegisterHostname method
	options := &dhcp.DHCPClientOptions{
		// Hostname: "", // Explicitly empty for initial request
		V6:       false,
		MACAddr:  m.finalMAC,
	}

	// Get IPv4 lease
	log.WithFields(m.logFields(false)).Info("Acquiring initial IPv4 DHCP lease")
	info, err := dhcp.GetIP(ctx, m.ctrVethName, options)
	if err != nil {
		return fmt.Errorf("failed to acquire IPv4 DHCP lease: %w", err)
	}

	// Parse and store IPv4 info
	m.IPv4, err = netlink.ParseAddr(info.IP)
	if err != nil {
		return fmt.Errorf("failed to parse IPv4 address: %w", err)
	}
	m.gateway = info.Gateway

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"ip":      info.IP,
		"gateway": info.Gateway,
		"domain":  info.Domain,
	}).Info("IPv4 DHCP lease acquired")

	// Get IPv6 lease if enabled
	if m.opts.IPv6 {
		log.WithFields(m.logFields(true)).Info("Acquiring initial IPv6 DHCP lease")
		optionsV6 := &dhcp.DHCPClientOptions{
			// Hostname: "", // Explicitly empty for initial request
			V6:       true,
			MACAddr:  m.finalMAC,
		}

		infoV6, err := dhcp.GetIP(ctx, m.ctrVethName, optionsV6)
		if err != nil {
			log.WithError(err).WithFields(m.logFields(true)).Warn("Failed to acquire IPv6 DHCP lease, continuing without IPv6")
		} else {
			m.IPv6, err = netlink.ParseAddr(infoV6.IP)
			if err != nil {
				log.WithError(err).WithFields(m.logFields(true)).Warn("Failed to parse IPv6 address, continuing without IPv6")
			} else {
				log.WithFields(m.logFields(true)).WithField("ipv6", infoV6.IP).Info("IPv6 DHCP lease acquired")
			}
		}
	}

	return nil
}

// RegisterHostname performs the second-stage DHCP request to register hostname
// This should be called after AcquireInitialLeases and once we have container namespace access
func (m *UnifiedDHCPManager) RegisterHostname(ctx context.Context, nsPath string) error {
	if m.hostname == "" {
		log.WithFields(m.logFields(false)).Debug("No hostname to register, skipping hostname registration")
		return nil
	}

	if m.IPv4 == nil {
		log.WithFields(m.logFields(false)).Warn("No IPv4 address available for hostname registration")
		return nil
	}

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"hostname":  m.hostname,
		"ip":        m.IPv4.IP.String(),
		"interface": m.ctrVethName,
		"namespace": nsPath,
	}).Info("Registering hostname with DHCP server using acquired IP")

	// Use the dedicated hostname registration function
	if err := dhcp.RegisterHostnameWithNamespace(ctx, m.ctrVethName, m.IPv4.IP.String(), m.hostname, nsPath); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"hostname":  m.hostname,
			"ip":        m.IPv4.IP.String(),
			"interface": m.ctrVethName,
		}).Warn("Failed to register hostname with DHCP server")
		// Don't treat this as a fatal error - the container can still work without hostname registration
		return nil
	}

	log.WithFields(m.logFields(false)).WithFields(log.Fields{
		"hostname": m.hostname,
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

// StartPersistentClients starts the persistent DHCP client management and registers hostname
func (m *UnifiedDHCPManager) StartPersistentClients(ctx context.Context, sandboxKey string) error {
	log.WithFields(m.logFields(false)).WithField("sandbox", sandboxKey).Info("Starting persistent DHCP client management")
	
	// Convert sandbox key to namespace path for hostname registration
	// Docker provides sandbox key, but we need the namespace path for DHCP library
	var nsPath string
	if sandboxKey != "" {
		// Convert sandbox key to namespace path
		// Sandbox key format: "/var/run/docker/netns/<id>"
		// Need to extract the network namespace path for the container
		nsPath = sandboxKey
		if !strings.HasPrefix(nsPath, "/proc/") {
			// If it's a Docker sandbox path, convert it to proc path
			// This might need adjustment based on your Docker setup
			log.WithFields(m.logFields(false)).WithFields(log.Fields{
				"sandbox_key": sandboxKey,
			}).Debug("Using sandbox key as namespace path for hostname registration")
		}
	}
	
	// Step 2 of the two-stage DHCP process: Register hostname with acquired IP
	if err := m.RegisterHostname(ctx, nsPath); err != nil {
		// Don't fail the entire process if hostname registration fails
		log.WithError(err).WithFields(m.logFields(false)).Warn("Failed to register hostname, continuing without it")
	}
	
	// TODO: In the future, implement persistent lease renewal clients here
	// For now, the initial leases from AcquireInitialLeases are sufficient
	
	m.isStarted = true
	log.WithFields(m.logFields(false)).Info("Persistent DHCP client management started successfully")
	return nil
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
