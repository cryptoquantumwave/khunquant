package triggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

var bkk = func() *time.Location {
	l, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic(err)
	}
	return l
}()

// noon is 12:00 in Bangkok: inside the default active hours.
var noon = time.Date(2026, 3, 10, 12, 0, 0, 0, bkk)

const (
	kindTick    = events.KindPerceptionMarketTick
	kindPort    = events.KindPerceptionPortfolioSnapshot
	testKind    = "price_cross"
	otherKind   = "move_vs_vol"
	secretError = "SECRET-FROM-THE-OUTSIDE"
)

// fakeView is a worldmodel.View of which only Now and Fresh work: the triggers of
// these tests need nothing else, and one that did would panic on the nil embedded
// interface, which the registry would then count as its error.
type fakeView struct {
	worldmodel.View
	now time.Time
}

func (v fakeView) Now() time.Time { return v.now }
func (v fakeView) Fresh(at time.Time, maxAge time.Duration) bool {
	return !at.IsZero() && v.now.Sub(at) <= maxAge
}

func diffOf(kind events.Kind) worldmodel.Diff {
	return worldmodel.Diff{
		Event: events.Event{ID: "ev", Kind: kind, Trust: events.TrustPtr(proactive.TrustExchange)},
		At:    noon,
	}
}

// evalFn is what a scripted trigger does. call counts the trigger's evaluations
// from 1.
type evalFn func(call int, st *State, d worldmodel.Diff) ([]proactive.Candidate, error)

// scripted is a Trigger whose behaviour a test supplies.
type scripted struct {
	id      string
	kinds   []events.Kind
	demands []perception.Demand
	fn      evalFn
	calls   atomic.Int64
}

func (s *scripted) ID() string                   { return s.id }
func (s *scripted) Kinds() []events.Kind         { return s.kinds }
func (s *scripted) Demands() []perception.Demand { return s.demands }
func (s *scripted) Evaluate(_ context.Context, _ worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	return s.fn(int(s.calls.Add(1)), st, d)
}

// scriptedParams are the parameters of a scripted spec.
type scriptedParams struct {
	Symbols []string `json:"symbols"`
	Reject  bool     `json:"reject"`
	Tag     string   `json:"tag"`
}

// factoryOf returns a Factory for kind whose triggers run fn on the given event
// kinds and demand a ticker for each symbol in the parameters. built receives
// every trigger it makes.
func factoryOf(kind string, kinds []events.Kind, fn evalFn, built *sync.Map) Factory {
	return func(spec Spec) (Trigger, error) {
		var p scriptedParams
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return nil, err
		}
		if p.Reject {
			return nil, errors.New("these parameters are not accepted")
		}
		var ds []perception.Demand
		for _, s := range p.Symbols {
			ds = append(ds, perception.Demand{Provider: "okx", Symbol: s, Kind: perception.DemandTicker})
		}
		t := &scripted{id: spec.Kind + ":" + spec.ID, kinds: kinds, demands: ds, fn: fn}
		if built != nil {
			built.Store(spec.ID, t)
		}
		return t, nil
	}
}

// cand is a well-formed candidate of kind.
func cand(kind, variant string) proactive.Candidate {
	return proactive.Candidate{
		DedupKey: kind + ":BTC/USDT:" + variant,
		Severity: proactive.SeverityNotice,
		Subject:  "BTC/USDT",
		Title:    "BTC/USDT crossed " + variant,
		Trust:    proactive.TrustExchange,
		Evidence: map[string]any{"price": 61000.5},
	}
}

// fires returns a trigger function that returns one candidate of kind each time.
func fires(kind string) evalFn {
	return func(call int, _ *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		return []proactive.Candidate{cand(kind, fmt.Sprintf("n%d", call))}, nil
	}
}

func failsWith(err error) evalFn {
	return func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) { return nil, err }
}

// env is a registry over a fresh database with a counter for IDs.
type env struct {
	t      *testing.T
	db     *sql.DB
	global atomic.Value // string
	ids    atomic.Int64
	now    atomic.Pointer[time.Time]
	r      *Registry
}

func newEnv(t *testing.T, f map[string]Factory) *env {
	t.Helper()
	e := &env{t: t, db: sqlitextest.Open(t)}
	e.global.Store(ModeLive)
	e.setNow(noon)
	e.r = e.registry(f)
	return e
}

func (e *env) setNow(tm time.Time) { e.now.Store(&tm) }

// registry builds another registry over the same database, as a restart does.
func (e *env) registry(f map[string]Factory) *Registry {
	e.t.Helper()
	r, err := NewRegistry(context.Background(), e.db, func() string { return e.global.Load().(string) }, f,
		WithClock(func() time.Time { return *e.now.Load() }),
		WithIDFunc(func() string { return fmt.Sprintf("id-%05d", e.ids.Add(1)) }))
	if err != nil {
		e.t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func (e *env) setGlobal(m string) { e.global.Store(m) }

// add adds a spec of kind in mode with the given parameters.
func (e *env) add(id, kind, mode, params string) Spec {
	e.t.Helper()
	s, err := e.r.Add(context.Background(), Spec{
		ID: id, Kind: kind, Mode: mode, Origin: "test", Params: json.RawMessage(params),
	})
	if err != nil {
		e.t.Fatalf("Add %s: %v", id, err)
	}
	return s
}

// eval evaluates an event of kind and, as the engine does, reports every candidate
// as decided.
func (e *env) eval(kind events.Kind) []proactive.Candidate {
	e.t.Helper()
	cs := e.evalUncommitted(kind)
	e.r.Committed(context.Background(), idsOf(cs))
	return cs
}

// evalUncommitted evaluates and does not call Committed.
func (e *env) evalUncommitted(kind events.Kind) []proactive.Candidate {
	e.t.Helper()
	cs, err := e.r.Evaluate(context.Background(), fakeView{now: noon}, diffOf(kind))
	if err != nil {
		e.t.Fatalf("Evaluate: %v", err)
	}
	return cs
}

func idsOf(cs []proactive.Candidate) []string {
	var ids []string
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

func (e *env) row(id string) (mode, state string, errs int) {
	e.t.Helper()
	err := e.db.QueryRow(`SELECT mode, state_json, errors FROM triggers WHERE id = ?`, id).Scan(&mode, &state, &errs)
	if err != nil {
		e.t.Fatalf("row %s: %v", id, err)
	}
	return mode, state, errs
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func nanFloat() float64 { return math.NaN() }

func manyDemands() *scripted {
	t := &scripted{id: testKind + ":s1", kinds: []events.Kind{kindTick}}
	for i := 0; i <= MaxDemandsPerTrigger; i++ {
		t.demands = append(t.demands, perception.Demand{Provider: "okx", Symbol: fmt.Sprintf("S%d/USDT", i)})
	}
	return t
}

func negativeInterval() *scripted {
	return &scripted{
		id: testKind + ":s1", kinds: []events.Kind{kindTick},
		demands: []perception.Demand{{Provider: "okx", Symbol: "BTC/USDT", MinInterval: -time.Second}},
	}
}
