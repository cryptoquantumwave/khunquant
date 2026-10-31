package triggers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// warnEvery limits how often one kind of failure of one trigger is logged.
const warnEvery = time.Minute

// maxWarnKeys bounds the rate limiter's memory.
const maxWarnKeys = 1024

// Option configures NewRegistry.
type Option func(*Registry)

// WithClock sets the clock used for the created and updated stamps and for the
// bounded-rate logging. It is never used to decide what a trigger sees: that is
// View.Now(). The default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) {
		if now != nil {
			r.now = now
		}
	}
}

// WithIDFunc sets how candidate and spec IDs are made. The default is
// proactive.NewID (a ULID); tests use a counter so that runs compare equal.
func WithIDFunc(f func() string) Option {
	return func(r *Registry) {
		if f != nil {
			r.newID = f
		}
	}
}

// Registry holds the trigger specs, builds their triggers, routes diffs to them and
// keeps their state. It implements perception.DemandSource and, through the
// gateway's stage adapter, the engine's Evaluator.
//
// # Threading
//
// Evaluate and Committed are called by ONE goroutine, the pipeline's, one after the
// other for every event. Add, Update, Remove, List and Demands may be called from
// any goroutine at any time, while Evaluate runs. The locking that makes it safe:
//
//   - The set of triggers is an immutable snapshot behind an atomic pointer. Evaluate
//     and Demands read it without a lock and never wait for a mutator. A mutator
//     (Add, Update, Remove) holds writeMu, which only mutators take, across its
//     database write, then swaps in a new snapshot. The pipeline never takes writeMu,
//     so a slow write by a tool cannot stall it.
//   - Each entry's mutable fields (its mode, state and error count) are guarded by
//     the entry's own mutex, held only for memory operations, never across a
//     database call or a call into a trigger.
//   - The pipeline's own database writes (the first load of a state, and the save
//     after attention has decided) are UPDATEs guarded by the spec's updated_ms, so a
//     write made for a spec that has since been changed or removed changes nothing.
//   - Lock order: writeMu, then an entry's mutex. No lock is held while a trigger or
//     a factory runs.
type Registry struct {
	db         *sql.DB
	globalMode func() string
	factories  map[string]Factory
	now        func() time.Time
	newID      func() string

	writeMu sync.Mutex
	snap    atomic.Pointer[snapshot]

	pendMu sync.Mutex
	pend   []*pending

	warnMu sync.Mutex
	warned map[string]time.Time

	// fireObs is told about the live candidates of intent specs once committed
	// (SetFireObserver).
	fireObs atomic.Pointer[func(Firing)]

	shadowMu     sync.Mutex
	shadowByKind map[string]uint64

	c counters
}

// snapshot is the set of specs at one moment. It is never modified after it is
// published.
type snapshot struct {
	byID    map[string]*entry
	ordered []*entry // by spec ID
	byKind  map[events.Kind][]*entry
}

// entry is one spec with its trigger. trig is nil when the spec cannot be run
// (its kind is unknown to this build, or its parameters are refused): it is kept so
// that nothing is lost, listed, and skipped.
type entry struct {
	trig    Trigger
	tid     string // trig.ID()
	kinds   []events.Kind
	demands []perception.Demand
	skip    string // why trig is nil

	mu          sync.Mutex
	spec        Spec
	updatedMs   int64 // the row's updated_ms, the guard of every pipeline write
	stateLoaded bool
	state       json.RawMessage
	unsaved     bool // the adopted state is not in the table yet
	errs        int
	persistedEr int
}

// NewRegistry creates the triggers table if needed, loads the specs and builds the
// triggers of the kinds f has a factory for. globalMode returns the global triggers
// mode ("off", "shadow" or "live"; the effective one from the config) and is read
// on every evaluation, so a change of configuration applies without rebuilding.
//
// A spec whose kind has no factory here, or whose parameters its factory refuses
// (written by a newer or an older version), is kept, skipped and reported once. It
// is never deleted.
func NewRegistry(ctx context.Context, db *sql.DB, globalMode func() string, f map[string]Factory, opts ...Option) (*Registry, error) {
	if db == nil {
		return nil, errors.New("triggers: nil database")
	}
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		return nil, fmt.Errorf("triggers: %w", err)
	}
	r := &Registry{
		db:         db,
		globalMode: globalMode,
		factories:  make(map[string]Factory, len(f)),
		//simclock:wall the default when no clock is injected
		now:    time.Now,
		newID:  proactive.NewID,
		warned: map[string]time.Time{},
	}
	if r.globalMode == nil {
		r.globalMode = func() string { return ModeOff }
	}
	for k, fn := range f {
		if fn != nil {
			r.factories[k] = fn
		}
	}
	for _, o := range opts {
		o(r)
	}

	rows, more, err := loadRows(ctx, db)
	if err != nil {
		return nil, err
	}
	if more {
		logger.WarnCF("triggers", "More triggers are stored than the registry holds; the newest are ignored",
			map[string]any{"limit": MaxSpecs})
	}
	var es []*entry
	for _, row := range rows {
		es = append(es, r.load(row))
	}
	r.snap.Store(newSnapshot(es))
	return r, nil
}

