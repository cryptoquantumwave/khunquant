// Package lab is the composition root of the Sandbox Lab: the pieces of one locked
// instance (exchange sandbox server, fixture store, simulator state) built once and
// kept for the life of the process. A config reload never rebuilds them, so the
// server's port and the simulator's orders survive it.
package lab

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// Options configures New.
type Options struct {
	Home        string // KHUNQUANT_HOME of the instance
	FixturesDir string // must be inside Home
	Token       string
	// Getenv, when set, makes New build the instance clock (InitClock) before the server
	// starts, so no request is served while Now is still the wall clock. Nil: no sim clock.
	Getenv func(string) string
}

// Lab holds the components of one sandbox instance.
type Lab struct {
	Home   string
	Server *sandbox.Server
	Store  *sandbox.Store
	State  *sandbox.StateManager
	Sim    *sandbox.StatefulSimulator
	Chain  *sandbox.Chain // the server's responder: fault, market, orders, web, fixtures
	Market *market.Book   // SimMarket: the one price per symbol (tickers, klines, fills)

	Journal *labjournal.Journal // opened by the gateway under the lock
	// Clock is the instance's sim clock (nil until InitClock); Now is derived from it,
	// and is wall time before that. Read Now per use.
	Clock *simclock.Sim
	Now   func() time.Time
	// Quiesce is the registry of components that can hold work in flight; the gateway
	// registers them once (sandbox_lab_quiesce.go). Settle, which an advance waits on
	// after each step (nil: nothing), is a settle over it unless a test replaces it.
	Quiesce *quiesce.Registry
	Settle  func(context.Context) error
	// Later PRs add their components here: Faults, Web, Crash, ...

	fixturesDir string
	token       string

	mu      sync.Mutex
	onClose []func() error
	closed  bool

	gmu    sync.RWMutex // guards groups (group.go)
	groups map[string]Group

	kmu   sync.RWMutex // guards kinds
	kinds map[string]bool
}

// New loads the fixtures, seeds the simulator and starts the sandbox server. It
// fails when the fixtures directory is outside Home or yields no venue, because a
// lab with no venues can serve nothing.
func New(ctx context.Context, opts Options) (*Lab, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("lab: an instance token is required")
	}
	if !sandbox.PathInside(opts.Home, opts.FixturesDir) {
		return nil, fmt.Errorf("lab: fixtures dir %q is not inside the instance home %q", opts.FixturesDir, opts.Home)
	}

	store := sandbox.NewStore()
	if err := store.Load(opts.FixturesDir); err != nil {
		return nil, fmt.Errorf("lab: load fixtures: %w", err)
	}
	if len(store.Venues()) == 0 {
		return nil, fmt.Errorf("lab: no venues in fixtures dir %q", opts.FixturesDir)
	}

	state := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, state)
	sim := sandbox.NewStatefulSimulator(state)

	srv := sandbox.NewServer(store)
	l := &Lab{
		Home:        opts.Home,
		Server:      srv,
		Store:       store,
		State:       state,
		Sim:         sim,
		Chain:       sandbox.NewChain(store),
		Now:         time.Now,
		fixturesDir: opts.FixturesDir,
		token:       opts.Token,
		Quiesce:     quiesce.NewRegistry(),
	}
	l.Settle = l.settleStep
	// The book reads l.Now on every call, so the sim clock (SBX-07) needs no re-wiring.
	l.Market = market.NewBook(market.Options{Now: func() time.Time { return l.Now() }})
	sim.SetPriceSource(l.Market)
	l.Chain.Set(sandbox.StageMarket, l.Market.Middleware())
	l.Chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(sim))
	srv.SetResponder(l.Chain)
	if opts.Getenv != nil {
		if err := l.InitClock(opts.Getenv); err != nil {
			return nil, err
		}
	}
	if err := srv.Start(ctx); err != nil {
		if l.Clock != nil {
			_ = l.Clock.Close()
		}
		return nil, fmt.Errorf("lab: start sandbox server: %w", err)
	}
	sandbox.SetInstance(srv)
	return l, nil
}

// DeclareKinds records the journal kinds this instance writes, so a scenario that reads
// one that is not declared can be refused by name. A name is exact, or "prefix.*" for
// every kind under it (event.* covers event.<kind>). Declaring twice is harmless.
func (l *Lab) DeclareKinds(kinds ...string) {
	l.kmu.Lock()
	defer l.kmu.Unlock()
	if l.kinds == nil {
		l.kinds = map[string]bool{}
	}
	for _, k := range kinds {
		l.kinds[k] = true
	}
}

// Kinds returns the declared journal kinds, sorted.
func (l *Lab) Kinds() []string {
	l.kmu.RLock()
	defer l.kmu.RUnlock()
	return append([]string{}, slices.Sorted(maps.Keys(l.kinds))...)
}

// Token is the instance token.
func (l *Lab) Token() string { return l.token }

// FixturesDir is where the fixtures were loaded from (and are written back to).
func (l *Lab) FixturesDir() string { return l.fixturesDir }

// OnClose registers teardown that Close runs, newest first, before it stops the server.
func (l *Lab) OnClose(f func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onClose = append(l.onClose, f)
}

// Close runs the registered teardown and stops the server. Only the first call does
// anything. Under the process lock, stopping the server leaves the sandbox armed with
// no base URL, so exchange traffic fails closed.
func (l *Lab) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	fns := l.onClose
	l.onClose = nil
	l.mu.Unlock()

	var errs []error
	for i := len(fns) - 1; i >= 0; i-- {
		if err := fns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	if err := l.Server.Stop(); err != nil {
		errs = append(errs, err)
	}
	if sandbox.GetInstance() == l.Server {
		sandbox.SetInstance(nil)
	}
	return errors.Join(errs...)
}
