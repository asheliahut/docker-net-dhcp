package dhcp

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/nclient6"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	"github.com/asheliahut/docker-net-dhcp/pkg/util"
)

// Info contains DHCP lease information
type Info struct {
	IP      string
	Gateway string
	Domain  string
}

// DHCPClientOptions contains options for the DHCP client
type DHCPClientOptions struct {
	Hostname  string
	V6        bool // Currently unsupported, kept for compatibility
	Namespace string
	RequestIP string
	LeaseTime time.Duration    // Optional lease time preference
	MACAddr   net.HardwareAddr // Optional MAC address for client identification
}

// DHCPClient represents a native Go DHCP client
type DHCPClient struct {
	opts      *DHCPClientOptions
	ifaceName string
	client4   *nclient4.Client
	client6   *nclient6.Client
	lease4    *nclient4.Lease
	lease6    *dhcpv6.Message
	stopChan  chan struct{}

	// Lease handoff support
	isTransferred bool
	transferMutex sync.RWMutex
}

// Event represents a DHCP event
type Event struct {
	Type string
	Data Info
}

// NewDHCPClient creates a new native Go DHCP client
func NewDHCPClient(iface string, opts *DHCPClientOptions) (*DHCPClient, error) {
	log.WithFields(log.Fields{
		"interface":  iface,
		"hostname":   opts.Hostname,
		"namespace":  opts.Namespace,
		"request_ip": opts.RequestIP,
		"ipv6":       opts.V6,
	}).Trace("creating new native DHCP client")

	return &DHCPClient{
		opts:      opts,
		ifaceName: iface,
		stopChan:  make(chan struct{}),
	}, nil
}

// Start starts the DHCP client and returns an event channel
func (c *DHCPClient) Start() (chan Event, error) {
	events := make(chan Event, 10)

	// Start the DHCP client goroutine
	go c.runDHCPClient(events)

	return events, nil
}

// runDHCPClient runs the DHCP client in a separate goroutine with proper namespace handling
func (c *DHCPClient) runDHCPClient(events chan Event) {
	defer close(events)

	// Handle network namespace switching if needed
	if c.opts.Namespace != "" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			log.WithError(err).Error("Failed to get current namespace")
			return
		}
		defer origNS.Close()

		ns, err := netns.GetFromPath(c.opts.Namespace)
		if err != nil {
			log.WithError(err).WithField("namespace", c.opts.Namespace).Error("Failed to open network namespace")
			return
		}
		defer ns.Close()

		if err := netns.Set(ns); err != nil {
			log.WithError(err).Error("Failed to enter network namespace")
			return
		}
		defer netns.Set(origNS)
	}

	if c.opts.V6 {
		c.runDHCPv6Client(events)
	} else {
		c.runDHCPv4Client(events)
	}
}