// load turns a stored row into an entry, or into a skipped one.
func (r *Registry) load(row storedRow) *entry {
	spec := row.spec
	var err error
	switch {
	case row.paramsSize > MaxParamsBytes:
		err = fmt.Errorf("%w: params over %d bytes", ErrInvalidSpec, MaxParamsBytes)
	default:
		if len(spec.Params) == 0 {
			spec.Params = json.RawMessage("{}")
		}
		err = validateSpec(spec)
	}
	var e *entry
	if err == nil {
		e, err = r.build(spec)
	}
	if err != nil {
		e = &entry{spec: spec, skip: skipReason(err)}
		r.c.unusable.Add(1)
		logger.WarnCF("triggers", "A stored trigger cannot run in this build; it is kept and skipped",
			map[string]any{"trigger_id": safeLabel(spec.ID), "kind": safeLabel(spec.Kind), "reason": e.skip})
	}
	e.updatedMs = row.updatedMs
	e.stateLoaded = false // read on the first evaluation
	e.errs, e.persistedEr = row.errors, row.errors
	return e
}

func skipReason(err error) string {
	switch {
	case errors.Is(err, ErrUnknownKind):
		return "unknown kind"
	default:
		return "invalid spec"
	}
}

// build runs a spec's factory and checks what comes back. spec must be valid in
// shape (validateSpec).
func (r *Registry) build(spec Spec) (*entry, error) {
	f, ok := r.factories[spec.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, spec.Kind)
	}
	trig, err := runFactory(f, spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	if trig == nil {
		return nil, fmt.Errorf("%w: the factory returned no trigger", ErrInvalidSpec)
	}
	tid := trig.ID()
	if want := spec.Kind + ":" + spec.ID; tid != want {
		return nil, fmt.Errorf("%w: trigger id %q, want %q", ErrInvalidSpec, tid, want)
	}
	kinds := dedupeKinds(trig.Kinds())
	if len(kinds) == 0 {
		return nil, fmt.Errorf("%w: the trigger lists no event kinds", ErrInvalidSpec)
	}
	ds := slices.Clone(trig.Demands())
	if err := validateDemands(ds); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	return &entry{
		spec: spec, trig: trig, tid: tid, kinds: kinds, demands: ds,
		updatedMs: spec.UpdatedAt.UnixMilli(), stateLoaded: true,
	}, nil
}

func runFactory(f Factory, spec Spec) (t Trigger, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			t, err = nil, fmt.Errorf("the factory panicked: %v", rec)
		}
	}()
	// The factory gets its own copy of the parameters.
	spec.Params = bytes.Clone(spec.Params)
	return f(spec)
}

func dedupeKinds(ks []events.Kind) []events.Kind {
	var out []events.Kind
	for _, k := range ks {
		if k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func newSnapshot(es []*entry) *snapshot {
	s := &snapshot{byID: make(map[string]*entry, len(es)), byKind: map[events.Kind][]*entry{}}
	s.ordered = slices.Clone(es)
	sort.Slice(s.ordered, func(i, j int) bool { return s.ordered[i].spec.ID < s.ordered[j].spec.ID })
	for _, e := range s.ordered {
		s.byID[e.spec.ID] = e
		for _, k := range e.kinds {
			s.byKind[k] = append(s.byKind[k], e)
		}
	}
	return s
}

// stamp returns the registry's time at millisecond precision, the precision of the
// table.
func (r *Registry) stamp() time.Time { return r.now().Truncate(time.Millisecond) }

// Add validates and stores a new spec and returns it with its ID and stamps filled
// in. The spec's kind must have a factory, its parameters must be at most
// MaxParamsBytes of JSON that the factory accepts, its mode must be one of off,
// shadow and live, and its origin non-empty.
func (r *Registry) Add(ctx context.Context, s Spec) (Spec, error) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	if s.ID == "" {
		s.ID = r.newID()
	}
	if len(bytes.TrimSpace(s.Params)) == 0 {
		s.Params = json.RawMessage("{}")
	}
	s.Params = bytes.Clone(s.Params)
	s.CreatedAt = r.stamp()
	s.UpdatedAt = s.CreatedAt
	if err := validateSpec(s); err != nil {
		return Spec{}, err
	}
	cur := r.snap.Load()
	if _, dup := cur.byID[s.ID]; dup {
		return Spec{}, fmt.Errorf("%w: %s", ErrExists, s.ID)
	}
	if len(cur.ordered) >= MaxSpecs {
		return Spec{}, fmt.Errorf("%w: the limit is %d", ErrTooMany, MaxSpecs)
	}
	e, err := r.build(s)
	if err != nil {
		return Spec{}, err
	}
	if err := insertSpec(ctx, r.db, s); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Spec{}, fmt.Errorf("%w: %s", ErrExists, s.ID)
		}
		return Spec{}, fmt.Errorf("triggers: add: %w", err)
	}
	r.snap.Store(newSnapshot(append(slices.Clone(cur.ordered), e)))
	return cloneSpec(s), nil
}

