package worldmodel

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel/detect"
)

// Store is the world model: in-memory state, a bounded change log, and checkpoints
// of both into the shared proactive database.
//
// Locking rule (see the package comment): one reader-writer lock, mu, guards every
// field below it. Apply is called from ONE goroutine, the pipeline's; it decodes
// and validates with no lock held and takes the write lock only to mutate. View
// methods, SinceMark, SetMark, Cursor and Stats may be called from any goroutine;
// the View methods take the read lock and return copies. Nothing holds mu across a
// database call, a callback (the zone function, the clock) or a log write. The
// checkpoint takes its own lock, ckMu, which serialises checkpoints with each
// other and with nothing else.
type Store struct {
	clock perception.Clock
	db    *sql.DB
	zone  func() *time.Location

	mu      sync.RWMutex
	series  map[seriesKey]*series
	pf      *portfolioState
	futures map[string]*futuresAccount
	plans   map[planKey]*planState
	fills   map[fillKey]*fillState
	sources map[string]*sourceState
	day     dayHint
	lastLoc *time.Location // the last time zone the zone function gave

	// The change log. seq is the last sequence number assigned; entries with a
	// sequence number above flushedSeq are in buf and not yet in the database;
	// lowSeq is the lowest sequence number that can still be read (everything
	// below it was pruned or dropped).
	seq        int64
	buf        []logEntry
	flushedSeq int64
	lowSeq     int64
	marks      map[string]markState

	// What changed since the last checkpoint, each with the version of its last
	// change so that a checkpoint clears only what it wrote.
	ver        uint64
	dirtyRows  map[rowKey]uint64
	dirtyDets  map[detKey]uint64
	dirtyMarks map[string]uint64

	// Checkpointing.
	ckMu     sync.Mutex
	lifeMu   sync.Mutex // guards closed and wg.Add
	closed   bool
	wg       sync.WaitGroup
	lifeCtx  context.Context
	lifeStop context.CancelFunc
	ckBusy   atomic.Bool
	ckActive atomic.Int32 // Checkpoint calls in progress, those waiting on ckMu included (for Idle)
	// noteDropped and noteEvicted carry a warning out of a locked section: the log
	// line is written after the lock is released.
	noteDropped atomic.Bool
	noteEvicted atomic.Bool
	nextCk      atomic.Int64 // unix nanos: earliest time Housekeep starts the next one
	lastCk      atomic.Int64 // unix nanos of the last successful checkpoint

	c counters

	warnMu sync.Mutex
	warned map[string]time.Time
}

type counters struct {
	applied, ignored, droppedInvalid, droppedBadName, droppedOutOfOrder atomic.Uint64
	droppedPaper, decodeErrors, panics, futureClamped, truncated        atomic.Uint64
	seriesEvicted, factsEvicted, changesLogged, changesDropped          atomic.Uint64
	snapshotsRebased, checkpoints, checkpointFailures, restoredRows     atomic.Uint64
	detectorsReset                                                      atomic.Uint64
}

// Option configures NewStore.
type Option func(*Store)

// WithZone sets where the owner's time zone comes from. It is called, with no lock
// held, when a portfolio snapshot arrives, to decide which local day the snapshot
// belongs to; it may read the profile and may return nil when it cannot say, in
// which case the last zone it gave (or the zone of the last day_start event, or
// UTC) is used. It must not call back into the Store.
func WithZone(f func() *time.Location) Option { return func(s *Store) { s.zone = f } }

// NewStore opens the world model. With a database it migrates the worldmodel
// tables and loads what the last run checkpointed (facts keep their original
// timestamps; detector state older than its bound is reset). With db == nil the
// store is memory-only: nothing is loaded or written.
func NewStore(ctx context.Context, db *sql.DB, clock perception.Clock, opts ...Option) (*Store, error) {
	if clock == nil {
		clock = perception.RealClock{}
	}
	s := &Store{
		clock: clock, db: db,
		series:     map[seriesKey]*series{},
		futures:    map[string]*futuresAccount{},
		plans:      map[planKey]*planState{},
		fills:      map[fillKey]*fillState{},
		sources:    map[string]*sourceState{},
		marks:      map[string]markState{},
		dirtyRows:  map[rowKey]uint64{},
		dirtyDets:  map[detKey]uint64{},
		dirtyMarks: map[string]uint64{},
		warned:     map[string]time.Time{},
	}
	for _, o := range opts {
		o(s)
	}
	s.lifeCtx, s.lifeStop = context.WithCancel(context.Background())
	if db != nil {
		if err := s.open(ctx); err != nil {
			s.lifeStop()
			return nil, err
		}
	}
	now := clock.Now()
	s.lastCk.Store(now.UnixNano())
	s.nextCk.Store(now.Add(CheckpointEvery).UnixNano())
	return s, nil
}

