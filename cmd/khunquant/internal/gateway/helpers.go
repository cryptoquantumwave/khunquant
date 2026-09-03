package gateway

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/cmd/khunquant/internal"
	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/dingtalk"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/discord"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/feishu"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/irc"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/line"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/maixcam"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/matrix"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/onebot"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/pico"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/qq"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/slack"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/telegram"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/wecom"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/whatsapp"
	_ "github.com/cryptoquantumwave/khunquant/pkg/channels/whatsapp_native"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/debugtap"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/devices"
	"github.com/cryptoquantumwave/khunquant/pkg/devmcp"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binance"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binanceth"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/bitkub"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/okx"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/settrade"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/webull"
	"github.com/cryptoquantumwave/khunquant/pkg/health"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/pid"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/voice"
)

// Timeout constants for service operations
const (
	serviceRestartTimeout   = 30 * time.Second
	serviceShutdownTimeout  = 30 * time.Second
	providerReloadTimeout   = 30 * time.Second
	gracefulShutdownTimeout = 15 * time.Second
)

// gatewayServices holds references to all running services
type gatewayServices struct {
	CronService      *cron.CronService
	HeartbeatService *heartbeat.HeartbeatService
	MediaStore       media.MediaStore
	ChannelManager   *channels.Manager
	DeviceService    *devices.Service
	HealthServer     *health.Server
	DebugTap         *debugtap.Store                 // non-nil only while dev-mcp is enabled
	LogBuf           *debugtap.LogBuffer             // persists across reloads while dev-mcp is on
	SandboxServer    *sandbox.Server                 // non-nil only while sandbox is enabled
	SandboxToken     string                          // bearer token for sandbox API
	SimulatorReseter interface{ ResetState() error } // non-nil when sandbox is enabled; wraps StateManager for reset-state endpoint
}

// sandboxStateReseter wraps StateManager and Store to implement the ResetState interface.
type sandboxStateReseter struct {
	sm    *sandbox.StateManager
	store *sandbox.Store
}

func (s *sandboxStateReseter) ResetState() error {
	// Reset all venues that have fixtures or state
	venues := s.store.Venues()
	for _, venue := range venues {
		if err := s.sm.Reset(venue); err != nil {
			logger.WarnCF("sandbox", fmt.Sprintf("failed to reset venue %s", venue), map[string]any{"error": err.Error()})
			// Continue resetting other venues even if one fails
		}
	}
	return nil
}

func gatewayCmd(debug bool) error {
	if debug {
		logger.SetLevel(logger.DEBUG)
		fmt.Println("🔍 Debug mode enabled")
	}

	configPath := internal.GetConfigPath()
	cfg, err := internal.LoadConfig()
	if err != nil {
		return fmt.Errorf("error loading config: %w", err)
	}

	// Arm sandbox state immediately after config load, before any exchange clients
	// can be constructed. This prevents a window where enabled config but GlobalState()
	// says disabled, allowing real orders through. With empty baseURL, transports error
	// loudly instead of passing through.
	if cfg.Debug.Sandbox.Enabled {
		sandbox.SetGlobalState(true, "")
		logger.Debugf("sandbox: state armed (enabled, baseURL empty)")
	}

	provider, modelID, err := providers.CreateProvider(cfg)
	if err != nil {
		return fmt.Errorf("error creating provider: %w", err)
	}

	// Use the resolved model ID from provider creation
	if modelID != "" {
		cfg.Agents.Defaults.ModelName = modelID
	}

	msgBus := bus.NewMessageBus()
	agentLoop := agent.NewAgentLoop(cfg, msgBus, provider)

	// Print agent startup info
	fmt.Println("\n📦 Agent Status:")
	startupInfo := agentLoop.GetStartupInfo()
	toolsInfo := startupInfo["tools"].(map[string]any)
	skillsInfo := startupInfo["skills"].(map[string]any)
	toolNames, _ := toolsInfo["names"].([]string)
	fmt.Printf("  • Tools: %d loaded (%s)\n", toolsInfo["count"], strings.Join(toolNames, ", "))
	fmt.Printf("  • Skills: %d/%d available\n",
		skillsInfo["available"],
		skillsInfo["total"])

	// Log to file as well
	logger.InfoCF("agent", "Agent initialized",
		map[string]any{
			"tools_count":      toolsInfo["count"],
			"tools":            toolNames,
			"skills_total":     skillsInfo["total"],
			"skills_available": skillsInfo["available"],
		})

	// Claim the PID file before starting services. WritePidFile records
	// os.Getpid(), so it has to run here in the gateway process rather than in
	// the launcher that spawned it, and it refuses to write when a live gateway
	// already holds the file. That refusal is the point: without it a launcher
	// restart forgets the running gateway (it is tracked only by an in-memory
	// *exec.Cmd) and happily starts a second one on the same port.
	home := config.HomeDir()
	if _, err := pid.WritePidFile(home, cfg.Gateway.Host, cfg.Gateway.Port); err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	defer pid.RemovePidFileIfPID(home, os.Getpid())

	// Setup and start all services
	services, err := setupAndStartServices(cfg, agentLoop, msgBus)
	if err != nil {
		return err
	}

	fmt.Printf("✓ Gateway started on %s:%d\n", cfg.Gateway.Host, cfg.Gateway.Port)
	fmt.Println("Press Ctrl+C to stop")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go agentLoop.Run(ctx)

	// Setup config file watcher for hot reload
	configReloadChan, stopWatch := setupConfigWatcherPolling(configPath, debug)
	defer stopWatch()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	// Main event loop - wait for signals or config changes
	for {
		select {
		case <-sigChan:
			logger.Info("Shutting down...")
			shutdownGateway(services, agentLoop, provider, true)
			return nil

		case newCfg := <-configReloadChan:
			err := handleConfigReload(ctx, agentLoop, newCfg, &provider, services, msgBus)
			if err != nil {
				logger.Errorf("Config reload failed: %v", err)
			}
		}
	}
}

