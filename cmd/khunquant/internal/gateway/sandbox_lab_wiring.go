package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/account"
	labapi "github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/api"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/llmops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/marketops"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// This file is the only place the gateway builds and attaches Sandbox Lab
// components. The lab exists only in a locked process; it is built once at start
// (buildLab), held on sharedResources, and never rebuilt by a reload.
//
// RULE for every lab group (inspect, market, chat, llm, and what SBX-06 adds): (*lab.Lab).AddGroup
// is called ONCE per process, from registerLabGroups (which also attaches the sim
// engine), right after buildLab and before the first buildServices. Never from registerSandboxLab or anything else that runs per
// generation: the dev-MCP server builds its sandbox_<group> tools from Lab.Groups() when
// a generation starts, and registerDevMCP runs before registerSandboxLab, so a group
// added there is missing from that generation's tools. What a group needs from the
// current generation it reads at call time through a closure over sharedResources (see
// labDeps), never by capturing a generation's services.

// buildLab builds the process-lifetime lab from the enforced config. It starts the
// sandbox server (which publishes its base URL) and fails when there are no venues.
func buildLab(cfg *config.Config, lk sandbox.LockOptions) (*lab.Lab, error) {
	l, err := lab.New(context.Background(), lab.Options{
		Home:        config.HomeDir(),
		FixturesDir: sandbox.ResolveFixturesDir(cfg),
		Token:       lk.Token,
	})
	if err != nil {
		return nil, err
	}
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
	l.OnClose(j.Close)
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
	if err := l.AddGroup(inspect.Group(l.Journal, labDeps(shared))); err != nil {
		return err
	}
	if err := l.AddGroup(marketops.Group(l.Market, marketops.Options{Home: l.Home})); err != nil {
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
		Journal: sj, TurnInfo: simTurnInfo, Store: filepath.Join(l.Home, "lab", "llm_script.json"),
	})
	if err != nil {
		return err
	}
	if err := sim.Attach(e); err != nil {
		return err
	}
	l.OnClose(func() error { sim.Detach(e); return nil })
	return l.AddGroup(llmops.Group(e))
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
// answers unavailable; between generations nothing is published.
func labDeps(shared *sharedResources) func() inspect.Deps {
	return func() inspect.Deps {
		s := shared.labGen.Load()
		if s == nil {
			return inspect.Deps{}
		}
		var d inspect.Deps
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

// publishLabGeneration makes s the generation the lab groups read. It runs once s has
// its approval service, workflow engine and policy record, so no group sees a half-built
// generation; retireLabGeneration withdraws a generation before its services stop.
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
	s.labSetup = &labSetup{al: d.AgentLoop, mux: priv, token: d.Cfg.Gateway.ManagementToken}
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
	registerSandboxAPI(mux, store, s.SandboxToken, s.SimulatorReseter, fixturesDir)
}