// runDHCPv4Client runs the DHCPv4 client
func (c *DHCPClient) runDHCPv4Client(events chan Event) {
	// Create the native DHCP client
	client, err := nclient4.New(c.ifaceName)
	if err != nil {
		log.WithError(err).WithField("interface", c.ifaceName).Error("Failed to create DHCPv4 client")
		return
	}
	defer client.Close()

	c.client4 = client

	// Configure DHCP options using library's built-in client identification
	modifiers := []dhcpv4.Modifier{}

	if c.opts.Hostname != "" {
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptHostName(c.opts.Hostname)))
	}

	if c.opts.LeaseTime > 0 {
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(c.opts.LeaseTime)))
	}

	// Add vendor ID for identification
	modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("docker-net-dhcp")))

	// Use library's client identifier with interface MAC (or provided MAC)
	clientMAC := c.opts.MACAddr
	if clientMAC == nil {
		// Get interface MAC address using the library's approach
		if iface, err := net.InterfaceByName(c.ifaceName); err == nil {
			clientMAC = iface.HardwareAddr
		}
	}
	if clientMAC != nil {
		modifiers = append(modifiers, dhcpv4.WithOption(dhcpv4.OptClientIdentifier(clientMAC)))
	}

	ctx := context.Background()

	for {
		select {
		case <-c.stopChan:
			return
		default:
		}

		var lease *nclient4.Lease
		var err error

		var usingTransferredLease bool

		// Create a fresh modifiers slice for each request
		requestModifiers := make([]dhcpv4.Modifier, len(modifiers))
		copy(requestModifiers, modifiers)

		// Check if we have an existing lease from transfer
		if c.lease4 != nil {
			// Use the existing transferred lease
			lease = c.lease4
			usingTransferredLease = true
			log.Debug("Using existing transferred lease")
		} else {

			if c.opts.RequestIP != "" {
				// Request specific IP
				requestIP := net.ParseIP(c.opts.RequestIP)
				if requestIP == nil {
					log.WithField("request_ip", c.opts.RequestIP).Error("Invalid request IP address")
					continue
				}

				requestModifiers = append(requestModifiers, dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(requestIP)))
				log.WithField("request_ip", c.opts.RequestIP).Debug("Requesting specific IPv4 address")
			}

			// Attempt to get a DHCP lease
			lease, err = client.Request(ctx, requestModifiers...)
			if err != nil {
				log.WithError(err).Error("DHCPv4 request failed")

				// Send leasefail event
				events <- Event{
					Type: "leasefail",
					Data: Info{},
				}

				// Wait before retrying
				select {
				case <-time.After(10 * time.Second):
					continue
				case <-c.stopChan:
					return
				}
			}

			c.lease4 = lease
		}
		ack := lease.ACK

		// Extract IP information
		info := Info{
			IP:      fmt.Sprintf("%s/%d", ack.YourIPAddr.String(), prefixLenFromMask(ack.SubnetMask())),
			Gateway: "",
			Domain:  "",
		}

		// Extract gateway
		if routers := ack.Router(); len(routers) > 0 {
			info.Gateway = routers[0].String()
		}

		// Extract domain
		if domain := ack.DomainName(); domain != "" {
			info.Domain = domain
		}

		log.WithFields(log.Fields{
			"ip":      info.IP,
			"gateway": info.Gateway,
			"domain":  info.Domain,
		}).Info("DHCPv4 lease acquired")

		// Send bound event
		events <- Event{
			Type: "bound",
			Data: info,
		}

		// If we used a transferred lease, mark it as consumed after first binding
		if usingTransferredLease {
			// We've successfully used the transferred lease, now clear the flag
			// so future iterations will use normal DHCP renewal/request logic
			usingTransferredLease = false
			log.Debug("Transferred lease successfully bound, will use normal renewal cycle")
		}

		// Calculate renewal time (typically 50% of lease time)
		leaseTime := ack.IPAddressLeaseTime(0)
		if leaseTime == 0 {
			leaseTime = 24 * time.Hour // Default 24 hours
		}
		renewalTime := leaseTime / 2

		log.WithFields(log.Fields{
			"lease_time":   leaseTime,
			"renewal_time": renewalTime,
		}).Debug("Scheduling DHCPv4 lease renewal")

		// Wait for renewal time or stop signal
		renewalTimer := time.After(renewalTime)

	renewalLoop:
		for {
			select {
			case <-renewalTimer:
				// Try to renew the lease
				newLease, err := client.Renew(ctx, lease, requestModifiers...)
				if err != nil {
					log.WithError(err).Warn("DHCPv4 renewal failed, will try to get new lease")
					break renewalLoop // Break to outer loop to get new lease
				}

				c.lease4 = newLease
				newACK := newLease.ACK

				// Extract updated info
				renewInfo := Info{
					IP:      fmt.Sprintf("%s/%d", newACK.YourIPAddr.String(), prefixLenFromMask(newACK.SubnetMask())),
					Gateway: "",
					Domain:  "",
				}

				if routers := newACK.Router(); len(routers) > 0 {
					renewInfo.Gateway = routers[0].String()
				}

				if domain := newACK.DomainName(); domain != "" {
					renewInfo.Domain = domain
				}

				log.WithFields(log.Fields{
					"ip":      renewInfo.IP,
					"gateway": renewInfo.Gateway,
				}).Debug("DHCPv4 lease renewed")

				// Send renewal event
				events <- Event{
					Type: "renew",
					Data: renewInfo,
				}

				// Update lease and schedule next renewal
				lease = newLease
				newLeaseTime := newACK.IPAddressLeaseTime(0)
				if newLeaseTime == 0 {
					newLeaseTime = 24 * time.Hour
				}
				renewalTimer = time.After(newLeaseTime / 2)

			case <-c.stopChan:
				return
			}
		}
	}
}