// Update replaces the spec with s.ID (its kind, parameters, intent, mode and
// origin). The new spec is validated like an added one. Its state is kept when the
// kind and the parameters did not change and cleared otherwise; its error count
// starts again from zero, which is how a spec that was switched off after repeated
// errors is turned back on. An evaluation that was running for the old spec when
// Update was called may lose its state change; the trigger then re-evaluates from
// the older state and attention's dedup absorbs the repeat.
func (r *Registry) Update(ctx context.Context, s Spec) error {
	return r.update(ctx, s, false)
}

// UpdateFresh is Update that always starts from nothing: the spec's state is
// cleared even when its kind and parameters did not change. It is how a spec
// leaves a shadow rehearsal for live (or goes back): what a trigger learned while
// its candidates were only logged, such as "already fired" or "on the far side
// of the line", must not decide what it does when they are delivered.
func (r *Registry) UpdateFresh(ctx context.Context, s Spec) error {
	return r.update(ctx, s, true)
}

func (r *Registry) update(ctx context.Context, s Spec, fresh bool) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	cur := r.snap.Load()
	old, ok := cur.byID[s.ID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, s.ID)
	}
	if len(bytes.TrimSpace(s.Params)) == 0 {
		s.Params = json.RawMessage("{}")
	}
	s.Params = bytes.Clone(s.Params)

	old.mu.Lock()
	prev, prevMs := old.spec, old.updatedMs
	prevState, prevLoaded := old.state, old.stateLoaded
	old.mu.Unlock()

	s.CreatedAt = prev.CreatedAt
	s.UpdatedAt = r.stamp()
	if s.UpdatedAt.UnixMilli() <= prevMs {
		s.UpdatedAt = time.UnixMilli(prevMs + 1) // the guard of the pipeline's writes is the stamp
	}
	if err := validateSpec(s); err != nil {
		return err
	}
	e, err := r.build(s)
	if err != nil {
		return err
	}
	keep := !fresh && s.Kind == prev.Kind && bytes.Equal(s.Params, prev.Params)
	if keep {
		e.stateLoaded, e.state = prevLoaded, bytes.Clone(prevState)
	}
	if _, err := replaceSpec(ctx, r.db, s, keep); err != nil {
		return fmt.Errorf("triggers: update: %w", err)
	}
	next := make([]*entry, 0, len(cur.ordered))
	for _, x := range cur.ordered {
		if x != old {
			next = append(next, x)
		}
	}
	r.snap.Store(newSnapshot(append(next, e)))
	return nil
}

// Remove deletes a spec and its state.
func (r *Registry) Remove(ctx context.Context, id string) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	cur := r.snap.Load()
	old, ok := cur.byID[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if _, err := deleteSpec(ctx, r.db, id); err != nil {
		return fmt.Errorf("triggers: remove: %w", err)
	}
	next := make([]*entry, 0, len(cur.ordered))
	for _, x := range cur.ordered {
		if x != old {
			next = append(next, x)
		}
	}
	r.snap.Store(newSnapshot(next))
	return nil
}