// setupAndStartServices initializes and starts all services
func setupAndStartServices(
	cfg *config.Config,
	agentLoop *agent.AgentLoop,
	msgBus *bus.MessageBus,
) (*gatewayServices, error) {
	services := &gatewayServices{}

	// Setup cron tool and service
	execTimeout := time.Duration(cfg.Tools.Cron.ExecTimeoutMinutes) * time.Minute
	var setupDNStore *deltaneutral.Store
	services.CronService, setupDNStore = setupCronTool(
		agentLoop,
		msgBus,
		cfg.WorkspacePath(),
		cfg.Agents.Defaults.RestrictToWorkspace,
		execTimeout,
		cfg,
	)
	if err := services.CronService.Start(); err != nil {
		return nil, fmt.Errorf("error starting cron service: %w", err)
	}
	fmt.Println("✓ Cron service started")

	// Setup heartbeat service
	services.HeartbeatService = heartbeat.NewHeartbeatService(
		cfg.WorkspacePath(),
		cfg.Heartbeat.Interval,
		cfg.Heartbeat.Enabled,
	)
	services.HeartbeatService.SetBus(msgBus)
	services.HeartbeatService.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		// Use cli:direct as fallback if no valid channel
		if channel == "" || chatID == "" {
			channel, chatID = "cli", "direct"
		}
		// Use ProcessHeartbeat - no session history, each heartbeat is independent
		var response string
		var err error
		response, err = agentLoop.ProcessHeartbeat(context.Background(), prompt, channel, chatID)
		if err != nil {
			return tools.ErrorResult(fmt.Sprintf("Heartbeat error: %v", err))
		}
		if response == "HEARTBEAT_OK" {
			return tools.SilentResult("Heartbeat OK")
		}
		// For heartbeat, always return silent - the subagent result will be
		// sent to user via processSystemMessage when the async task completes
		return tools.SilentResult(response)
	})
	if err := services.HeartbeatService.Start(); err != nil {
		return nil, fmt.Errorf("error starting heartbeat service: %w", err)
	}
	fmt.Println("✓ Heartbeat service started")

	// Create media store for file lifecycle management with TTL cleanup
	services.MediaStore = media.NewFileMediaStoreWithCleanup(media.MediaCleanerConfig{
		Enabled:  cfg.Tools.MediaCleanup.Enabled,
		MaxAge:   time.Duration(cfg.Tools.MediaCleanup.MaxAge) * time.Minute,
		Interval: time.Duration(cfg.Tools.MediaCleanup.Interval) * time.Minute,
	})
	// Start the media store if it's a FileMediaStore with cleanup
	if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
		fms.Start()
	}

	// Create channel manager
	var err error
	services.ChannelManager, err = channels.NewManager(cfg, msgBus, services.MediaStore)
	if err != nil {
		// Stop the media store if it's a FileMediaStore with cleanup
		if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
		return nil, fmt.Errorf("error creating channel manager: %w", err)
	}

	// Inject channel manager and media store into agent loop
	agentLoop.SetChannelManager(services.ChannelManager)
	agentLoop.SetMediaStore(services.MediaStore)

	// Wire up voice transcription if a supported provider is configured.
	if transcriber := voice.DetectTranscriber(cfg); transcriber != nil {
		agentLoop.SetTranscriber(transcriber)
		logger.InfoCF("voice", "Transcription enabled (agent-level)", map[string]any{"provider": transcriber.Name()})
	}

	enabledChannels := services.ChannelManager.GetEnabledChannels()
	if len(enabledChannels) > 0 {
		fmt.Printf("✓ Channels enabled: %s\n", enabledChannels)
	} else {
		fmt.Println("⚠ Warning: No channels enabled")
	}

	// Setup shared HTTP server with health endpoints and webhook handlers
	addr := fmt.Sprintf("%s:%d", cfg.Gateway.Host, cfg.Gateway.Port)
	services.HealthServer = health.NewServer(cfg.Gateway.Host, cfg.Gateway.Port)
	services.ChannelManager.SetupHTTPServer(addr, services.HealthServer)
	registerCronAPI(services.ChannelManager, services.CronService, setupDNStore)
	if cfg.Debug.DevMCP.Enabled {
		registerDevMCP(cfg, services, agentLoop)
	}
	if cfg.Debug.Sandbox.Enabled {
		if err := startAndRegisterSandbox(cfg, services); err != nil {
			logger.Errorf("sandbox: failed to start: %v", err)
		}
	}

	if err := services.ChannelManager.StartAll(context.Background()); err != nil {
		return nil, fmt.Errorf("error starting channels: %w", err)
	}

	fmt.Printf("✓ Health endpoints available at http://%s:%d/health and /ready\n", cfg.Gateway.Host, cfg.Gateway.Port)

	// Setup state manager and device service
	stateManager := state.NewManager(cfg.WorkspacePath())
	services.DeviceService = devices.NewService(devices.Config{
		Enabled:    cfg.Devices.Enabled,
		MonitorUSB: cfg.Devices.MonitorUSB,
	}, stateManager)
	services.DeviceService.SetBus(msgBus)
	if err := services.DeviceService.Start(context.Background()); err != nil {
		logger.ErrorCF("device", "Error starting device service", map[string]any{"error": err.Error()})
	} else if cfg.Devices.Enabled {
		fmt.Println("✓ Device event service started")
	}

	return services, nil
}