// runDHCPv6Client runs the DHCPv6 client
func (c *DHCPClient) runDHCPv6Client(events chan Event) {
	// Create the native DHCPv6 client
	client, err := nclient6.New(c.ifaceName)
	if err != nil {
		log.WithError(err).WithField("interface", c.ifaceName).Error("Failed to create DHCPv6 client")
		return
	}
	defer client.Close()

	c.client6 = client

	// Configure DHCPv6 options
	modifiers := []dhcpv6.Modifier{}

	if c.opts.Hostname != "" {
		// FQDN option for DHCPv6
		modifiers = append(modifiers, dhcpv6.WithFQDN(0x00, c.opts.Hostname))
	}

	// Add client identifier for DHCPv6
	duid := &dhcpv6.DUIDLL{
		HWType:        1,               // Ethernet
		LinkLayerAddr: make([]byte, 6), // Will be filled with interface MAC
	}
	modifiers = append(modifiers, dhcpv6.WithClientID(duid))

	ctx := context.Background()

	for {
		select {
		case <-c.stopChan:
			return
		default:
		}

		// Create a fresh modifiers slice for each request
		requestModifiers := make([]dhcpv6.Modifier, len(modifiers))
		copy(requestModifiers, modifiers)

		if c.opts.RequestIP != "" {
			// Request specific IPv6 address
			requestIP := net.ParseIP(c.opts.RequestIP)
			if requestIP == nil || requestIP.To16() == nil {
				log.WithField("request_ip", c.opts.RequestIP).Error("Invalid request IPv6 address")
				continue
			}

			// For DHCPv6, we need to use IANA (Identity Association for Non-temporary Addresses)
			// We'll request the specific IP through IANA option
			log.WithField("request_ip", c.opts.RequestIP).Debug("Requesting specific IPv6 address")
		}

		// Get interface hardware address
		iface, err := net.InterfaceByName(c.ifaceName)
		if err != nil {
			log.WithError(err).WithField("interface", c.ifaceName).Error("Failed to get interface")
			continue
		}

		// Create a solicit message
		solicit, err := dhcpv6.NewSolicit(iface.HardwareAddr, requestModifiers...)
		if err != nil {
			log.WithError(err).Error("Failed to create DHCPv6 solicit message")
			continue
		}

		// Attempt to get a DHCPv6 lease
		lease, err := client.Request(ctx, solicit)
		if err != nil {
			log.WithError(err).Error("DHCPv6 request failed")

			// Send leasefail event
			events <- Event{
				Type: "leasefail",
				Data: Info{},
			}

			// Wait before retrying
			select {
			case <-time.After(10 * time.Second):
				continue
			case <-c.stopChan:
				return
			}
		}

		c.lease6 = lease

		// Extract IPv6 information
		var ipv6Addr net.IP
		var prefixLen int = 64 // Default IPv6 prefix length

		// Extract IPv6 address from IANA option
		if iana := lease.Options.OneIANA(); iana != nil {
			if addrs := iana.Options.Addresses(); len(addrs) > 0 {
				ipv6Addr = addrs[0].IPv6Addr
			}
		}

		if ipv6Addr == nil {
			log.Error("No IPv6 address received from DHCPv6 server")
			continue
		}

		info := Info{
			IP:      fmt.Sprintf("%s/%d", ipv6Addr.String(), prefixLen),
			Gateway: "",
			Domain:  "",
		}

		// Extract domain from domain search list
		if domains := lease.Options.DomainSearchList(); domains != nil && len(domains.Labels) > 0 {
			info.Domain = domains.Labels[0]
		}

		log.WithFields(log.Fields{
			"ip":     info.IP,
			"domain": info.Domain,
		}).Info("DHCPv6 lease acquired")

		// Send bound event
		events <- Event{
			Type: "bound",
			Data: info,
		}

		// Calculate renewal time from IANA option
		var renewalTime time.Duration = 12 * time.Hour // Default renewal time

		if iana := lease.Options.OneIANA(); iana != nil {
			if iana.T1 > 0 {
				renewalTime = time.Duration(iana.T1) * time.Second
			}
		}

		log.WithFields(log.Fields{
			"renewal_time": renewalTime,
		}).Debug("Scheduling DHCPv6 lease renewal")

		// Wait for renewal time or stop signal
		renewalTimer := time.After(renewalTime)

	renewalLoop:
		for {
			select {
			case <-renewalTimer:
				// Try to renew the lease
				renew, err := dhcpv6.NewMessage()
				if err != nil {
					log.WithError(err).Warn("Failed to create DHCPv6 renew message")
					break renewalLoop // Break to outer loop to get new lease
				}
				renew.MessageType = dhcpv6.MessageTypeRenew
				renew.TransactionID = lease.TransactionID

				// Apply modifiers to the renew message
				for _, mod := range requestModifiers {
					mod(renew)
				}

				newLease, err := client.Request(ctx, renew)
				if err != nil {
					log.WithError(err).Warn("DHCPv6 renewal failed, will try to get new lease")
					break renewalLoop // Break to outer loop to get new lease
				}

				c.lease6 = newLease

				// Extract updated IPv6 information
				var newIPv6Addr net.IP
				if iana := newLease.Options.OneIANA(); iana != nil {
					if addrs := iana.Options.Addresses(); len(addrs) > 0 {
						newIPv6Addr = addrs[0].IPv6Addr
					}
				}

				if newIPv6Addr == nil {
					log.Warn("No IPv6 address in DHCPv6 renewal response")
					break renewalLoop
				}

				renewInfo := Info{
					IP:      fmt.Sprintf("%s/%d", newIPv6Addr.String(), prefixLen),
					Gateway: "",
					Domain:  "",
				}

				if domains := newLease.Options.DomainSearchList(); domains != nil && len(domains.Labels) > 0 {
					renewInfo.Domain = domains.Labels[0]
				}

				log.WithFields(log.Fields{
					"ip": renewInfo.IP,
				}).Debug("DHCPv6 lease renewed")

				// Send renewal event
				events <- Event{
					Type: "renew",
					Data: renewInfo,
				}

				// Update lease and schedule next renewal
				lease = newLease
				newRenewalTime := renewalTime // Keep same renewal interval
				if iana := newLease.Options.OneIANA(); iana != nil && iana.T1 > 0 {
					newRenewalTime = time.Duration(iana.T1) * time.Second
				}
				renewalTimer = time.After(newRenewalTime)

			case <-c.stopChan:
				return
			}
		}
	}
}

