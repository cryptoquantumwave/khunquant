package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/debugtap"
	"github.com/cryptoquantumwave/khunquant/pkg/devices"
	"github.com/cryptoquantumwave/khunquant/pkg/health"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/voice"
)

// This file is the one place the gateway's services are put together. The
// first start and every config reload go through buildServices, so a service
// or HTTP route added here exists in both; there is no second copy to forget.
//
// To add a service: a field on gatewayServices, its construction in
// buildServices, its shutdown in stopServices. To add an HTTP route: one line
// in registerGatewayRoutes.

// Seams for tests. Production never reassigns them.
var (
	// createProvider builds the LLM provider for a reloaded config.
	createProvider = providers.CreateProvider
	// lossGuardStarter applies the loss-day zone and starts the equity sampler.
	lossGuardStarter = startLossGuard
)

// handleMux is the part of the gateway's HTTP mux the route registrars need.
// *channels.Manager satisfies it.
type handleMux interface {
	Handle(pattern string, handler http.Handler)
}

// sharedResources holds what lives for the whole gateway process, across every
// config reload. Rule: reload must not reopen what lives here. It is created
// once in gatewayCmd, handed to every buildServices call, and closed only at
// shutdown. Anything that must restart with new config (the loss-guard sampler,
// the cron service, the channel manager, ...) belongs on gatewayServices instead.
//
// Later changes add a database handle and an event bus: register each with
// add so Close releases it.
type sharedResources struct {
	// LogBuf is the dev-MCP log ring buffer. It is created the first time
	// dev-MCP is enabled, kept across reloads so history survives them, and
	// dropped (and the logger hook removed) when dev-MCP is switched off. It is
	// touched only by the single goroutine that builds services.
	LogBuf *debugtap.LogBuffer

	mu      sync.Mutex
	closers []namedCloser
	closed  bool
}

type namedCloser struct {
	name string
	c    io.Closer
}

func newSharedResources() *sharedResources {
	return &sharedResources{}
}

// add registers a resource for Close. Resources close in reverse order of
// registration, so a resource may depend on the ones added before it.
func (r *sharedResources) add(name string, c io.Closer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closers = append(r.closers, namedCloser{name: name, c: c})
}

// Close releases every registered resource, newest first, and reports all
// failures. Only the first call does anything.
func (r *sharedResources) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	closers := r.closers
	r.closers = nil
	r.mu.Unlock()

	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i].c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close %s: %w", closers[i].name, err))
		}
	}
	return errors.Join(errs...)
}

// assemblyDeps is everything buildServices needs. The same struct describes a
// first start and a reload.
type assemblyDeps struct {
	Cfg       *config.Config
	AgentLoop *agent.AgentLoop
	Bus       *bus.MessageBus
	// State is the agent loop's state manager, shared with the heartbeat and
	// device services so all of them see the last channel as it is now.
	State  *state.Manager
	Shared *sharedResources

	// Prev is the previous generation of services on a reload, nil at start. It
	// carries only what must outlive a rebuild: the running sandbox server
	// (so a failed reload cannot strand its listener) and the dev-MCP debug tap.
	Prev *gatewayServices

	// Reload selects wording of console and log lines ("restarted" vs "started").
	// It must never change what is built.
	Reload bool
}

// phase picks the wording of progress lines for a first start or a reload.
type phase struct{ reload bool }

func (p phase) pick(start, reload string) string {
	if p.reload {
		return reload
	}
	return start
}

// sayf prints a console progress line, indented on reload as it always was.
func (p phase) sayf(format string, args ...any) {
	fmt.Print(p.pick("", "  ") + fmt.Sprintf(format, args...) + "\n")
}

