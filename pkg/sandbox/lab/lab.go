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
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
)

// Options configures New.
type Options struct {
	Home        string // KHUNQUANT_HOME of the instance
	FixturesDir string // must be inside Home
	Token       string
	// Getenv, when set, makes New build the instance clock (InitClock) before the server
	// starts, so no request is served while Now is still the wall clock. Nil: no sim clock.
	Getenv func(string) string
	// AvoidPort is the gateway's own port: the server must not take it (0: none).
	AvoidPort int
	// DeferStart makes New build the lab without starting the server: the caller attaches
	// the rest of the world (the fault rules), restores it, and calls Start. The server then
	// serves its first request on a fully restored world.
	DeferStart bool
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
	// Faults is the fault injector (nil until the gateway builds it under the lock).
	Faults *fault.Injector
	// Web is SimWeb, the simulated web (nil until the gateway builds it under the lock).
	Web *simweb.Store
	// Crash is the table of armed crash points (nil until the gateway builds it under the lock).
	Crash *crash.Arms

	fixturesDir string
	token       string

	mu      sync.Mutex
	onClose []func() error
	closed  bool

	gmu    sync.RWMutex // guards groups (group.go)
	groups map[string]Group

	kmu   sync.RWMutex // guards kinds
	kinds map[string]bool

	pmu   sync.Mutex // guards the list of saved parts (persist.go); each part has its own lock
	world []*persisted
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
	sim.EnableEquityVenues() // the lab, and only the lab, simulates Settrade and Webull

	srv := sandbox.NewServer(store)
	srv.AvoidPort(opts.AvoidPort)
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
	l.Chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(persistingSim{sim, l}))
	srv.SetResponder(l.Chain)
	if opts.Getenv != nil {
		if err := l.InitClock(opts.Getenv); err != nil {
			return nil, err
		}
	}
	// The exchange and the market outlive the process: they are restored before the server
	// can answer, after the clock and the simulator's wake exist.
	sim.SetClock(func() time.Time { return l.Now() }) // the restore re-arms the wake on sim time
	l.Register(l.Market)
	l.Register(sim)
	if err := l.Restore(); err != nil {
		if l.Clock != nil {
			_ = l.Clock.Close()
		}
		return nil, err
	}
	if opts.DeferStart {
		return l, nil
	}
	return l, l.Start(ctx)
}

// Start starts the sandbox server and publishes it as the process's instance. New calls it
// unless DeferStart was set.
func (l *Lab) Start(ctx context.Context) error {
	if err := l.Server.Start(ctx); err != nil {
		if l.Clock != nil {
			_ = l.Clock.Close()
		}
		return fmt.Errorf("lab: start sandbox server: %w", err)
	}
	sandbox.SetInstance(l.Server)
	return nil
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
