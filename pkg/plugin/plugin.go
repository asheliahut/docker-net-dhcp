package plugin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/asheliahut/docker-net-dhcp/pkg/dhcp"
	"github.com/asheliahut/docker-net-dhcp/pkg/util"
)

// DriverName is the name of the Docker Network Driver
const DriverName string = "net-dhcp"

const defaultLeaseTimeout = 10 * time.Second

var driverRegexp = regexp.MustCompile(`^ghcr\.io/asheliahut/docker-net-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// DHCPNetworkOptions contains options for the DHCP network driver
type DHCPNetworkOptions struct {
	Bridge          string
	IPv6            bool
	LeaseTimeout    time.Duration `mapstructure:"lease_timeout"`
	IgnoreConflicts bool          `mapstructure:"ignore_conflicts"`
	SkipRoutes      bool          `mapstructure:"skip_routes"`
}

func decodeOpts(input interface{}) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	optsDecoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &opts,
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return opts, fmt.Errorf("failed to create options decoder: %w", err)
	}

	if err := optsDecoder.Decode(input); err != nil {
		return opts, err
	}

	return opts, nil
}

type joinHint struct {
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	Gateway string
	MAC     net.HardwareAddr
}

// Plugin is the DHCP network plugin
type Plugin struct {
	awaitTimeout time.Duration

	docker *docker.Client
	server http.Server

	joinHints      map[string]joinHint
	persistentDHCP map[string]*dhcpManager
	initialDHCP    map[string]*dhcp.DHCPClient

	// Container lifecycle tracking
	eventChan   chan events.Message
	eventCancel context.CancelFunc
	eventCtx    context.Context

	// Resource management
	cleanupTimer *time.Timer
	shutdownChan chan struct{}
}

// NewPlugin creates a new Plugin
func NewPlugin(awaitTimeout time.Duration) (*Plugin, error) {
	var (
		currentIteration = 0
		maxRetries       = 10
		client           *docker.Client
		err              error
	)
	for {
		client, err = docker.NewClientWithOpts(
			docker.WithHost("unix:///run/docker.sock"),
			docker.WithAPIVersionNegotiation(),
			docker.WithTimeout(5*time.Second)) // If local Docker doesn't respond in under 2s, it's probably not ready.
		if err == nil {
			break
		}

		if currentIteration >= maxRetries {
			return nil, fmt.Errorf("failed to connect to Docker: %w", err)
		}

		time.Sleep(5 * time.Second)
		currentIteration++
	}

	// Set up container event monitoring
	eventCtx, eventCancel := context.WithCancel(context.Background())
	eventChan := make(chan events.Message, 100)

	p := Plugin{
		awaitTimeout: awaitTimeout,

		docker: client,

		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		initialDHCP:    make(map[string]*dhcp.DHCPClient),

		eventChan:    eventChan,
		eventCancel:  eventCancel,
		eventCtx:     eventCtx,
		shutdownChan: make(chan struct{}),
	}

	// Start container lifecycle monitoring
	go p.monitorContainerEvents()

	// Start resource cleanup monitoring
	go p.startResourceMonitor()

	mux := http.NewServeMux()
	mux.HandleFunc("/NetworkDriver.GetCapabilities", p.apiGetCapabilities)

	mux.HandleFunc("/NetworkDriver.CreateNetwork", p.apiCreateNetwork)
	mux.HandleFunc("/NetworkDriver.DeleteNetwork", p.apiDeleteNetwork)

	mux.HandleFunc("/NetworkDriver.CreateEndpoint", p.apiCreateEndpoint)
	mux.HandleFunc("/NetworkDriver.EndpointOperInfo", p.apiEndpointOperInfo)
	mux.HandleFunc("/NetworkDriver.DeleteEndpoint", p.apiDeleteEndpoint)

	mux.HandleFunc("/NetworkDriver.Join", p.apiJoin)
	mux.HandleFunc("/NetworkDriver.Leave", p.apiLeave)

	p.server = http.Server{
		Handler: handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
	}

	return &p, nil
}

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	return p.server.Serve(l)
}

// Close stops the plugin server
func (p *Plugin) Close() error {
	// Signal shutdown
	close(p.shutdownChan)

	// Stop cleanup timer
	if p.cleanupTimer != nil {
		p.cleanupTimer.Stop()
	}

	// Stop container event monitoring
	if p.eventCancel != nil {
		p.eventCancel()
	}

	// Clean up any remaining DHCP managers with timeout
	p.cleanupAllManagers(30 * time.Second)

	// Clean up any remaining initial DHCP clients
	p.cleanupAllInitialClients(10 * time.Second)

	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	if err := p.server.Close(); err != nil {
		return fmt.Errorf("failed to close http server: %w", err)
	}

	return nil
}

// monitorContainerEvents watches for container lifecycle events
func (p *Plugin) monitorContainerEvents() {
	// Set up Docker events listener
	eventOptions := events.ListOptions{
		Filters: filters.NewArgs(),
	}
	eventOptions.Filters.Add("type", "container")
	eventOptions.Filters.Add("event", "die")
	eventOptions.Filters.Add("event", "kill")
	eventOptions.Filters.Add("event", "stop")

	eventReader, _ := p.docker.Events(p.eventCtx, eventOptions)
	if eventReader == nil {
		log.Error("Failed to start Docker events listener: reader is nil")
		return
	}

	log.Info("Started container lifecycle monitoring")

	for {
		select {
		case <-p.eventCtx.Done():
			log.Info("Stopping container lifecycle monitoring")
			return
		case event := <-eventReader:
			if event.Type == events.ContainerEventType {
				p.handleContainerEvent(event)
			}
		}
	}
}

// handleContainerEvent processes container lifecycle events
func (p *Plugin) handleContainerEvent(event events.Message) {
	containerID := event.ID
	action := event.Action

	log.WithFields(log.Fields{
		"container": containerID[:12],
		"action":    action,
	}).Debug("Processing container event")

	// Handle container death/stop events
	if action == "die" || action == "kill" || action == "stop" {
		// Find any DHCP managers for this container
		var managersToCleanup []string
		for endpointID, manager := range p.persistentDHCP {
			// Check if this manager is for the stopped container
			if p.isManagerForContainer(manager, containerID) {
				managersToCleanup = append(managersToCleanup, endpointID)
			}
		}

		// Clean up managers for stopped container
		for _, endpointID := range managersToCleanup {
			manager := p.persistentDHCP[endpointID]
			log.WithFields(log.Fields{
				"container": containerID[:12],
				"endpoint":  endpointID[:12],
				"action":    action,
			}).Info("Container stopped, cleaning up DHCP manager")

			if err := manager.Stop(); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"container": containerID[:12],
					"endpoint":  endpointID[:12],
				}).Error("Failed to stop DHCP manager for stopped container")
			}
			delete(p.persistentDHCP, endpointID)
		}
	}
}

// isManagerForContainer checks if a DHCP manager belongs to a specific container
func (p *Plugin) isManagerForContainer(manager *dhcpManager, containerID string) bool {
	// We need to check if the manager's network namespace corresponds to this container
	// This is done by inspecting the network to find containers and matching endpoint IDs
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dockerNet, err := p.docker.NetworkInspect(ctx, manager.joinReq.NetworkID, network.InspectOptions{})
	if err != nil {
		log.WithError(err).WithField("network", manager.joinReq.NetworkID[:12]).Warn("Failed to inspect network for container matching")
		return false
	}

	for ctrID, info := range dockerNet.Containers {
		if ctrID == containerID && info.EndpointID == manager.joinReq.EndpointID {
			return true
		}
	}

	return false
}

// startResourceMonitor monitors and cleans up stale resources
func (p *Plugin) startResourceMonitor() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	log.Info("Started resource cleanup monitor")

	for {
		select {
		case <-ticker.C:
			p.performPeriodicCleanup()
		case <-p.shutdownChan:
			log.Info("Stopping resource cleanup monitor")
			return
		}
	}
}

// performPeriodicCleanup checks for and cleans up stale resources
func (p *Plugin) performPeriodicCleanup() {
	log.Debug("Performing periodic resource cleanup check")

	staleManagers := make([]string, 0)
	staleInitialClients := make([]string, 0)
	
	// Check each DHCP manager for staleness
	for endpointID, manager := range p.persistentDHCP {
		if p.isManagerStale(manager) {
			staleManagers = append(staleManagers, endpointID)
		}
	}

	// Check for stale initial clients (shouldn't happen, but safety net)
	for endpointID := range p.initialDHCP {
		// If there's an initial client without a corresponding manager, it's likely stale
		if _, hasManager := p.persistentDHCP[endpointID]; !hasManager {
			staleInitialClients = append(staleInitialClients, endpointID)
		}
	}

	// Clean up stale managers
	for _, endpointID := range staleManagers {
		manager := p.persistentDHCP[endpointID]
		log.WithField("endpoint", endpointID[:12]).Info("Cleaning up stale DHCP manager")

		go func(m *dhcpManager, id string) {
			if err := m.Stop(); err != nil {
				log.WithError(err).WithField("endpoint", id[:12]).Error("Failed to stop stale DHCP manager")
			}
		}(manager, endpointID)

		delete(p.persistentDHCP, endpointID)
	}

	// Clean up stale initial clients
	for _, endpointID := range staleInitialClients {
		client := p.initialDHCP[endpointID]
		log.WithField("endpoint", endpointID[:12]).Warn("Cleaning up stale initial DHCP client")

		go func(c *dhcp.DHCPClient, id string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Finish(ctx); err != nil {
				log.WithError(err).WithField("endpoint", id[:12]).Error("Failed to stop stale initial DHCP client")
			}
		}(client, endpointID)

		delete(p.initialDHCP, endpointID)
	}

	totalCleaned := len(staleManagers) + len(staleInitialClients)
	if totalCleaned > 0 {
		log.WithFields(log.Fields{
			"managers": len(staleManagers),
			"initial_clients": len(staleInitialClients),
			"total": totalCleaned,
		}).Info("Cleaned up stale DHCP resources")
	}
}

// isManagerStale checks if a DHCP manager is stale (container no longer exists)
func (p *Plugin) isManagerStale(manager *dhcpManager) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check if there's still a container using this manager's endpoint by inspecting the network
	dockerNet, err := p.docker.NetworkInspect(ctx, manager.joinReq.NetworkID, network.InspectOptions{})
	if err != nil {
		log.WithError(err).WithField("network", manager.joinReq.NetworkID[:12]).Warn("Failed to inspect network for staleness check")
		// If we can't inspect the network, assume the manager might be stale
		return true
	}

	// Check if any container is still using this endpoint
	for _, info := range dockerNet.Containers {
		if info.EndpointID == manager.joinReq.EndpointID {
			// Found a container still using this endpoint, manager is not stale
			return false
		}
	}

	// No container found using this endpoint, manager is stale
	return true
}

// cleanupAllManagers cleans up all managers with timeout
func (p *Plugin) cleanupAllManagers(timeout time.Duration) {
	if len(p.persistentDHCP) == 0 {
		return
	}

	log.WithField("count", len(p.persistentDHCP)).Info("Cleaning up all DHCP managers")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Create a channel to track completion
	done := make(chan struct{}, len(p.persistentDHCP))

	// Start cleanup for each manager
	for endpointID, manager := range p.persistentDHCP {
		go func(m *dhcpManager, id string) {
			defer func() { done <- struct{}{} }()

			if err := m.Stop(); err != nil {
				log.WithError(err).WithField("endpoint", id[:12]).Error("Failed to stop DHCP manager during shutdown")
			}
		}(manager, endpointID)
	}

	// Wait for all managers to finish or timeout
	managersToWait := len(p.persistentDHCP)
	for i := 0; i < managersToWait; i++ {
		select {
		case <-done:
			// Manager finished
		case <-ctx.Done():
			log.Warn("Timeout waiting for DHCP managers to stop, forcing cleanup")
			// Force cleanup remaining managers
			for _, manager := range p.persistentDHCP {
				go manager.forceCleanup()
			}
			// Clear the map
			for endpointID := range p.persistentDHCP {
				delete(p.persistentDHCP, endpointID)
			}
			return
		}
	}

	// Clear the map
	for endpointID := range p.persistentDHCP {
		delete(p.persistentDHCP, endpointID)
	}

	log.Info("All DHCP managers cleaned up")
}

// cleanupAllInitialClients cleans up all initial DHCP clients with timeout
func (p *Plugin) cleanupAllInitialClients(timeout time.Duration) {
	if len(p.initialDHCP) == 0 {
		return
	}

	log.WithField("count", len(p.initialDHCP)).Info("Cleaning up all initial DHCP clients")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Create a channel to track completion
	done := make(chan struct{}, len(p.initialDHCP))

	// Start cleanup for each initial client
	for endpointID, client := range p.initialDHCP {
		go func(c *dhcp.DHCPClient, id string) {
			defer func() { done <- struct{}{} }()

			if err := c.Finish(ctx); err != nil {
				log.WithError(err).WithField("endpoint", id[:12]).Error("Failed to stop initial DHCP client during shutdown")
			}
		}(client, endpointID)
	}

	// Wait for all clients to finish or timeout
	clientsToWait := len(p.initialDHCP)
	for i := 0; i < clientsToWait; i++ {
		select {
		case <-done:
			// Client finished
		case <-ctx.Done():
			log.Warn("Timeout waiting for initial DHCP clients to stop")
			// Clear the map even if some didn't finish cleanly
			for endpointID := range p.initialDHCP {
				delete(p.initialDHCP, endpointID)
			}
			return
		}
	}

	// Clear the map
	for endpointID := range p.initialDHCP {
		delete(p.initialDHCP, endpointID)
	}

	log.Info("All initial DHCP clients cleaned up")
}