// Finish gracefully stops the DHCP client
func (c *DHCPClient) Finish(ctx context.Context) error {
	c.transferMutex.RLock()
	isTransferred := c.isTransferred
	c.transferMutex.RUnlock()

	close(c.stopChan)

	var err error

	// Only release lease if it hasn't been transferred
	if !isTransferred {
		// Release IPv4 lease if present
		if c.client4 != nil && c.lease4 != nil {
			if releaseErr := c.client4.Release(c.lease4); releaseErr != nil {
				log.WithError(releaseErr).Warn("Failed to release DHCPv4 lease")
				err = releaseErr
			} else {
				log.Debug("DHCPv4 lease released successfully")
			}
		}

		// DHCPv6 client doesn't support release method
		if c.client6 != nil && c.lease6 != nil {
			log.Debug("DHCPv6 lease will expire naturally (no release method available)")
		}
	} else {
		log.Debug("Skipping lease release - lease was transferred to persistent client")
	}

	// Close clients
	if c.client4 != nil {
		c.client4.Close()
	}
	if c.client6 != nil {
		c.client6.Close()
	}

	return err
}

// GetIP is a convenience function that gets a DHCP lease once and returns the info
func GetIP(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
	client, err := NewDHCPClient(iface, opts)
	if err != nil {
		return Info{}, fmt.Errorf("failed to create DHCP client: %w", err)
	}

	events, err := client.Start()
	if err != nil {
		return Info{}, fmt.Errorf("failed to start DHCP client: %w", err)
	}

	// Wait for bound event or timeout
	select {
	case event := <-events:
		switch event.Type {
		case "bound":
			// Clean up the client
			client.Finish(ctx)
			return event.Data, nil
		case "leasefail":
			client.Finish(ctx)
			return Info{}, util.ErrNoLease
		}
	case <-ctx.Done():
		client.Finish(ctx)
		return Info{}, ctx.Err()
	}

	client.Finish(ctx)
	return Info{}, util.ErrNoLease
}

