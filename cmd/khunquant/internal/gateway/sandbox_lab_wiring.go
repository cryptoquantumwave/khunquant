package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
	labapi "github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab/api"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// This file is the only place the gateway builds and attaches Sandbox Lab
// components. The lab exists only in a locked process; it is built once at start
// (buildLab), held on sharedResources, and never rebuilt by a reload.

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

// registerSandboxLab starts the observer and mounts the lab route; it does nothing
// unless the process is locked.
func registerSandboxLab(mux handleMux, s *gatewayServices, d assemblyDeps) error {
	lk, locked := sandbox.LockInfo()
	if !locked || s.lab == nil {
		return nil
	}
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
