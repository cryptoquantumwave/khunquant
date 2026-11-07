package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/account"
	labapi "github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/api"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/faultops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/llmops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/marketops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/webops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// This file is the only place the gateway builds and attaches Sandbox Lab
// components. The lab exists only in a locked process; it is built once at start
// (buildLab), held on sharedResources, and never rebuilt by a reload.
//
// RULE for every lab group (inspect, market, clock, chat, llm, and what SBX-06 adds): (*lab.Lab).AddGroup
// is called ONCE per process, from registerLabGroups (which also attaches the sim
// engine), right after buildLab and before the first buildServices. Never from registerSandboxLab or anything else that runs per
// generation: the dev-MCP server builds its sandbox_<group> tools from Lab.Groups() when
// a generation starts, and registerDevMCP runs before registerSandboxLab, so a group
// added there is missing from that generation's tools. What a group needs from the
// current generation it reads at call time through a closure over sharedResources (see
// labDeps), never by capturing a generation's services.

// faultMaxHold caps every fault hold (timeout, written_then_timeout, latency). It is under
// a settle's 10 s cap and the lab call's 25 s, so a held request has ended, by closing the
// connection, before either gives up; the exchange clients wait longer (ccxt 30 s). A
// variable so tests can shorten it.
var faultMaxHold = 8 * time.Second

// kindConfigReload is journaled once a locked gateway has finished a config reload.
const kindConfigReload = "config.reload"

// labJournalKinds are the journal kinds the components of this binary write, declared
// so a scenario that reads one this build lacks is refused by name. A PR that adds a
// producer adds its kinds here (TestDeclaredKinds_CoverMergedProducers scans the source
// for kinds nobody declared). event.* covers every bus event, whose kind is
// event.<events.Kind>; the observer also files workflow, approval and policy events as
// workflow.step, proposal and policy.decision.
var labJournalKinds = []string{
	"chat.in", "chat.out", "chat.edit", "chat.press", "chat.react", "chat.state", // simchat
	"exchange.req",             // the sandbox server's request log
	simweb.KindReq,             // SimWeb: one web_fetch / web_search request
	sandbox.KindOrder,          // the simulator's order ledger: create, fill, cancel, effect
	sandbox.KindFunding,        // a funding payment of an open perpetual position
	"llm.call", "llm.complete", // the simulated model
	"event.*", "workflow.step", "proposal", "policy.decision", // the bus observer
	"tool.call",   // the tool tap
	"manage.call", // sandbox_manage
	"clock.step",  // the sim clock
	"settle",      // quiescence
	fault.KindFired,
	kindConfigReload,
}

// journalConfigReload appends config.reload {ok, error} once a reload has finished or been
// refused. The journal outlives every generation, so the entry is written after the
// services were rebuilt. A refused reload ends the process with exit 3 soon after.
func journalConfigReload(shared *sharedResources, errp *error) {
	if shared == nil || shared.lab == nil || shared.lab.Journal == nil {
		return
	}
	p := map[string]any{"ok": *errp == nil}
	if *errp != nil {
		p["error"] = (*errp).Error()
	}
	b, _ := json.Marshal(p)
	shared.lab.Journal.Append(labjournal.Entry{Kind: kindConfigReload, Source: "gateway", Payload: b})
}