// stopAndCleanupServices stops all services and cleans up resources
func stopAndCleanupServices(
	services *gatewayServices,
	shutdownTimeout time.Duration,
) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	if services.ChannelManager != nil {
		services.ChannelManager.StopAll(shutdownCtx)
	}
	if services.DeviceService != nil {
		services.DeviceService.Stop()
	}
	if services.HeartbeatService != nil {
		services.HeartbeatService.Stop()
	}
	if services.CronService != nil {
		services.CronService.Stop()
	}
	if services.MediaStore != nil {
		// Stop the media store if it's a FileMediaStore with cleanup
		if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
	}
}

// shutdownGateway performs a complete gateway shutdown
func shutdownGateway(
	services *gatewayServices,
	agentLoop *agent.AgentLoop,
	provider providers.LLMProvider,
	fullShutdown bool,
) {
	if cp, ok := provider.(providers.StatefulProvider); ok && fullShutdown {
		cp.Close()
	}

	stopAndCleanupServices(services, gracefulShutdownTimeout)

	// Stop sandbox server after all services are stopped
	if services.SandboxServer != nil {
		services.SandboxServer.Stop()
		// Explicitly clear global state on disable
		sandbox.SetGlobalState(false, "")
		logger.Debugf("sandbox: stopped and state cleared")
	}

	agentLoop.Stop()
	agentLoop.Close()

	logger.Info("✓ Gateway stopped")
}

