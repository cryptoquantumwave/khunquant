package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/debugtap"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/devices"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/health"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/state"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
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
// The proactive database handle and the event bus live here; register anything
// further with add so Close releases it.
type sharedResources struct {
	// LogBuf is the dev-MCP log ring buffer. It is created the first time
	// dev-MCP is enabled, kept across reloads so history survives them, and
	// dropped (and the logger hook removed) when dev-MCP is switched off. It is
	// touched only by the single goroutine that builds services.
	LogBuf *debugtap.LogBuffer

	// Events is the gateway's one event bus (see event_bus.go). It is made with
	// the resources and closed by Close, never by a reload.
	Events *events.EventBus

	mu      sync.Mutex
	closers []namedCloser
	closed  bool

	// triggerLim is the external trigger route's rate limit, per process: a reload
	// keeps it (see trigger_api.go).
	triggerLim *triggerLimiter

	// dcaStats and dcaOutcomes are the dca_due trigger's counters and per-slot record:
	// per process, so that a reload keeps the day's record (dca_due_wiring.go).
	dcaStats    *triggers.DCAStats
	dcaOutcomes *triggers.DCAOutcomes

	// dcaStart is the workflow engine as the dispatcher sees it (set by the mandated
	// DCA flow, which exists only once the engine does); dcaPromoted maps a plan id to
	// the mandate its dca_due spec is live for (dca_mandate_wiring.go).
	dcaStart    atomic.Pointer[dcaStarterBox]
	dcaPromoted sync.Map
	// dcaMute tells the owner of a buy a mute skipped (set with dcaStart).
	dcaMute atomic.Pointer[dcaMuteNotifier]

	// portfolioStats are the portfolio triggers' counters, per process
	// (portfolio_triggers_wiring.go).
	portfolioStats *triggers.PortfolioStats

	// sweep is the heartbeat sweep's overlap guard, failure count and counters, per
	// process: a sweep still running when a reload builds the next generation is
	// seen by it (sweep_wiring.go). Created on first use, guarded by mu.
	sweep *sweep.State
}

// sweepState returns the process's sweep state, creating it on first use.
func (r *sharedResources) sweepState() *sweep.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sweep == nil {
		r.sweep = &sweep.State{}
	}
	return r.sweep
}

type namedCloser struct {
	name string
	c    io.Closer
}