// RegisterHostname performs a DHCP request to register hostname with the DHCP server
// This forces a new DHCP REQUEST message to be sent with the hostname option
func RegisterHostname(ctx context.Context, iface string, currentIP string, hostname string) error {
	return RegisterHostnameWithNamespace(ctx, iface, currentIP, hostname, "")
}

// RegisterHostnameWithNamespace performs a DHCP INFORM to register hostname with the DHCP server in a specific namespace
// This sends a DHCP INFORM message which is specifically for updating hostname without getting a new lease
func RegisterHostnameWithNamespace(ctx context.Context, iface string, currentIP string, hostname string, namespace string) error {
	if hostname == "" {
		log.WithField("interface", iface).Debug("RegisterHostname: hostname is empty, skipping")
		return nil // Nothing to register
	}

	log.WithFields(log.Fields{
		"interface": iface,
		"hostname":  hostname,
		"ip":        currentIP,
		"namespace": namespace,
	}).Info("RegisterHostname: Starting hostname registration with DHCP INFORM")

	// Extract just the IP part without CIDR
	ipOnly := strings.Split(currentIP, "/")[0]
	clientIP := net.ParseIP(ipOnly)
	if clientIP == nil {
		log.WithFields(log.Fields{
			"interface": iface,
			"hostname":  hostname,
			"ip":        currentIP,
		}).Error("RegisterHostname: Invalid IP address format")
		return nil
	}

	// Handle network namespace switching if needed
	if namespace != "" {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		origNS, err := netns.Get()
		if err != nil {
			log.WithError(err).Error("RegisterHostname: Failed to get current namespace")
			return nil
		}
		defer origNS.Close()

		ns, err := netns.GetFromPath(namespace)
		if err != nil {
			log.WithError(err).WithField("namespace", namespace).Error("RegisterHostname: Failed to open network namespace")
			return nil
		}
		defer ns.Close()

		if err := netns.Set(ns); err != nil {
			log.WithError(err).Error("RegisterHostname: Failed to enter network namespace")
			return nil
		}
		defer netns.Set(origNS)
	}

	log.WithFields(log.Fields{
		"interface": iface,
		"hostname":  hostname,
		"ip_only":   ipOnly,
		"namespace": namespace,
	}).Debug("RegisterHostname: Creating DHCP client for hostname registration REQUEST")

	// Create DHCP client
	client, err := nclient4.New(iface)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"interface": iface,
			"hostname":  hostname,
			"namespace": namespace,
		}).Error("RegisterHostname: Failed to create DHCP client for hostname registration")
		return nil // Non-fatal
	}
	defer client.Close()

	// Get interface hardware address for client identification
	netIface, err := net.InterfaceByName(iface)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"interface": iface,
			"hostname":  hostname,
		}).Error("RegisterHostname: Failed to get interface information")
		return nil
	}

	// Create DHCP REQUEST using library's built-in client identification
	requestModifiers := []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(clientIP)),
		dhcpv4.WithOption(dhcpv4.OptHostName(hostname)),
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier("docker-net-dhcp")),
		dhcpv4.WithOption(dhcpv4.OptClientIdentifier(netIface.HardwareAddr)),
		// Use a very short lease time to signal this is an update, not a new lease
		dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(60 * time.Second)),
	}

	// Create a timeout context for the hostname registration
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	log.WithFields(log.Fields{
		"interface": iface,
		"hostname":  hostname,
		"ip":        ipOnly,
		"namespace": namespace,
		"client_ip": clientIP.String(),
	}).Info("RegisterHostname: Sending DHCP REQUEST for hostname registration with existing IP")

	// Send the REQUEST message using the client's Request method
	// This properly handles the DHCP protocol flow
	_, err = client.Request(timeoutCtx, requestModifiers...)
	if err != nil {
		// Log as warning since hostname registration failures are often non-critical
		log.WithError(err).WithFields(log.Fields{
			"interface": iface,
			"hostname":  hostname,
			"ip":        currentIP,
			"namespace": namespace,
		}).Warn("RegisterHostname: DHCP REQUEST for hostname registration received NAK or timeout (may be normal)")
		return nil
	}

	log.WithFields(log.Fields{
		"interface": iface,
		"hostname":  hostname,
		"ip":        currentIP,
		"namespace": namespace,
	}).Info("RegisterHostname: DHCP REQUEST for hostname registration completed successfully")

	return nil
}