// handleConfigReload handles config file reload by stopping all services,
// reloading the provider and config, and restarting services with the new config.
func handleConfigReload(
	ctx context.Context,
	al *agent.AgentLoop,
	newCfg *config.Config,
	providerRef *providers.LLMProvider,
	services *gatewayServices,
	msgBus *bus.MessageBus,
) error {
	logger.Info("🔄 Config file changed, reloading...")

	newModel := newCfg.Agents.Defaults.ModelName
	if newModel == "" {
		newModel = newCfg.Agents.Defaults.Model
	}

	logger.Infof(" New model is '%s', recreating provider...", newModel)

	// Stop all services before reloading
	logger.Info("  Stopping all services...")
	stopAndCleanupServices(services, serviceShutdownTimeout)

	// Create new provider from updated config first to ensure validity
	// This will use the correct API key and settings from newCfg.ModelList
	newProvider, newModelID, err := providers.CreateProvider(newCfg)
	if err != nil {
		logger.Errorf("  ⚠ Error creating new provider: %v", err)
		logger.Warn("  Attempting to restart services with old provider and config...")
		// Try to restart services with old configuration
		if restartErr := restartServices(al, services, msgBus); restartErr != nil {
			logger.Errorf("  ⚠ Failed to restart services: %v", restartErr)
		}
		return fmt.Errorf("error creating new provider: %w", err)
	}

	if newModelID != "" {
		newCfg.Agents.Defaults.ModelName = newModelID
	}

	// Use the atomic reload method on AgentLoop to safely swap provider and config.
	// This handles locking internally to prevent races with in-flight LLM calls
	// and concurrent reads of registry/config while the swap occurs.
	reloadCtx, reloadCancel := context.WithTimeout(context.Background(), providerReloadTimeout)
	defer reloadCancel()

	if err := al.ReloadProviderAndConfig(reloadCtx, newProvider, newCfg); err != nil {
		logger.Errorf("  ⚠ Error reloading agent loop: %v", err)
		// Close the newly created provider since it wasn't adopted
		if cp, ok := newProvider.(providers.StatefulProvider); ok {
			cp.Close()
		}
		logger.Warn("  Attempting to restart services with old provider and config...")
		if restartErr := restartServices(al, services, msgBus); restartErr != nil {
			logger.Errorf("  ⚠ Failed to restart services: %v", restartErr)
		}
		return fmt.Errorf("error reloading agent loop: %w", err)
	}

	// Update local provider reference only after successful atomic reload
	*providerRef = newProvider

	// Drop cached exchange clients so edited credentials, regions and
	// proxies take effect with the reload instead of only after a restart:
	// exchanges.CreateExchangeForAccount caches by (exchange, account), so
	// a client built from the previous config would otherwise outlive it
	// for the whole process lifetime. The web launcher does the same after
	// its own config saves (web/backend/api/config.go).
	exchanges.ResetInstanceCache()

	// Handle sandbox enable/disable toggle
	wasEnabled := services.SandboxServer != nil
	isEnabled := newCfg.Debug.Sandbox.Enabled

	if wasEnabled && !isEnabled {
		// Disable: stop the server and clear state
		logger.Info("  Disabling sandbox...")
		services.SandboxServer.Stop()
		sandbox.SetGlobalState(false, "")
		services.SandboxServer = nil
		services.SandboxToken = ""
		logger.Debugf("sandbox: disabled and state cleared")
	} else if !wasEnabled && isEnabled {
		// Enable: arm state (already done by config reload watcher, but arm again)
		// and prepare to start server in restartServices
		logger.Info("  Enabling sandbox...")
		sandbox.SetGlobalState(true, "")
		logger.Debugf("sandbox: state armed for restart")
	} else if wasEnabled && isEnabled {
		// Sandbox stays enabled: stop the old server so a fresh one can start in restartServices
		logger.Info("  Reloading sandbox configuration...")
		services.SandboxServer.Stop()
		sandbox.SetGlobalState(false, "")
		services.SandboxServer = nil
		services.SandboxToken = ""
		logger.Debugf("sandbox: old server stopped, will restart with new config")
	}

	// Restart all services with new config
	logger.Info("  Restarting all services with new configuration...")
	if err := restartServices(al, services, msgBus); err != nil {
		logger.Errorf("  ⚠ Error restarting services: %v", err)
		return fmt.Errorf("error restarting services: %w", err)
	}

	logger.Info("  ✓ Provider, configuration, and services reloaded successfully (thread-safe)")
	return nil
}