// buildServices constructs and starts every gateway service for cfg, in this
// order: loss guard, cron, heartbeat, media store, channel manager (and agent
// loop wiring, voice), HTTP server, routes, channels, notify, devices.
//
// On error it returns the services that were already started, not nil, so the
// caller can keep tracking and later stop them.
//
// ctx only bounds construction. Long-lived services (channels, devices) run on
// context.Background() on purpose: they must outlive this call.
func buildServices(ctx context.Context, d assemblyDeps) (*gatewayServices, error) {
	s := &gatewayServices{}
	if prev := d.Prev; prev != nil {
		s.SandboxServer = prev.SandboxServer
		s.SandboxToken = prev.SandboxToken
		s.SimulatorReseter = prev.SimulatorReseter
		s.DebugTap = prev.DebugTap
	}
	if err := ctx.Err(); err != nil {
		return s, fmt.Errorf("service assembly aborted: %w", err)
	}

	cfg, al, msgBus := d.Cfg, d.AgentLoop, d.Bus
	p := phase{reload: d.Reload}
	starting := p.pick("starting", "restarting")

	// The shared proactive database: opened once per process, closed at
	// shutdown only. A failure here is not fatal; the services that need it
	// report their own absence.
	if _, err := proactiveDB(d); err != nil {
		logger.ErrorCF("gateway", "Shared proactive database unavailable",
			map[string]any{"error": err.Error()})
	}

	s.LossGuard = lossGuardStarter(cfg)

	// Cron tool and service.
	execTimeout := time.Duration(cfg.Tools.Cron.ExecTimeoutMinutes) * time.Minute
	s.CronService, s.DNStore = setupCronTool(
		al,
		msgBus,
		cfg.WorkspacePath(),
		cfg.Agents.Defaults.RestrictToWorkspace,
		execTimeout,
		cfg,
	)
	if err := s.CronService.Start(); err != nil {
		return s, fmt.Errorf("error %s cron service: %w", starting, err)
	}
	p.sayf("✓ Cron service %s", p.pick("started", "restarted"))

	// Heartbeat.
	s.HeartbeatService = heartbeat.NewHeartbeatService(
		cfg.WorkspacePath(),
		cfg.Heartbeat.Interval,
		cfg.Heartbeat.Enabled,
		d.State,
	)
	s.HeartbeatService.SetBus(msgBus)
	s.HeartbeatService.SetHandler(heartbeatHandler(al))
	if err := s.HeartbeatService.Start(); err != nil {
		return s, fmt.Errorf("error %s heartbeat service: %w", starting, err)
	}
	p.sayf("✓ Heartbeat service %s", p.pick("started", "restarted"))

	// Media store for file lifecycle management with TTL cleanup.
	s.MediaStore = media.NewFileMediaStoreWithCleanup(media.MediaCleanerConfig{
		Enabled:  cfg.Tools.MediaCleanup.Enabled,
		MaxAge:   time.Duration(cfg.Tools.MediaCleanup.MaxAge) * time.Minute,
		Interval: time.Duration(cfg.Tools.MediaCleanup.Interval) * time.Minute,
	})
	if fms, ok := s.MediaStore.(*media.FileMediaStore); ok {
		fms.Start()
	}

	// Channel manager.
	var err error
	s.ChannelManager, err = channels.NewManager(cfg, msgBus, s.MediaStore)
	if err != nil {
		if fms, ok := s.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
		return s, fmt.Errorf("error %s channel manager: %w", p.pick("creating", "recreating"), err)
	}

	// Inject channel manager and media store into the agent loop.
	al.SetChannelManager(s.ChannelManager)
	al.SetMediaStore(s.MediaStore)

	// Voice transcription follows the config: nil clears a transcriber that a
	// reload has just disabled.
	transcriber := voice.DetectTranscriber(cfg)
	al.SetTranscriber(transcriber)
	switch {
	case transcriber != nil:
		logger.InfoCF("voice", p.pick("Transcription enabled (agent-level)", "Transcription re-enabled (agent-level)"),
			map[string]any{"provider": transcriber.Name()})
	case d.Reload:
		logger.InfoCF("voice", "Transcription disabled", nil)
	}

	if enabled := s.ChannelManager.GetEnabledChannels(); len(enabled) > 0 {
		p.sayf("✓ Channels enabled: %s", fmt.Sprint(enabled))
	} else {
		p.sayf("⚠ Warning: No channels enabled")
	}

	// Shared HTTP server with health endpoints and webhook handlers. Routes go
	// on after this and before StartAll, in registerGatewayRoutes only.
	addr := fmt.Sprintf("%s:%d", cfg.Gateway.Host, cfg.Gateway.Port)
	s.HealthServer = health.NewServer(cfg.Gateway.Host, cfg.Gateway.Port)
	s.ChannelManager.SetupHTTPServer(addr, s.HealthServer)
	registerGatewayRoutes(s.ChannelManager, s, d)

	// context.Background(), not ctx: channel goroutines (e.g. pico WebSocket
	// readLoops) must not be cancelled when this function returns. Channels are
	// stopped explicitly via StopAll on the next reload or at shutdown.
	if err := s.ChannelManager.StartAll(context.Background()); err != nil {
		return s, fmt.Errorf("error %s channels: %w", starting, err)
	}
	if d.Reload {
		fmt.Printf(
			"  ✓ Channels restarted, health endpoints at http://%s:%d/health and ready\n",
			cfg.Gateway.Host, cfg.Gateway.Port)
	} else {
		fmt.Printf("✓ Health endpoints available at http://%s:%d/health and /ready\n",
			cfg.Gateway.Host, cfg.Gateway.Port)
	}

	// Owner delivery. It needs the channel manager (to send) and the agent
	// loop (to write the message into the owner's session), so it comes after
	// both. A failure to build it leaves delivery disabled and the gateway up.
	s.NotifyService = buildNotifyService(d, s.ChannelManager)

	// Device service, on the shared state manager.
	s.DeviceService = devices.NewService(devices.Config{
		Enabled:    cfg.Devices.Enabled,
		MonitorUSB: cfg.Devices.MonitorUSB,
	}, d.State)
	s.DeviceService.SetBus(msgBus)
	if s.NotifyService != nil {
		s.DeviceService.SetNotifier(deviceNotifier{svc: s.NotifyService})
	}
	if err := s.DeviceService.Start(context.Background()); err != nil {
		if d.Reload {
			logger.WarnCF("device", "Failed to restart device service", map[string]any{"error": err.Error()})
		} else {
			logger.ErrorCF("device", "Error starting device service", map[string]any{"error": err.Error()})
		}
	} else if cfg.Devices.Enabled {
		p.sayf("✓ Device event service %s", p.pick("started", "restarted"))
	}

	return s, nil
}