// List returns the specs that match f, oldest first. It reads memory, not the
// database. Specs this build cannot run are listed too.
func (r *Registry) List(_ context.Context, f ListFilter) ([]Spec, error) {
	var out []Spec
	for _, e := range r.snap.Load().ordered {
		e.mu.Lock()
		s := cloneSpec(e.spec)
		e.mu.Unlock()
		if f.Kind != "" && s.Kind != f.Kind || f.Mode != "" && s.Mode != f.Mode ||
			f.IntentID != "" && s.IntentID != f.IntentID || !strings.HasPrefix(s.Origin, f.Origin) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func cloneSpec(s Spec) Spec {
	s.Params = bytes.Clone(s.Params)
	return s
}

// Demands implements perception.DemandSource: the de-duplicated union of what the
// evaluated triggers need, in a stable order. A spec whose effective mode is off
// (its own, or the global one) contributes nothing, and with the global mode off
// the result is empty, so perception makes no request for triggers that cannot
// run. It is safe to call from any goroutine; the slice is the caller's.
func (r *Registry) Demands() []perception.Demand {
	g := globalRank(r.globalMode())
	if g == 0 {
		return nil
	}
	seen := map[perception.Demand]struct{}{}
	var out []perception.Demand
	for _, e := range r.snap.Load().ordered {
		if e.trig == nil {
			continue
		}
		e.mu.Lock()
		eff := effectiveRank(g, e.spec.Mode)
		e.mu.Unlock()
		if eff == 0 {
			continue
		}
		for _, d := range e.demands {
			if _, dup := seen[d]; !dup {
				seen[d] = struct{}{}
				out = append(out, d)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return demandLess(out[i], out[j]) })
	return out
}

func demandLess(a, b perception.Demand) bool {
	switch {
	case a.Provider != b.Provider:
		return a.Provider < b.Provider
	case a.Account != b.Account:
		return a.Account < b.Account
	case a.Symbol != b.Symbol:
		return a.Symbol < b.Symbol
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	case a.Timeframe != b.Timeframe:
		return a.Timeframe < b.Timeframe
	case a.MinInterval != b.MinInterval:
		return a.MinInterval < b.MinInterval
	}
	return a.Lookback < b.Lookback
}

var _ perception.DemandSource = (*Registry)(nil)

// GlobalMode is the global triggers mode as the registry reads it now: "off",
// "shadow" or "live", with anything else reported as "off", as evaluation counts
// it. A caller that must act differently when triggers deliver (the alert tools)
// asks the registry rather than the configuration, so that "live" means a live
// registry is actually running.
func (r *Registry) GlobalMode() string {
	switch globalRank(r.globalMode()) {
	case 2:
		return ModeLive
	case 1:
		return ModeShadow
	}
	return ModeOff
}

// SetStoredMode switches the stored spec id to mode without a registry: for a
// generation that has none and must still stop an earlier one's spec from being
// taken for active (see migrate). It changes the mode and the stamp and nothing else,
// reports whether a row was written, and never creates a table. now is the stamp.
func SetStoredMode(ctx context.Context, db *sql.DB, id, mode string, now time.Time) (bool, error) {
	if _, ok := modeRank(mode); !ok {
		return false, fmt.Errorf("%w: mode %q", ErrInvalidSpec, mode)
	}
	if db == nil {
		return false, nil
	}
	res, err := db.ExecContext(ctx, `UPDATE triggers SET mode = ?, updated_ms = max(updated_ms + 1, ?) WHERE id = ?`,
		mode, now.UnixMilli(), id)
	if err != nil {
		return false, fmt.Errorf("triggers: set mode: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteStored removes the stored spec id without a registry; see SetStoredMode.
func DeleteStored(ctx context.Context, db *sql.DB, id string) (bool, error) {
	if db == nil {
		return false, nil
	}
	return deleteSpec(ctx, db, id)
}

// ReadSpecs lists the specs stored in db that match f, without a registry: it opens
// nothing, creates nothing and reads nothing but the specs, so it is safe to call
// in a generation that did not build a registry, to learn what an earlier one left.
// It returns nil when the triggers table does not exist.
func ReadSpecs(ctx context.Context, db *sql.DB, f ListFilter) ([]Spec, error) {
	if db == nil {
		return nil, nil
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'triggers'`).Scan(&n); err != nil {
		return nil, fmt.Errorf("triggers: read: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	rows, _, err := loadRows(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []Spec
	for _, row := range rows {
		s := row.spec
		if f.Kind != "" && s.Kind != f.Kind || f.Mode != "" && s.Mode != f.Mode ||
			f.IntentID != "" && s.IntentID != f.IntentID || !strings.HasPrefix(s.Origin, f.Origin) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// warn logs msg at most once per warnEvery for each key.
func (r *Registry) warn(key, msg string, fields map[string]any) {
	now := r.now()
	r.warnMu.Lock()
	last, seen := r.warned[key]
	due := !seen || now.Sub(last) >= warnEvery
	if due {
		if len(r.warned) >= maxWarnKeys {
			clear(r.warned)
		}
		r.warned[key] = now
	}
	r.warnMu.Unlock()
	if due {
		logger.WarnCF("triggers", msg, fields)
	}
}

// safeLabel makes outside text fit in a log field: printable, short.
func safeLabel(s string) string {
	const max = 120
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