// restartServices restarts all services after a config reload
func restartServices(
	al *agent.AgentLoop,
	services *gatewayServices,
	msgBus *bus.MessageBus,
) error {
	// Create an independent context with timeout for service restart operations
	// (cron, heartbeat, device startup). This is intentionally separate from the
	// context passed to StartAll below, which must outlive this function.
	ctx, cancel := context.WithTimeout(context.Background(), serviceRestartTimeout)
	defer cancel()

	// Get current config from agent loop (which has been updated if this is a reload)
	cfg := al.GetConfig()

	// Re-create and start cron service with new config
	execTimeout := time.Duration(cfg.Tools.Cron.ExecTimeoutMinutes) * time.Minute
	var restartDNStore *deltaneutral.Store
	services.CronService, restartDNStore = setupCronTool(
		al,
		msgBus,
		cfg.WorkspacePath(),
		cfg.Agents.Defaults.RestrictToWorkspace,
		execTimeout,
		cfg,
	)
	if err := services.CronService.Start(); err != nil {
		return fmt.Errorf("error restarting cron service: %w", err)
	}
	fmt.Println("  ✓ Cron service restarted")

	// Re-create and start heartbeat service with new config
	services.HeartbeatService = heartbeat.NewHeartbeatService(
		cfg.WorkspacePath(),
		cfg.Heartbeat.Interval,
		cfg.Heartbeat.Enabled,
	)
	services.HeartbeatService.SetBus(msgBus)
	services.HeartbeatService.SetHandler(func(prompt, channel, chatID string) *tools.ToolResult {
		if channel == "" || chatID == "" {
			channel, chatID = "cli", "direct"
		}
		var response string
		var err error
		response, err = al.ProcessHeartbeat(context.Background(), prompt, channel, chatID)
		if err != nil {
			return tools.ErrorResult(fmt.Sprintf("Heartbeat error: %v", err))
		}
		if response == "HEARTBEAT_OK" {
			return tools.SilentResult("Heartbeat OK")
		}
		return tools.SilentResult(response)
	})
	if err := services.HeartbeatService.Start(); err != nil {
		return fmt.Errorf("error restarting heartbeat service: %w", err)
	}
	fmt.Println("  ✓ Heartbeat service restarted")

	// Stop the old media store before creating a new one
	if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
		fms.Stop()
	}

	// Re-create media store with new config
	services.MediaStore = media.NewFileMediaStoreWithCleanup(media.MediaCleanerConfig{
		Enabled:  cfg.Tools.MediaCleanup.Enabled,
		MaxAge:   time.Duration(cfg.Tools.MediaCleanup.MaxAge) * time.Minute,
		Interval: time.Duration(cfg.Tools.MediaCleanup.Interval) * time.Minute,
	})
	// Start the media store if it's a FileMediaStore with cleanup
	if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
		fms.Start()
	}
	al.SetMediaStore(services.MediaStore)

	// Re-create channel manager with new config
	var err error
	services.ChannelManager, err = channels.NewManager(cfg, msgBus, services.MediaStore)
	if err != nil {
		// Stop the media store if it's a FileMediaStore with cleanup
		if fms, ok := services.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
		return fmt.Errorf("error recreating channel manager: %w", err)
	}
	al.SetChannelManager(services.ChannelManager)

	enabledChannels := services.ChannelManager.GetEnabledChannels()
	if len(enabledChannels) > 0 {
		fmt.Printf("  ✓ Channels enabled: %s\n", enabledChannels)
	} else {
		fmt.Println("  ⚠ Warning: No channels enabled")
	}

	// Setup HTTP server with new config
	addr := fmt.Sprintf("%s:%d", cfg.Gateway.Host, cfg.Gateway.Port)
	services.HealthServer = health.NewServer(cfg.Gateway.Host, cfg.Gateway.Port)
	services.ChannelManager.SetupHTTPServer(addr, services.HealthServer)
	registerCronAPI(services.ChannelManager, services.CronService, restartDNStore)
	if cfg.Debug.DevMCP.Enabled {
		registerDevMCP(cfg, services, al)
	} else {
		teardownDevMCP(services, al)
	}
	if cfg.Debug.Sandbox.Enabled {
		if err := startAndRegisterSandbox(cfg, services); err != nil {
			logger.Errorf("sandbox: failed to restart: %v", err)
		}
	}

	// Use context.Background() so channel goroutines (e.g. pico WebSocket readLoops)
	// are not cancelled when this function returns. Channels are stopped explicitly
	// via StopAll() on the next reload or shutdown.
	if err := services.ChannelManager.StartAll(context.Background()); err != nil {
		return fmt.Errorf("error restarting channels: %w", err)
	}
	fmt.Printf(
		"  ✓ Channels restarted, health endpoints at http://%s:%d/health and ready\n",
		cfg.Gateway.Host,
		cfg.Gateway.Port,
	)

	// Re-create device service with new config
	stateManager := state.NewManager(cfg.WorkspacePath())
	services.DeviceService = devices.NewService(devices.Config{
		Enabled:    cfg.Devices.Enabled,
		MonitorUSB: cfg.Devices.MonitorUSB,
	}, stateManager)
	services.DeviceService.SetBus(msgBus)
	if err := services.DeviceService.Start(ctx); err != nil {
		logger.WarnCF("device", "Failed to restart device service", map[string]any{"error": err.Error()})
	} else if cfg.Devices.Enabled {
		fmt.Println("  ✓ Device event service restarted")
	}

	// Wire up voice transcription with new config
	transcriber := voice.DetectTranscriber(cfg)
	al.SetTranscriber(transcriber) // This will set it to nil if disabled
	if transcriber != nil {
		logger.InfoCF("voice", "Transcription re-enabled (agent-level)", map[string]any{"provider": transcriber.Name()})
	} else {
		logger.InfoCF("voice", "Transcription disabled", nil)
	}

	return nil
}