// heartbeatHandler runs a heartbeat prompt through the agent loop.
func heartbeatHandler(al *agent.AgentLoop) heartbeat.HeartbeatHandler {
	return func(prompt, channel, chatID string) *tools.ToolResult {
		// Use cli:direct as fallback if no valid channel
		if channel == "" || chatID == "" {
			channel, chatID = "cli", "direct"
		}
		// Use ProcessHeartbeat - no session history, each heartbeat is independent
		response, err := al.ProcessHeartbeat(context.Background(), prompt, channel, chatID)
		if err != nil {
			return tools.ErrorResult(fmt.Sprintf("Heartbeat error: %v", err))
		}
		if response == "HEARTBEAT_OK" {
			return tools.SilentResult("Heartbeat OK")
		}
		// For heartbeat, always return silent - the subagent result will be
		// sent to user via processSystemMessage when the async task completes
		return tools.SilentResult(response)
	}
}

// registerGatewayRoutes puts every gateway HTTP route on mux (and starts the
// sandbox server whose API it serves). It is the only place routes are added:
// buildServices calls it right after SetupHTTPServer and before StartAll, on a
// first start and on every reload, because the mux is recreated each time.
func registerGatewayRoutes(mux handleMux, s *gatewayServices, d assemblyDeps) {
	cfg := d.Cfg

	ensureManagementToken(cfg)
	registerCronAPI(mux, s.CronService, s.DNStore, cfg.Gateway.ManagementToken)

	if cfg.Debug.DevMCP.Enabled {
		registerDevMCP(mux, cfg, s, d.Shared, d.AgentLoop)
	} else {
		// A no-op unless dev-MCP was on before this rebuild.
		teardownDevMCP(s, d.Shared, d.AgentLoop)
	}

	if cfg.Debug.Sandbox.Enabled {
		if err := startAndRegisterSandbox(mux, cfg, s); err != nil {
			logger.Errorf("sandbox: failed to %s: %v", phase{reload: d.Reload}.pick("start", "restart"), err)
		}
	}
}

// stopServices stops all services of one generation and cleans up their
// resources. Order: loss guard, channels, devices, notify, heartbeat, cron, media store.
// The sandbox server is deliberately not stopped here; see handleConfigReload
// and shutdownGateway.
func stopServices(s *gatewayServices, shutdownTimeout time.Duration) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	s.LossGuard.Stop()
	s.LossGuard = nil
	if s.ChannelManager != nil {
		s.ChannelManager.StopAll(shutdownCtx)
	}
	if s.DeviceService != nil {
		s.DeviceService.Stop()
	}
	// After the device service, its last caller. Drains queued session writes.
	stopNotifyService(s.NotifyService)
	s.NotifyService = nil
	if s.HeartbeatService != nil {
		s.HeartbeatService.Stop()
	}
	if s.CronService != nil {
		s.CronService.Stop()
	}
	if s.MediaStore != nil {
		// Stop the media store if it's a FileMediaStore with cleanup
		if fms, ok := s.MediaStore.(*media.FileMediaStore); ok {
			fms.Stop()
		}
	}
}

// reloadServices rebuilds the services after stopServices, from the agent
// loop's current config (the new one after a successful reload, the old one on
// the recovery paths). The result replaces *services in place, even when the
// build failed part way, so the next stop still sees whatever did start.
func reloadServices(
	al *agent.AgentLoop,
	services *gatewayServices,
	shared *sharedResources,
	msgBus *bus.MessageBus,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), serviceRestartTimeout)
	defer cancel()

	next, err := buildServices(ctx, assemblyDeps{
		Cfg:       al.GetConfig(),
		AgentLoop: al,
		Bus:       msgBus,
		State:     al.State(),
		Shared:    shared,
		Prev:      services,
		Reload:    true,
	})
	if next != nil {
		*services = *next
	}
	return err
}