// buildLab builds the process-lifetime lab from the enforced config. It starts the
// sandbox server (which publishes its base URL) and fails when there are no venues.
func buildLab(cfg *config.Config, lk sandbox.LockOptions) (*lab.Lab, error) {
	l, err := lab.New(context.Background(), lab.Options{
		Home:        config.HomeDir(),
		FixturesDir: sandbox.ResolveFixturesDir(cfg),
		Token:       lk.Token,
		// The one clock of the process (clock.json, else the KQ_SANDBOX_CLOCK_* env), built
		// inside New before the server starts: l.Now then follows it from the first request.
		Getenv: os.Getenv,
		// The gateway binds this port after the server starts: the server must not take it.
		AvoidPort: cfg.Gateway.Port,
	})
	if err != nil {
		return nil, err
	}
	// The simulator stamps orders and fills with lab time, so they match the candle times.
	l.Sim.SetClock(func() time.Time { return l.Now() })
	// The journal is opened once per process, here, so a reload never reopens it.
	j, err := labjournal.Open(labjournal.Options{
		Path: filepath.Join(l.Home, "lab", "journal.db"),
		Now:  func() time.Time { return l.Now() },
	})
	if err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("opening the lab journal: %w", err)
	}
	l.Journal = j
	l.DeclareKinds(labJournalKinds...)
	l.OnClose(j.Close)
	// The fault injector reads lab time through a closure (SBX-07 swaps l.Now) and is built
	// before the model engine, which asks it. It closes before the server stops, so a held
	// request does not stall the shutdown.
	l.Faults = fault.New(fault.Options{Now: func() time.Time { return l.Now() }, Journal: j, MaxHold: faultMaxHold})
	l.Chain.Set(sandbox.StageFault, l.Faults.Middleware())
	l.Server.SetHandlerWrap(l.Faults.HandlerWrap())
	l.OnClose(l.Faults.Close)
	// The simulator asks the injector what an order create should suffer, and journals its
	// order transitions at their own sim time. Only a locked process gets either: buildLab
	// is not run otherwise. The injector accepts the order effects once the simulator
	// that executes them is wired, so this stays after SetOrderEffects.
	l.Sim.SetOrderEffects(l.Faults)
	l.Sim.SetJournal(func(kind, subject string, payload any) {
		b, _ := json.Marshal(payload)
		e := labjournal.Entry{Kind: kind, Source: "sandbox", Subject: subject, Payload: b}
		switch p := payload.(type) {
		case sandbox.OrderEntry:
			e.SimTime = p.At
		case sandbox.FundingEntry:
			e.SimTime = p.Time
		}
		j.Append(e)
	})
	l.Faults.EnableOrderKinds("partial", "accept_no_fill", "cancel_after_accept", "create_reports_fill", "lookup_lag")
	// SimWeb answers every venue-web request itself (StageWeb). Its rules persist in
	// <home>/lab/web.json, outside the workspace the instance's file tools are confined to.
	w, err := simweb.Open(simweb.Options{Path: filepath.Join(l.Home, "lab", "web.json"), Journal: j, Now: func() time.Time { return l.Now() }})
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	l.Web = w
	l.Chain.Set(sandbox.StageWeb, w.Middleware())
	// Every request the chain sees is journaled (Append never blocks: a full queue drops).
	l.Chain.Observe(func(i sandbox.ReqInfo) {
		payload, _ := json.Marshal(i)
		j.Append(labjournal.Entry{Kind: "exchange.req", Source: "sandbox", Subject: i.Venue, Payload: payload})
	})
	return l, nil
}

// startLabObserver subscribes the journal to the shared bus once per process (a second
// subscriber would journal every event twice); it closes before the lab and its journal.
func startLabObserver(l *lab.Lab, shared *sharedResources) error {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.labObserver != nil || l.Journal == nil || shared.eventBus() == nil {
		return nil
	}
	obs, err := labjournal.StartObserver(context.Background(), shared.eventBus(), l.Journal)
	if err != nil {
		return err
	}
	shared.labObserver = obs
	shared.closers = append(shared.closers, namedCloser{name: "lab observer", c: obs})
	return nil
}

// registerLabGroups adds the lab's groups to the registry, once per process (see the
// rule above). Call it right after buildLab, before the first buildServices.
func registerLabGroups(l *lab.Lab, shared *sharedResources, cfg *config.Config) error {
	// The lab's sim clock is the process's clock: every generation's components read it.
	if l.Clock != nil { // never a typed nil in the interface
		shared.Clock = l.Clock
	}
	if err := l.AddGroup(inspect.Group(l.Journal, labDeps(shared))); err != nil {
		return err
	}
	if err := l.AddGroup(l.ClockGroup()); err != nil {
		return err
	}
	if err := l.AddGroup(marketops.Group(l.Market, marketops.Options{Home: l.Home, ResetClients: exchanges.ResetInstanceCache})); err != nil {
		return err
	}
	// The simulator lives as long as the lab, so the account group holds it directly.
	if err := l.AddGroup(account.Group(l.Sim)); err != nil {
		return err
	}
	// call_tool needs the current generation's agent loop: it reads it at call time.
	if err := l.AddGroup(callToolGroup(shared)); err != nil {
		return err
	}
	if err := l.AddGroup(manageGroup(shared)); err != nil {
		return err
	}
	// The chat group's world lives as long as the process; each generation's channel
	// attaches to it (attachSimChat). The personas are those of the first config.
	var j simchat.Journal
	if l.Journal != nil { // a nil *Journal inside the interface would not be nil
		j = l.Journal
	}
	shared.simWorld = simchat.NewWorld(j, func() time.Time { return l.Now() }, cfg.Channels.SimChat)
	// Every component that can hold work in flight, once per process. An advance (l.Settle,
	// set by lab.New) and inspect settle wait for all of them.
	registerQuiescers(l.Quiesce, shared, l)
	if err := l.AddGroup(shared.simWorld.Group()); err != nil {
		return err
	}
	// The simulated model: the engine answers every model call of the process (the
	// provider factory builds only sim/*, which talks to the attached engine), so it
	// is attached once, with the lab, and detached when the lab closes.
	var sj sim.Journal
	if l.Journal != nil {
		sj = simJournal{l.Journal}
	}
	e, err := sim.NewEngine(sim.EngineOptions{
		Journal: sj, TurnInfo: simTurnInfo, Faults: llmFaults{l.Faults},
		Store: filepath.Join(l.Home, "lab", "llm_script.json"),
	})
	if err != nil {
		return err
	}
	if err := sim.Attach(e); err != nil {
		return err
	}
	l.OnClose(func() error { sim.Detach(e); return nil })
	if err := l.AddGroup(faultops.Group(l.Faults)); err != nil {
		return err
	}
	if err := l.AddGroup(webops.Group(l.Web)); err != nil {
		return err
	}
	return l.AddGroup(llmops.Group(e))
}