// startAndRegisterSandbox initializes and starts the sandbox server, then registers
// its API endpoints on the channel manager's mux. Must be called after the mux is
// set up and before channels start.
func startAndRegisterSandbox(cfg *config.Config, services *gatewayServices) error {
	// Stop any server left over from a previous start. handleConfigReload's
	// recovery paths call restartServices directly when the new provider or the
	// agent loop fails to load, bypassing the enable/disable toggle that would
	// otherwise stop the old server — without this, every failed reload strands
	// another listener for the lifetime of the process.
	if services.SandboxServer != nil {
		services.SandboxServer.Stop()
		services.SandboxServer = nil
		services.SimulatorReseter = nil
	}

	// Resolve fixtures directory
	fixturesDir := sandbox.ResolveFixturesDir(cfg)

	// Create store and load fixtures
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir); err != nil {
		return fmt.Errorf("load fixtures: %w", err)
	}

	// A missing or empty fixtures directory is not an error (Store.Load treats it
	// as "nothing to serve"), but it leaves the simulator with no markets, mark
	// prices or balances, so every request fails at runtime. Say so up front
	// instead of printing the "sandbox enabled" banner and failing later.
	if len(store.Venues()) == 0 {
		logger.WarnCF("sandbox",
			"No fixtures found; the simulator has no markets or balances and every exchange call will fail",
			map[string]any{"fixtures_dir": fixturesDir})
	}

	// Create and start server
	srv := sandbox.NewServer(store)
	if err := srv.Start(context.Background()); err != nil {
		return fmt.Errorf("start server: %w", err)
	}

	// Create stateful simulator and wire it into the server.
	// The simulator will be consulted before the fixture store for write endpoints.
	stateManager := sandbox.NewStateManager()

	// Seed Markets from fixtures so the simulator can handle order requests
	// (contractSize and minAmount are needed for order validation)
	for _, venue := range store.Venues() {
		venueState := stateManager.GetState(venue)
		sandbox.SeedMarketsFromFixtures(venue, store, venueState)

		// Seed dev-mode default values for MarkPrices and Balances
		// These are required for the simulator to accept orders
		// Values are arbitrary dev defaults; easily changed
		for symbol := range venueState.Markets {
			venueState.MarkPrices[symbol] = 50000 // Default price for all symbols
		}
		venueState.Balances["USDT"] = sandbox.Balance{Free: 100000, Locked: 0}

		// Capture the seeded state as the reset baseline. Without this, calling the reset
		// tool will wipe all simulator state and leave it inert until process restart.
		stateManager.SnapshotAsSeed(venue)
	}

	sim := sandbox.NewStatefulSimulator(stateManager)
	srv.SetResponder(sim)

	// Store references
	services.SandboxServer = srv
	services.SimulatorReseter = &sandboxStateReseter{sm: stateManager, store: store}
	sandbox.SetInstance(srv)

	// Generate or reuse bearer token for API access (persist in config like DevMCP)
	if cfg.Debug.Sandbox.Token == "" {
		cfg.Debug.Sandbox.Token = generateDevMCPToken()
		if err := config.SaveConfig(internal.GetConfigPath(), cfg); err != nil {
			logger.WarnCF("sandbox", "Failed to persist sandbox token to config", map[string]any{"err": err.Error()})
		}
	}
	services.SandboxToken = cfg.Debug.Sandbox.Token

	// Register API routes (pass store separately since Server doesn't expose it)
	registerSandboxAPI(services.ChannelManager, store, services.SandboxToken, services.SimulatorReseter, fixturesDir)

	baseURL := srv.BaseURL()
	fmt.Printf("🏜️  Sandbox: %s\n   Token:    %s\n", baseURL, services.SandboxToken)
	logger.WarnCF("sandbox",
		fmt.Sprintf("Developer sandbox mode enabled at %s — disable in production", baseURL),
		nil)

	return nil
}

// setupConfigWatcherPolling sets up a simple polling-based config file watcher
// Returns a channel for config updates and a stop function
func setupConfigWatcherPolling(configPath string, debug bool) (chan *config.Config, func()) {
	configChan := make(chan *config.Config, 1)
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		// Get initial file info
		lastModTime := getFileModTime(configPath)
		lastSize := getFileSize(configPath)

		ticker := time.NewTicker(2 * time.Second) // Check every 2 seconds
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				currentModTime := getFileModTime(configPath)
				currentSize := getFileSize(configPath)

				// Check if file changed (modification time or size changed)
				if currentModTime.After(lastModTime) || currentSize != lastSize {
					if debug {
						logger.Debugf("🔍 Config file change detected")
					}

					// Debounce - wait a bit to ensure file write is complete
					time.Sleep(500 * time.Millisecond)

					// Validate and load new config
					newCfg, err := config.LoadConfig(configPath)
					if err != nil {
						logger.Errorf("⚠ Error loading new config: %v", err)
						logger.Warn("  Using previous valid config")
						continue
					}

					// Validate the new config
					if err := newCfg.ValidateModelList(); err != nil {
						logger.Errorf("  ⚠ New config validation failed: %v", err)
						logger.Warn("  Using previous valid config")
						continue
					}

					logger.Info("✓ Config file validated and loaded")

					// Update last known state
					lastModTime = currentModTime
					lastSize = currentSize

					// Send new config to main loop (non-blocking)
					select {
					case configChan <- newCfg:
					default:
						// Channel full, skip this update
						logger.Warn("⚠ Previous config reload still in progress, skipping")
					}
				}

			case <-stop:
				return
			}
		}
	}()

	stopFunc := func() {
		close(stop)
		wg.Wait()
	}

	return configChan, stopFunc
}

