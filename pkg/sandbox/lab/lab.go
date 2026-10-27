// Package lab is the composition root of the Sandbox Lab: the pieces of one locked
// instance (exchange sandbox server, fixture store, simulator state) built once and
// kept for the life of the process. A config reload never rebuilds them, so the
// server's port and the simulator's orders survive it.
package lab

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

// Options configures New.
type Options struct {
	Home        string // KHUNQUANT_HOME of the instance
	FixturesDir string // must be inside Home
	Token       string
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
	Now     func() time.Time    // sim-time source (wall time until the sim clock); read it per use
	// Later PRs add their components here: Clock, Faults, Web, Crash, ...

	fixturesDir string
	token       string

	mu      sync.Mutex
	onClose []func() error
	closed  bool

	gmu    sync.RWMutex // guards groups (group.go)
	groups map[string]Group
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
	}
	// The book reads l.Now on every call, so the sim clock (SBX-07) needs no re-wiring.
	l.Market = market.NewBook(market.Options{Now: func() time.Time { return l.Now() }})
	sim.SetPriceSource(l.Market)
	l.Chain.Set(sandbox.StageMarket, l.Market.Middleware())
	l.Chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(sim))
	srv.SetResponder(l.Chain)
	if err := srv.Start(ctx); err != nil {
		return nil, fmt.Errorf("lab: start sandbox server: %w", err)
	}
	sandbox.SetInstance(srv)
	return l, nil
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
