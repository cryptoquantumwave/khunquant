package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	labapi "github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/api"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/inspect"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// This file is the only place the gateway builds and attaches Sandbox Lab
// components. The lab exists only in a locked process; it is built once at start
// (buildLab), held on sharedResources, and never rebuilt by a reload.
//
// RULE for every lab group (inspect, chat, and what SBX-03/04/06 add): (*lab.Lab).AddGroup
// is called ONCE per process, from registerLabGroups, right after buildLab and before
// the first buildServices. Never from registerSandboxLab or anything else that runs per
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
	// The chat group's world lives as long as the process; each generation's channel
	// attaches to it (attachSimChat). The personas are those of the first config.
	var j simchat.Journal
	if l.Journal != nil { // a nil *Journal inside the interface would not be nil
		j = l.Journal
	}
	shared.simWorld = simchat.NewWorld(j, func() time.Time { return l.Now() }, cfg.Channels.SimChat)
	return l.AddGroup(shared.simWorld.Group())
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