// Close stops the checkpoint worker, waits for one in flight, and writes a final
// checkpoint with ctx. It is safe to call twice. After Close, Apply still works
// on memory but nothing is written again.
func (s *Store) Close(ctx context.Context) error {
	s.lifeMu.Lock()
	if s.closed {
		s.lifeMu.Unlock()
		return nil
	}
	s.closed = true
	s.lifeMu.Unlock()
	s.lifeStop()
	s.wg.Wait()
	return s.Checkpoint(ctx)
}

// Housekeep is the store's per-second call on the pipeline goroutine (the engine's
// Housekeeper). It only decides whether a checkpoint is due; the write runs on a
// goroutine of its own, at most one at a time, bounded by checkpointTimeout, so
// the pipeline never waits for the database. A failed checkpoint is retried after
// a minute; the state it could not write stays marked as changed.
func (s *Store) Housekeep(_ context.Context, now time.Time) {
	if s.db == nil || now.UnixNano() < s.nextCk.Load() || s.ckBusy.Load() {
		return
	}
	s.lifeMu.Lock()
	if s.closed || !s.ckBusy.CompareAndSwap(false, true) {
		s.lifeMu.Unlock()
		return
	}
	s.wg.Add(1)
	s.lifeMu.Unlock()
	s.nextCk.Store(now.Add(CheckpointEvery).UnixNano())
	go func() {
		defer s.wg.Done()
		defer s.ckBusy.Store(false)
		defer func() {
			if r := recover(); r != nil {
				s.c.panics.Add(1)
				s.warn("checkpoint-panic", "World model checkpoint panicked", fmt.Errorf("%v", r))
				s.nextCk.Store(now.Add(checkpointRetry).UnixNano())
			}
		}()
		ctx, cancel := context.WithTimeout(s.lifeCtx, checkpointTimeout)
		defer cancel()
		if err := s.Checkpoint(ctx); err != nil {
			s.nextCk.Store(now.Add(checkpointRetry).UnixNano())
		}
	}()
}

// Idle is true when no checkpoint is running or waiting to run. The next one, five
// minutes away, is a timer. It changes nothing; only the sandbox lab calls it.
func (s *Store) Idle() bool { return !s.ckBusy.Load() && s.ckActive.Load() == 0 }

// Stats returns the counters.
func (s *Store) Stats() Stats {
	st := Stats{
		Applied: s.c.applied.Load(), Ignored: s.c.ignored.Load(),
		DroppedInvalid: s.c.droppedInvalid.Load(), DroppedBadName: s.c.droppedBadName.Load(),
		DroppedOutOfOrder: s.c.droppedOutOfOrder.Load(), DroppedPaper: s.c.droppedPaper.Load(),
		DecodeErrors: s.c.decodeErrors.Load(), Panics: s.c.panics.Load(),
		FutureClamped: s.c.futureClamped.Load(), Truncated: s.c.truncated.Load(),
		SeriesEvicted: s.c.seriesEvicted.Load(), FactsEvicted: s.c.factsEvicted.Load(),
		ChangesLogged: s.c.changesLogged.Load(), ChangesDropped: s.c.changesDropped.Load(),
		SnapshotsRebased: s.c.snapshotsRebased.Load(),
		Checkpoints:      s.c.checkpoints.Load(), CheckpointFailures: s.c.checkpointFailures.Load(),
		RestoredRows: s.c.restoredRows.Load(), DetectorsReset: s.c.detectorsReset.Load(),
	}
	if n := s.lastCk.Load(); n != 0 {
		st.LastCheckpoint = time.Unix(0, n).UTC()
	}
	s.mu.RLock()
	st.Series = len(s.series)
	st.BufferedLog = len(s.buf)
	st.DirtyRows = len(s.dirtyRows) + len(s.dirtyDets) + len(s.dirtyMarks)
	s.mu.RUnlock()
	return st
}