// llmFaults lets the fault injector delay or fail a model call. The wait is made here, in
// the injector's counted hold (a settle sees it), so the engine gets no delay of its own.
type llmFaults struct{ i *fault.Injector }

func (f llmFaults) BeforeLLM(ctx context.Context, c sim.CallInfo) (time.Duration, error) {
	d, err := f.i.LLM(ctx, c.Turn, c.Caller, c.Model)
	if err == nil {
		err = f.i.Hold(ctx, d)
	}
	return 0, err
}

// simJournal writes the simulated model's llm.call / llm.complete entries to the lab
// journal (sim cannot import labjournal: see its imports_test.go).
type simJournal struct{ j *labjournal.Journal }

func (s simJournal) Record(kind, subject string, payload json.RawMessage) {
	s.j.Append(labjournal.Entry{Kind: kind, Source: "sim", Subject: subject, Payload: payload})
}

// simTurnInfo tells the simulated model what kind of turn a call belongs to, from the
// context the agent loop tagged. pkg/providers/sim cannot import pkg/tools; this can.
// A sub-turn comes first (it keeps its parent's origin), then a proactive or reflection
// turn by its kind, then the legacy wake-ups by origin; a person's message, from a chat
// or the console, is "user". An untagged context is "unknown", never a guess.
func simTurnInfo(ctx context.Context) sim.CallInfo {
	ci := sim.CallInfo{Origin: tools.ToolOrigin(ctx), TurnID: tools.TurnID(ctx), Sender: tools.ToolSender(ctx)}
	switch kind := tools.ProactiveTurnKind(ctx); {
	case tools.IsSubTurn(ctx):
		ci.Turn = string(proactive.TurnSubTurn)
	case kind != "":
		ci.Turn = string(kind)
	case ci.Origin == tools.OriginHeartbeat:
		ci.Turn = string(proactive.TurnHeartbeatLegacy)
	case ci.Origin == tools.OriginCron:
		ci.Turn = string(proactive.TurnCronLegacy)
	case ci.Origin == tools.OriginRemote || ci.Origin == tools.OriginInternal:
		ci.Turn = string(proactive.TurnUser)
	}
	return ci
}

// attachSimChat binds this generation's simchat channel (the manager built a new one)
// to the process's world, so the chat group talks to the channel that is running now.
func attachSimChat(shared *sharedResources, cm *channels.Manager) {
	if shared.simWorld == nil || cm == nil {
		return
	}
	if ch, ok := cm.GetChannel("simchat"); ok {
		if sc, ok := ch.(*simchat.Channel); ok {
			sc.Attach(shared.simWorld)
		}
	}
}

// labDeps is the inspect group's view of the current generation, looked up on every
// call. An op whose service this generation does not run gets a nil dependency and
// answers unavailable. Between generations (a reload) every op that needs one answers
// errNoGeneration instead, so that cannot be read as "this instance lacks the feature";
// ops that read only the journal keep working.
func labDeps(shared *sharedResources) func() inspect.Deps {
	return func() inspect.Deps {
		s := shared.labGen.Load()
		if s == nil {
			return inspect.Deps{Err: errNoGeneration()}
		}
		var d inspect.Deps
		if shared.lab != nil && shared.lab.Quiesce != nil {
			d.SettleWait, d.Snapshot = shared.lab.SettleWait, shared.lab.Quiesce.Snapshot
		}
		if rt := s.journalLive.Load(); rt != nil {
			d.Workflows = rt.engine.List
		}
		if s.Approval != nil && s.Approval.svc != nil {
			d.Pending = s.Approval.svc.ListPending
		}
		if s.Policy != nil && s.Policy.store != nil {
			d.Policy = s.Policy.store.Recent
		}
		return d
	}
}