// getFileModTime returns the modification time of a file, or zero time if file doesn't exist
func getFileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// getFileSize returns the size of a file, or 0 if file doesn't exist
func getFileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func setupCronTool(
	agentLoop *agent.AgentLoop,
	msgBus *bus.MessageBus,
	workspace string,
	restrict bool,
	execTimeout time.Duration,
	cfg *config.Config,
) (*cron.CronService, *deltaneutral.Store) {
	cronStorePath := filepath.Join(workspace, "cron", "jobs.json")

	// Create cron service
	cronService := cron.NewCronService(cronStorePath, nil)

	// Create and register CronTool if enabled
	var cronTool *tools.CronTool
	if cfg.Tools.IsToolEnabled("cron") {
		var err error
		cronTool, err = tools.NewCronTool(cronService, agentLoop, msgBus, workspace, restrict, execTimeout, cfg)
		if err != nil {
			logger.Fatalf("Critical error during CronTool initialization: %v", err)
		}

		agentLoop.RegisterTool(cronTool)
	}

	// DCA store — opened unconditionally so the cron trigger gate works even when
	// individual DCA tools are disabled (e.g. legacy dca:* jobs still in jobs.json).
	var dcaStore *dca.Store
	if store, storeErr := dca.NewStore(workspace); storeErr != nil {
		logger.ErrorCF("gateway", "Failed to open DCA store; DCA cron gate and tools disabled",
			map[string]any{"error": storeErr.Error()})
	} else {
		dcaStore = store
	}

	// Delta-neutral store — opened unconditionally so the cron trigger gate works even when
	// individual DN tools are disabled (e.g. legacy dn:* jobs still in jobs.json).
	var dnStore *deltaneutral.Store
	if store, storeErr := deltaneutral.NewStore(workspace); storeErr != nil {
		logger.ErrorCF("gateway", "Failed to open delta-neutral store; DN cron gate and tools disabled",
			map[string]any{"error": storeErr.Error()})
	} else {
		dnStore = store
	}

	// Set onJob handler — alert, DCA, and DN jobs are handled directly in code;
	// all other jobs are routed through the agent LLM via cronTool.
	cronService.SetOnJob(func(job *cron.CronJob) (string, error) {
		if strings.HasPrefix(job.Name, "price_alert:") {
			return handlePriceAlertJob(context.Background(), job, cfg, cronService, msgBus)
		}
		if strings.HasPrefix(job.Name, "indicator_alert:") {
			return handleIndicatorAlertJob(context.Background(), job, cfg, cronService, msgBus)
		}
		if strings.HasPrefix(job.Name, "dca:") && dcaStore != nil {
			return handleDCAAutoJob(context.Background(), job, cfg, dcaStore, cronTool)
		}
		if strings.HasPrefix(job.Name, "dn:") && dnStore != nil {
			return handleDeltaNeutralMonitorJob(context.Background(), job, cfg, dnStore, cronTool, msgBus)
		}
		if cronTool != nil {
			return cronTool.ExecuteJob(context.Background(), job), nil
		}
		return "", fmt.Errorf("no executor configured for job %q", job.Name)
	})

	// Alert tools (Track D) — require cron service, registered after it is created.
	if cfg.Tools.IsToolEnabled("set_price_alert") {
		agentLoop.RegisterTool(tools.NewSetPriceAlertTool(cfg, cronService))
	}
	if cfg.Tools.IsToolEnabled("set_indicator_alert") {
		agentLoop.RegisterTool(tools.NewSetIndicatorAlertTool(cfg, cronService))
	}

	// DCA tools (Track E) — require cron service + the store opened above.
	dcaEnabled := cfg.Tools.IsToolEnabled("create_dca_plan") ||
		cfg.Tools.IsToolEnabled("list_dca_plans") ||
		cfg.Tools.IsToolEnabled("update_dca_plan") ||
		cfg.Tools.IsToolEnabled("delete_dca_plan") ||
		cfg.Tools.IsToolEnabled("execute_dca_order") ||
		cfg.Tools.IsToolEnabled("get_dca_history") ||
		cfg.Tools.IsToolEnabled("get_dca_summary")
	if dcaEnabled && dcaStore != nil {
		if cfg.Tools.IsToolEnabled("create_dca_plan") {
			agentLoop.RegisterTool(tools.NewCreateDCAPlanTool(cfg, dcaStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("list_dca_plans") {
			agentLoop.RegisterTool(tools.NewListDCAPlansTool(dcaStore))
		}
		if cfg.Tools.IsToolEnabled("update_dca_plan") {
			agentLoop.RegisterTool(tools.NewUpdateDCAPlanTool(dcaStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("delete_dca_plan") {
			agentLoop.RegisterTool(tools.NewDeleteDCAPlanTool(dcaStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("execute_dca_order") {
			agentLoop.RegisterTool(tools.NewExecuteDCAOrderTool(cfg, dcaStore))
		}
		if cfg.Tools.IsToolEnabled("get_dca_history") {
			agentLoop.RegisterTool(tools.NewGetDCAHistoryTool(dcaStore))
		}
		if cfg.Tools.IsToolEnabled("get_dca_summary") {
			agentLoop.RegisterTool(tools.NewGetDCASummaryTool(cfg, dcaStore))
		}
	}

	// Delta-neutral tools (Track F) — require cron service + the store opened above.
	dnEnabled := cfg.Tools.IsToolEnabled("create_delta_neutral_plan") ||
		cfg.Tools.IsToolEnabled("list_delta_neutral_plans") ||
		cfg.Tools.IsToolEnabled("get_delta_neutral_plan") ||
		cfg.Tools.IsToolEnabled("update_delta_neutral_plan") ||
		cfg.Tools.IsToolEnabled("delete_delta_neutral_plan") ||
		cfg.Tools.IsToolEnabled("get_delta_neutral_summary") ||
		cfg.Tools.IsToolEnabled("get_delta_neutral_history") ||
		cfg.Tools.IsToolEnabled("prepare_delta_neutral_plan") ||
		cfg.Tools.IsToolEnabled("open_delta_neutral_position") ||
		cfg.Tools.IsToolEnabled("unwind_delta_neutral_position") ||
		cfg.Tools.IsToolEnabled("render_delta_neutral_yield_chart")
	if dnEnabled && dnStore != nil {
		if cfg.Tools.IsToolEnabled("create_delta_neutral_plan") {
			agentLoop.RegisterTool(tools.NewCreateDeltaNeutralPlanTool(cfg, dnStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("list_delta_neutral_plans") {
			agentLoop.RegisterTool(tools.NewListDeltaNeutralPlansTool(dnStore))
		}
		if cfg.Tools.IsToolEnabled("get_delta_neutral_plan") {
			agentLoop.RegisterTool(tools.NewGetDeltaNeutralPlanTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("update_delta_neutral_plan") {
			agentLoop.RegisterTool(tools.NewUpdateDeltaNeutralPlanTool(cfg, dnStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("delete_delta_neutral_plan") {
			agentLoop.RegisterTool(tools.NewDeleteDeltaNeutralPlanTool(dnStore, cronService))
		}
		if cfg.Tools.IsToolEnabled("get_delta_neutral_summary") {
			agentLoop.RegisterTool(tools.NewGetDeltaNeutralSummaryTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("get_delta_neutral_history") {
			agentLoop.RegisterTool(tools.NewGetDeltaNeutralHistoryTool(dnStore))
		}
		if cfg.Tools.IsToolEnabled("prepare_delta_neutral_plan") {
			agentLoop.RegisterTool(tools.NewPrepareDeltaNeutralPlanTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("open_delta_neutral_position") {
			agentLoop.RegisterTool(tools.NewOpenDeltaNeutralPositionTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("unwind_delta_neutral_position") {
			agentLoop.RegisterTool(tools.NewUnwindDeltaNeutralPositionTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("resize_delta_neutral_position") {
			agentLoop.RegisterTool(tools.NewResizeDeltaNeutralPositionTool(cfg, dnStore))
		}
		if cfg.Tools.IsToolEnabled("render_delta_neutral_yield_chart") {
			agentLoop.RegisterTool(tools.NewRenderDeltaNeutralYieldChartTool(dnStore))
		}
	}

	return cronService, dnStore
}

// registerDevMCP wires the read-only developer MCP server onto the shared
// gateway HTTP mux. Only called when cfg.Debug.DevMCP.Enabled is true.
// Must be called after SetupHTTPServer has created the mux on ChannelManager.
// The mux is recreated on every reload, so the route is re-registered here each time.
func registerDevMCP(cfg *config.Config, services *gatewayServices, al *agent.AgentLoop) {
	// Auto-generate a token if not already configured, then persist it so the
	// WebUI status endpoint and subsequent restarts can read the same token.
	if cfg.Debug.DevMCP.Token == "" {
		cfg.Debug.DevMCP.Token = generateDevMCPToken()
		if err := config.SaveConfig(internal.GetConfigPath(), cfg); err != nil {
			logger.WarnCF("devmcp", "Failed to persist dev-mcp token to config", map[string]any{"err": err.Error()})
		}
	}

	store := debugtap.NewStore(cfg.Debug.DevMCP.MaxLogEntries)
	services.DebugTap = store
	al.SetDebugTap(store)

	// Reuse the existing log buffer across reloads so history isn't lost.
	if services.LogBuf == nil {
		services.LogBuf = debugtap.NewLogBuffer(2000)
		logger.SetAdditionalWriter(services.LogBuf)
	}

	handler := devmcp.NewHTTPHandler(devmcp.Deps{
		Loop:     al,
		DebugTap: store,
		LogBuf:   services.LogBuf,
		Cfg:      cfg,
	})

	prefix := cfg.Debug.DevMCP.PathPrefix
	endpoint := fmt.Sprintf("http://%s:%d%s", cfg.Gateway.Host, cfg.Gateway.Port, prefix)
	guarded := loopbackOnly(bearerTokenMiddleware(cfg.Debug.DevMCP.Token,
		http.StripPrefix(prefix, handler)))

	services.ChannelManager.Handle(prefix, guarded)
	services.ChannelManager.Handle(prefix+"/", guarded)

	fmt.Printf("🔌 Dev MCP: %s\n   Token:    %s\n", endpoint, cfg.Debug.DevMCP.Token)
	logger.WarnCF("devmcp",
		fmt.Sprintf("Developer MCP server enabled at %s — disable in production", endpoint),
		nil)
}

// teardownDevMCP cleans up dev-MCP state when the flag is turned off.
// It is a no-op when dev-MCP was never enabled (all fields are nil).
func teardownDevMCP(services *gatewayServices, al *agent.AgentLoop) {
	if services.DebugTap != nil {
		al.SetDebugTap(nil)
		services.DebugTap = nil
	}
	if services.LogBuf != nil {
		logger.SetAdditionalWriter(nil)
		services.LogBuf = nil
	}
}