// warn logs at most once per warnEvery for each key.
func (s *Store) warn(key, msg string, err error) {
	now := s.clock.Now()
	s.warnMu.Lock()
	last, seen := s.warned[key]
	due := !seen || now.Sub(last) >= warnEvery
	if due {
		s.warned[key] = now
	}
	s.warnMu.Unlock()
	if due {
		f := map[string]any{"key": key}
		if err != nil {
			f["error"] = err.Error()
		}
		logger.WarnCF("worldmodel", msg, f)
	}
}

// ---- state types ----------------------------------------------------------

type seriesKey struct{ provider, symbol string }

func (k seriesKey) subject() string { return "price:" + k.provider + ":" + k.symbol }

// trustTrack keeps the lowest trust among the ticks of the open bucket and of the
// last closed one, so a bucket-close change can carry the minimum trust of its
// two inputs.
type trustTrack struct {
	cur, prev proactive.Trust
	started   bool
}

// observe folds in a tick of trust tr. closed says the tick opened a new bucket,
// and the returned trust is that of the bucket that closed (its own ticks and the
// close before it).
func (t *trustTrack) observe(tr proactive.Trust, closed bool) proactive.Trust {
	switch {
	case !t.started:
		t.cur, t.prev, t.started = tr, tr, true
		return tr
	case closed:
		out := proactive.MinTrust(t.prev, t.cur)
		t.prev, t.cur = t.cur, tr
		return out
	default:
		t.cur = proactive.MinTrust(t.cur, tr)
		return t.cur
	}
}

type series struct {
	price float64
	at    time.Time
	trust proactive.Trust
	src   string

	m1, h1 detect.ReturnSeries
	cusum  detect.CUSUM

	z1, zh   float64
	z1ok     bool
	zhok     bool
	tr1, trh trustTrack
}

func newSeries() *series {
	return &series{m1: newMinuteSeries(), h1: newHourSeries(), cusum: newCUSUM()}
}

type portfolioState struct {
	At         time.Time
	SnapshotID int64
	TotalUSD   float64
	ByClass    map[string]float64
	Positions  []PositionView
	Truncated  bool

	// Key identifies the set of unpriced assets the values exclude; UnpricedN is
	// its size. Two snapshots with different keys are not comparable.
	Key               string
	UnpricedN         int
	UnpricedTruncated bool

	DayDate         string
	DayStartUSD     float64
	DayStartAt      time.Time
	DayKey          string
	DayStartRebased bool

	// Peaks are the highest totals of up to 31 local days, oldest first, all on the
	// footing of PeakKey.
	Peaks   []dayPeak
	PeakKey string

	Trust proactive.Trust
	Src   string
}

type dayPeak struct {
	Date string  `json:"date"`
	Max  float64 `json:"max"`
}

type futuresAccount struct {
	at        time.Time
	trust     proactive.Trust
	src       string
	positions map[string]FuturesView
}

type planKey struct {
	kind string
	id   int64
}

type planState struct {
	view PlanView
	src  string
}

type fillKey struct{ account, symbol string }

type fillState struct {
	view FillView
	desc string
	src  string
}

type sourceState struct {
	lastEvent   time.Time
	lastErr     time.Time
	reason      string
	whole       bool                // degraded as a whole, not by account
	failed      map[string]struct{} // accounts or providers that cannot be read
	consecutive int
}

func (st *sourceState) degraded() bool { return st.whole || len(st.failed) > 0 }

type dayHint struct{ Date, Zone string }

type markState struct {
	seq  int64
	atMs int64
}

// evictOldest removes the n entries of m with the oldest stamp (ties broken by
// the key's order, so the choice is deterministic) and returns their keys.
func evictOldest[K comparable, V any](m map[K]V, n int, at func(V) time.Time, less func(a, b K) bool) []K {
	type kv struct {
		k K
		t time.Time
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, at(v)})
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].t.Equal(all[j].t) {
			return all[i].t.Before(all[j].t)
		}
		return less(all[i].k, all[j].k)
	})
	if n > len(all) {
		n = len(all)
	}
	out := make([]K, 0, n)
	for _, e := range all[:n] {
		delete(m, e.k)
		out = append(out, e.k)
	}
	return out
}