// Helper function to convert subnet mask to prefix length
func prefixLenFromMask(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}

// LeaseInfo contains information about an active DHCP lease
type LeaseInfo struct {
	IPv4Lease    *nclient4.Lease
	IPv6Lease    *dhcpv6.Message
	AcquiredTime time.Time
	IsIPv6       bool
}

// GetActiveLease returns the current active lease information
func (c *DHCPClient) GetActiveLease() (*LeaseInfo, error) {
	c.transferMutex.RLock()
	defer c.transferMutex.RUnlock()

	if c.isTransferred {
		return nil, fmt.Errorf("lease has already been transferred")
	}

	info := &LeaseInfo{
		AcquiredTime: time.Now(),
	}

	if c.lease4 != nil {
		info.IPv4Lease = c.lease4
		info.IsIPv6 = false
	} else if c.lease6 != nil {
		info.IPv6Lease = c.lease6
		info.IsIPv6 = true
	} else {
		return nil, fmt.Errorf("no active lease available")
	}

	return info, nil
}

// TransferLease marks this client as transferred and returns lease info for handoff
func (c *DHCPClient) TransferLease() (*LeaseInfo, error) {
	c.transferMutex.Lock()
	defer c.transferMutex.Unlock()

	if c.isTransferred {
		return nil, fmt.Errorf("lease has already been transferred")
	}

	leaseInfo, err := c.getActiveLeaseUnsafe()
	if err != nil {
		return nil, fmt.Errorf("failed to get active lease: %w", err)
	}

	// Mark as transferred to prevent accidental release
	c.isTransferred = true

	log.WithFields(log.Fields{
		"interface": c.ifaceName,
		"ipv6":      leaseInfo.IsIPv6,
		"ip":        c.getLeaseIP(leaseInfo),
	}).Info("DHCP lease transferred to persistent client")

	return leaseInfo, nil
}

// getActiveLeaseUnsafe returns lease info without locking (internal use)
func (c *DHCPClient) getActiveLeaseUnsafe() (*LeaseInfo, error) {
	info := &LeaseInfo{
		AcquiredTime: time.Now(),
	}

	if c.lease4 != nil {
		info.IPv4Lease = c.lease4
		info.IsIPv6 = false
	} else if c.lease6 != nil {
		info.IPv6Lease = c.lease6
		info.IsIPv6 = true
	} else {
		return nil, fmt.Errorf("no active lease available")
	}

	return info, nil
}

// getLeaseIP extracts IP address from lease info
func (c *DHCPClient) getLeaseIP(leaseInfo *LeaseInfo) string {
	if leaseInfo.IsIPv6 && leaseInfo.IPv6Lease != nil {
		// Extract IPv6 address
		if iana := leaseInfo.IPv6Lease.Options.OneIANA(); iana != nil {
			if addrs := iana.Options.Addresses(); len(addrs) > 0 {
				return fmt.Sprintf("%s/64", addrs[0].IPv6Addr.String())
			}
		}
	} else if leaseInfo.IPv4Lease != nil {
		ack := leaseInfo.IPv4Lease.ACK
		return fmt.Sprintf("%s/%d", ack.YourIPAddr.String(), prefixLenFromMask(ack.SubnetMask()))
	}
	return "unknown"
}

// NewDHCPClientFromLease creates a new DHCP client with an existing lease
func NewDHCPClientFromLease(iface string, opts *DHCPClientOptions, leaseInfo *LeaseInfo) (*DHCPClient, error) {
	client, err := NewDHCPClient(iface, opts)
	if err != nil {
		return nil, err
	}

	// Pre-populate with existing lease
	if leaseInfo.IsIPv6 {
		client.lease6 = leaseInfo.IPv6Lease
	} else {
		client.lease4 = leaseInfo.IPv4Lease
	}

	log.WithFields(log.Fields{
		"interface": iface,
		"ipv6":      leaseInfo.IsIPv6,
		"ip":        client.getLeaseIP(leaseInfo),
	}).Info("Created DHCP client from transferred lease")

	return client, nil
}