// errNoGeneration is the one answer of every lab group while no generation of services
// is published (a reload is in progress, or the first build is not done). The scenario
// runner's noServices recognises it for every group.
func errNoGeneration() error {
	return lab.Errorf(lab.CodeUnavailable, "no generation of services is running (a reload is in progress?)")
}

// publishLabGeneration makes s the generation the lab groups read. buildServices calls
// it last, once s is fully built (the lab's goroutines read s's fields, which are
// assigned until then), and only when the build succeeded; retireLabGeneration
// withdraws a generation before its services stop.
func publishLabGeneration(shared *sharedResources, s *gatewayServices) {
	if shared.lab != nil {
		shared.labGen.Store(s)
	}
}

func (r *sharedResources) retireLabGeneration() { r.labGen.Store(nil) }

// attachLabTap puts the tool-call tap on the default agent's registry, once per
// registry: a reload builds a new registry (ReloadProviderAndConfig), and a recovery
// rebuild reuses the old one.
func attachLabTap(shared *sharedResources, reg *tools.ToolRegistry) {
	if shared.lab == nil || shared.lab.Journal == nil || reg == nil {
		return
	}
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.labTapped != reg {
		reg.AddHook(labToolTap{j: shared.lab.Journal})
		shared.labTapped = reg
	}
}

// registerSandboxLab starts the observer, taps tool calls and mounts the lab route; it
// does nothing unless the process is locked.
func registerSandboxLab(mux handleMux, s *gatewayServices, d assemblyDeps) error {
	lk, locked := sandbox.LockInfo()
	if !locked || s.lab == nil {
		return nil
	}
	d.Shared.retireLabGeneration() // until buildServices has built this one
	if reg := d.AgentLoop.GetRegistry(); reg != nil {
		if inst := reg.GetDefaultAgent(); inst != nil {
			attachLabTap(d.Shared, inst.Tools)
		}
	}
	// sandbox_manage's private mux: the same registrar the gateway's mux uses, over this
	// generation's services, called in process. Set before buildServices publishes s.
	priv := http.NewServeMux()
	registerProactiveRoutes(priv, s, d)
	s.labSetup = &labSetup{al: d.AgentLoop, bus: d.Bus, mux: priv, token: d.Cfg.Gateway.ManagementToken}
	attachSimChat(d.Shared, s.ChannelManager)
	if err := startLabObserver(s.lab, d.Shared); err != nil {
		return fmt.Errorf("starting the lab observer: %w", err)
	}
	labapi.Register(mux, s.lab, labGuard(lk.Token))
	return nil
}

// labGuard is the lab routes' guard: loopback only, the instance token as bearer, and a
// 503 for every request while there is no token (bearerTokenMiddleware passes all then).
func labGuard(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return loopbackOnly(bearerTokenMiddleware(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				writeManage(w, http.StatusServiceUnavailable, map[string]string{"error": "no instance token is configured"})
				return
			}
			next.ServeHTTP(w, r)
		})))
	}
}

// registerLockedSandbox attaches the running lab to this generation of services and
// puts the /api/sandbox/* routes on mux. The bearer is the instance token, which
// stays in memory: it is not saved to config and not printed.
func registerLockedSandbox(mux handleMux, s *gatewayServices, l *lab.Lab) error {
	if l == nil {
		return errors.New("locked sandbox has no lab")
	}
	s.SandboxServer = l.Server
	s.SandboxToken = l.Token()
	s.SimulatorReseter = &sandboxStateReseter{sm: l.State, store: l.Store}

	mountSandboxAPI(mux, s, l.Store, l.FixturesDir())

	logger.InfoCF("sandbox", "Locked sandbox instance serving", map[string]any{"base_url": l.Server.BaseURL()})
	return nil
}

// mountSandboxAPI registers the /api/sandbox/* routes for the sandbox that s
// carries. Both the locked and the unlocked path end here, so the routes have one
// registration site.
func mountSandboxAPI(mux handleMux, s *gatewayServices, store *sandbox.Store, fixturesDir string) {
	registerSandboxAPI(mux, store, s.SandboxToken, s.SimulatorReseter, fixturesDir, s.labReady)
}