func newSharedResources() *sharedResources {
	r := &sharedResources{Events: events.NewBus()}
	r.add(eventBusResource, r.Events)
	return r
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

	// Alerts is the alert side of this generation (alerts_wiring.go): buildServices
	// makes it before the tools are registered and sets it on its own copy of the
	// deps; the caller of buildServices never sets it.
	Alerts *alertRuntime

	// DNStore is the delta-neutral plan store (nil when it could not be opened);
	// buildServices sets it on its own copy of the deps once the cron side has opened
	// it, and the caller never sets it.
	DNStore *deltaneutral.Store

	// Approval is the order-approval side of this generation (approval_wiring.go);
	// buildServices sets it on its own copy of the deps before the workflow engine is
	// built, and the caller never sets it.
	Approval *approvalRuntime

	// Control is the control path of this generation (control_wiring.go); buildServices
	// sets it on its own copy of the deps right after the channel manager exists, and
	// the caller never sets it.
	Control *controlRuntime

	// DNFlow is the delta-neutral workflow side (dn_workflows_wiring.go); buildServices
	// sets it on its own copy of the deps before the workflow engine is built.
	DNFlow *dnFlowRuntime

	// DCAFlow is the mandated-DCA side (dca_mandate_wiring.go); buildServices sets it
	// on its own copy of the deps before the workflow engine is built.
	DCAFlow *dcaFlowRuntime

	// Demands is what the market source reads. buildPerception sets it on its own
	// copy of the deps when the triggers registry is built; it is never set by
	// the caller of buildServices.
	Demands perception.DemandSource

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
// order: loss guard, cron, media store, channel manager (and agent loop wiring,
// voice), HTTP server, routes, channels, notify, attention, perception, heartbeat, devices.
//
// On error it returns the services that were already started, not nil, so the
// caller can keep tracking and later stop them.
//
// ctx only bounds construction. Long-lived services (channels, devices) run on
// context.Background() on purpose: they must outlive this call.
func buildServices(ctx context.Context, d assemblyDeps) (*gatewayServices, error) {
	s := &gatewayServices{perceptionLive: newPerceptionHolder(), journalLive: newJournalHolder()}
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

	// Sub-turn events go to the shared event bus, on a first start and on every
	// reload (the agent loop outlives both).
	wireEventBus(d)

	s.LossGuard = lossGuardStarter(cfg)

	// The alert side: the late-bound route from the alert tools to the trigger
	// registry (which does not exist yet), and the migration of the cron alerts.
	s.Alerts = newAlertRuntime(d)
	d.Alerts = s.Alerts

	// Cron tool and service.
	execTimeout := time.Duration(cfg.Tools.Cron.ExecTimeoutMinutes) * time.Minute
	s.CronService, s.DNStore = setupCronTool(
		al,
		msgBus,
		cfg.WorkspacePath(),
		cfg.Agents.Defaults.RestrictToWorkspace,
		execTimeout,
		cfg,
		cronHooks{Alerts: s.Alerts.resolver(), DNSync: s.Alerts.requestDNSync},
	)
	d.DNStore = s.DNStore
	s.Alerts.dn.configure(s.DNStore)
	if err := s.CronService.Start(); err != nil {
		return s, fmt.Errorf("error %s cron service: %w", starting, err)
	}
	p.sayf("✓ Cron service %s", p.pick("started", "restarted"))

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

	// The control path: the kill flag and the dispatcher that sees every message
	// before the agent does. It is installed on the channels now, before they start
	// and before the services it acts on exist (attach adds them below), so that
	// /whoami and the refusals work from the first message. The approval service
	// and the routes need the kill flag, so it comes before both.
	s.Control = buildControl(d, s.ChannelManager)
	d.Control = s.Control

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

	// Attention: the governor and the outbox that turns its decisions into
	// messages. After notify, which it delivers through.
	s.Attention, s.attentionRun = buildAttention(d, s.NotifyService)
	wireDNAudit(s.Attention, d) // the audit rows of delivered delta-neutral alerts

	// Perception: the sources, the manager and the pipeline engine. After
	// attention, whose outbox it decides through and hands over to; nothing is
	// built unless perception is effectively on and that outbox exists.
	s.Perception = buildPerception(d, s.Attention)
	s.perceptionLive.Store(s.Perception)

	// Alerts: with a registry, import and (when live) promote the cron alerts; without
	// one, hand back any job a promotion had switched off. After perception, which
	// builds the registry, and after the cron service started.
	s.Alerts.attach(d, s.CronService, s.Perception)

	// Order approvals: the service is built first, because its workflow definition
	// goes to the engine below, and attached after, because it needs the engine to
	// start workflows. With approvals off none of it exists.
	s.Approval = buildApproval(d, s.NotifyService, s.ChannelManager)
	d.Approval = s.Approval

	// The workflow engine: built only when a definition is registered (the approval
	// one is, while approvals are on), so a gateway without one gains no goroutine
	// and no table.
	// The delta-neutral workflows (dn.open, dn.recover) join the same engine. They
	// ask through the approval service and wake the owner through the outbox, so they
	// come after both.
	s.DNFlow = buildDNFlow(d, s.Attention, s.NotifyService)
	d.DNFlow = s.DNFlow

	// Mandated DCA: after the approval service, which creates mandates and owns the
	// plan store handle. Without it, a plan whose legacy job a mandate switched off
	// gets the job back.
	s.DCAFlow = buildDCAFlow(d, s.NotifyService)
	d.DCAFlow = s.DCAFlow
	if s.DCAFlow == nil && s.CronService != nil {
		restoreDCALegacyJobs(d, s.CronService)
	}

	s.journalLive.Store(buildJournal(d))
	s.Approval.attach(s.journalLive.Load())
	s.DNFlow.attach(s.journalLive.Load())
	if s.CronService != nil {
		s.DCAFlow.attach(d, s.journalLive.Load(), s.Perception, s.CronService)
	}

	// The control path gets the services it acts on, now that they exist. An
	// approval service that could not attach (no engine) is withdrawn from it by
	// the same rule propose_order is: approvals stay unavailable.
	s.Control.attach(d, s.NotifyService, s.Attention, s.Approval)
	s.Control.attachStop(d, s.journalLive.Load(), s.NotifyService)

	// Heartbeat. After attention: a governed heartbeat delivers through the
	// outbox, so the outbox must exist (or its absence be known) before the first
	// run is scheduled. The run time is kept in the shared state manager, so a
	// reload does not start a heartbeat early.
	s.HeartbeatService = heartbeat.NewHeartbeatService(
		cfg.WorkspacePath(),
		cfg.Heartbeat.Interval,
		cfg.Heartbeat.Enabled,
		d.State,
	)
	if contract := heartbeatContract(cfg, s); contract == heartbeat.ContractGoverned {
		// No bus: a governed heartbeat publishes nothing itself.
		s.HeartbeatService.SetContract(contract)
		// The tier and the tool subset are read here, from the config this
		// generation was built from, so a reload that changes
		// proactive.heartbeat.model or .tools builds a handler with it.
		eff := cfg.Proactive.Effective()
		interval := s.HeartbeatService.Interval()
		if h := sweepHandler(d, s, eff, al, interval); h != nil {
			// A sweep: reconcile, look at what changed, ask the model only if
			// something did. It runs on every tick, also when HEARTBEAT.md lists no
			// task: its reconciliation does not depend on one.
			s.HeartbeatService.SetRunWithoutTasks(true)
			s.HeartbeatService.SetContextHandler(h)
		} else {
			s.HeartbeatService.SetContextHandler(newGovernedHandler(
				al, s.Attention, interval, eff.HeartbeatModel, eff.HeartbeatTools))
		}
	} else {
		s.HeartbeatService.SetBus(msgBus)
		s.HeartbeatService.SetHandler(heartbeatHandler(al))
	}
	if err := s.HeartbeatService.Start(); err != nil {
		return s, fmt.Errorf("error %s heartbeat service: %w", starting, err)
	}
	p.sayf("✓ Heartbeat service %s", p.pick("started", "restarted"))

	// Device service, on the shared state manager. When the perception device
	// adapter has taken the monitors over, it is not started: two readers of the
	// same hardware would report every plug twice.
	if s.Perception != nil && s.Perception.devicesTakenOver {
		p.sayf("✓ Device events go through the perception pipeline")
		return s, nil
	}
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
	registerProactiveStatsAPI(mux, d.Shared.eventBus(), cfg.Gateway.ManagementToken,
		withPerceptionStats(perceptionStatsFn(s.perceptionLive)), withJournalStats(journalStatsFn(s.journalLive)))
	registerTriggerAPIFor(mux, s, d)
	registerControlAPI(mux, s.Control.killSwitch(), s.Control.stopper, cfg.Gateway.ManagementToken)

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
// resources. Order: heartbeat, the control path's hold on the services below,
// approval sweeper, workflow engine, perception (sources, then the engine), attention outbox, loss guard, channels, devices,
// notify, cron, media store.
// The sandbox server is deliberately not stopped here; see handleConfigReload
// and shutdownGateway.
func stopServices(s *gatewayServices, shutdownTimeout time.Duration) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()

	// The heartbeat goes first: a governed run in flight hands its decisions to
	// the outbox, which must still be accepting them, and Stop cancels that run
	// and waits for it.
	if s.HeartbeatService != nil {
		s.HeartbeatService.Stop()
	}

	// The control path lets go of the services it acts on before they stop, so that
	// an /approve that arrives while they do is told approvals are unavailable and
	// does not approve an order that nothing is left to execute. It stays installed
	// on the channels until they have stopped (below): taking it off earlier would
	// hand a late /approve or /stop to the agent instead.
	s.Control.detach()

	// The approval sweeper and propose_order go before the engine: no new proposal
	// may start while the services it needs are being stopped, and the sweeper edits
	// cards and delivers through notify and the channels, which are still up here.
	appr := s.Approval
	s.Approval.stop()
	s.Approval = nil
	s.DNFlow.stop() // the open tool refuses once the engine is going
	s.DNFlow = nil
	s.DCAFlow.stop() // the dispatcher starts nothing once the engine is going
	s.DCAFlow = nil

	// The workflow engine next, while channels, notify and the exchanges are still up:
	// a step in flight may be placing an order or sending a receipt, and it is let to
	// finish (bounded) before anything it uses goes away.
	if s.journalLive != nil {
		s.journalLive.Load().stop()
		s.journalLive.Store(nil)
	}
	appr.closeStores() // after the engine: a step in flight may still write the plan store

	// Then perception, before the outbox: the sources stop first so that nothing
	// new arrives, then the engine drains what its inbox holds through the pipeline,
	// which hands its decisions to the outbox. The outbox must still be accepting
	// them, so it stops after, never before. (The heartbeat is already stopped, so
	// no second producer is left.)
	s.Alerts.stop() // before the registry's pipeline goes: it writes to the registry and the cron store
	s.Alerts = nil
	s.Perception.stop()
	if s.perceptionLive != nil {
		s.perceptionLive.Store(nil)
	}
	s.Perception = nil

	// Then the outbox: its final flush delivers through the notify service and
	// the channels, which are still up at this point.
	s.attentionRun.stop()
	s.attentionRun, s.Attention = nil, nil

	s.LossGuard.Stop()
	s.LossGuard = nil
	if s.ChannelManager != nil {
		s.ChannelManager.StopAll(shutdownCtx)
	}
	s.Control = nil // the channels are stopped: nothing offers it a message any more
	if s.DeviceService != nil {
		s.DeviceService.Stop()
	}
	// After the device service, its last caller. Drains queued session writes.
	stopNotifyService(s.NotifyService)
	s.NotifyService = nil
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
