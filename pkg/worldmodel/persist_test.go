package worldmodel

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func tableCount(t testing.TB, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestMigration_FromAnEmptyDatabase(t *testing.T) {
	db := openDB(t)
	for _, tbl := range []string{"wm_facts", "wm_detectors", "wm_changes", "wm_marks"} {
		if _, err := db.Exec(`SELECT 1 FROM ` + tbl); err == nil {
			t.Fatalf("%s exists before the store was built", tbl)
		}
	}
	s := newDBStore(t, db, newClock())
	_ = s
	for _, tbl := range []string{"wm_facts", "wm_detectors", "wm_changes", "wm_marks"} {
		if n := tableCount(t, db, tbl); n != 0 {
			t.Fatalf("%s has %d rows", tbl, n)
		}
	}
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, Component).Scan(&v); err != nil || v != 1 {
		t.Fatalf("schema version = %d, %v", v, err)
	}
	// A second store over the same database migrates nothing and loads nothing.
	if _, err := NewStore(context.Background(), db, newClock()); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.Migrate(context.Background(), db, migrations...); err != nil {
		t.Fatalf("migrations are not idempotent: %v", err)
	}
}

// populate applies a mixed history: 40 minutes of ticks, a snapshot, futures, a
// plan and a fill, and returns the time of the last tick.
func populate(t testing.TB, s *Store) time.Time {
	t.Helper()
	feedMinutes(t, s, t0, "binance", "BTC/USDT", 40, noisy)
	apply(t, s, snapshotEvent(t0.Add(41*time.Minute), 1000))
	apply(t, s, snapshotEvent(t0.Add(42*time.Minute), 1100))
	apply(t, s, futuresEvent(t0.Add(43*time.Minute), "okx", "main", longBTC(135)))
	apply(t, s, events.Event{Kind: events.KindPerceptionPlanHealth, Time: t0.Add(44 * time.Minute), Trust: events.TrustPtr(proactive.TrustSystem),
		Payload: PlanPayload{Kind: "dn", ID: 2, Status: "healthy", Health: map[string]float64{"hedge_ratio": 0.99}}})
	apply(t, s, fillEvent(t0.Add(45*time.Minute), orderfeed.OrderEvent{
		Provider: "binance", Account: "main", Symbol: "BTC/USDT", Side: "buy", Status: "closed",
		Filled: 0.5, FilledKnown: true, AvgPrice: 62000, AvgPriceKnown: true, AmountUnit: orderfeed.UnitBase, At: t0.Add(45 * time.Minute),
	}))
	return t0.Add(39*time.Minute + 50*time.Second)
}

func TestCheckpointRestore(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	lastTick := populate(t, s)
	ctx := context.Background()
	if err := s.SetMark(ctx, "proactive"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, snapshotEvent(t0.Add(46*time.Minute), 1300)) // a change after the mark
	before, _ := s.View().Series("price:binance:BTC/USDT")
	if before.Samples < 20 {
		t.Fatalf("not warmed up: %+v", before)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if tableCount(t, db, "wm_facts") != 5 || tableCount(t, db, "wm_detectors") != 3 {
		t.Fatalf("facts %d detectors %d", tableCount(t, db, "wm_facts"), tableCount(t, db, "wm_detectors"))
	}

	// Ten minutes later: a restart with the process down for a short while.
	clk2 := perceptiontest.NewClock(t0.Add(56 * time.Minute))
	s2 := newDBStore(t, db, clk2)
	v := s2.View()

	p, at, ok := v.Price("binance", "BTC/USDT")
	if !ok || p != noisy(39) || !at.Equal(lastTick) {
		t.Fatalf("restored price %v at %v (ok=%v), want %v at %v: a restored fact keeps its own timestamp", p, at, ok, noisy(39), lastTick)
	}
	if v.Fresh(at, time.Minute) {
		t.Fatal("a price restored 16 minutes old must not be fresh for a minute")
	}
	if tr, _ := v.PriceTrust("binance", "BTC/USDT"); tr != proactive.TrustExchange {
		t.Fatalf("trust = %v", tr)
	}
	pv, ok := v.Portfolio()
	if !ok || pv.TotalUSD != 1300 || !pv.At.Equal(t0.Add(46*time.Minute)) || pv.DayStartUSD != 1000 || pv.Peak30dUSD != 1300 || pv.DayDate != "2026-03-10" {
		t.Fatalf("portfolio = %+v", pv)
	}
	if pv.Trust != proactive.TrustSystem || len(pv.Positions) != 2 {
		t.Fatalf("%+v", pv)
	}
	fv, ok := v.FuturesPosition("okx/main", "BTC/USDT:USDT")
	if !ok || fv.Contracts != 135 || !fv.At.Equal(t0.Add(43*time.Minute)) || fv.Trust != proactive.TrustExchange || !fv.NotionalKnown {
		t.Fatalf("futures = %+v", fv)
	}
	pl, ok := v.Plan("dn", 2)
	if !ok || pl.Status != "healthy" || pl.Health["hedge_ratio"] != 0.99 || !pl.At.Equal(t0.Add(44*time.Minute)) {
		t.Fatalf("plan = %+v", pl)
	}
	if lf, ok := v.LastFill("binance/main", "BTC/USDT"); !ok || lf.Filled != 0.5 || !lf.At.Equal(t0.Add(45*time.Minute)) {
		t.Fatalf("fill = %+v", lf)
	}

	// 16 minutes is inside the one-minute detector's 30-minute bound: kept.
	after, _ := v.Series("price:binance:BTC/USDT")
	if after.Samples != before.Samples || after.Mean != before.Mean || after.Sigma != before.Sigma {
		t.Fatalf("detector state changed across a short restart:\n%+v\n%+v", before, after)
	}
	if s2.Stats().DetectorsReset != 0 {
		t.Fatalf("stats = %+v", s2.Stats())
	}

	// The mark and the log survived: the post-mark change is still there.
	d, err := s2.SinceMark(ctx, "proactive")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Changes) == 0 || d.Changes[0].Subject != "portfolio" {
		t.Fatalf("since mark after restart = %+v", d)
	}
	// And the sequence continues instead of starting over.
	if s2.Cursor() != s.Cursor() {
		t.Fatalf("cursor %d after restart, was %d", s2.Cursor(), s.Cursor())
	}

	// The next tick refreshes the fact.
	apply(t, s2, tickAt(t0.Add(57*time.Minute), "binance", "BTC/USDT", 101))
	if _, at, _ := v.Price("binance", "BTC/USDT"); !at.Equal(t0.Add(57 * time.Minute)) {
		t.Fatalf("not refreshed: %v", at)
	}
}

func TestCheckpointRestore_StaleDetectorsAreReset(t *testing.T) {
	db := openDB(t)
	s := newDBStore(t, db, newClock())
	lastTick := populate(t, s)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Two hours on: beyond the minute series' 30-minute bound, inside the hour
	// series' six hours.
	s2 := newDBStore(t, db, perceptiontest.NewClock(t0.Add(2*time.Hour+45*time.Minute)))
	v := s2.View()
	m, _ := v.Series("price:binance:BTC/USDT@1m")
	if m.Samples != 0 || m.CUSUMPos != 0 || m.LastZValid {
		t.Fatalf("a stale one-minute detector must be reset, not trusted: %+v", m)
	}
	if s2.Stats().DetectorsReset < 2 {
		t.Fatalf("stats = %+v", s2.Stats())
	}
	// The price fact is kept, and honest about its age.
	if _, at, ok := v.Price("binance", "BTC/USDT"); !ok || !at.Equal(lastTick) {
		t.Fatalf("price at %v ok=%v", at, ok)
	}
	// The reset series restarts cleanly.
	feedMinutes(t, s2, t0.Add(3*time.Hour), "binance", "BTC/USDT", 3, noisy)
	if m, _ = v.Series("price:binance:BTC/USDT@1m"); m.Samples == 0 {
		t.Fatalf("the series did not restart: %+v", m)
	}

	// A day later the hour series is stale too.
	s3 := newDBStore(t, db, perceptiontest.NewClock(t0.Add(30*time.Hour)))
	if h, _ := s3.View().Series("price:binance:BTC/USDT@1h"); h.Samples != 0 {
		t.Fatalf("%+v", h)
	}
}

func TestCheckpoint_FailedCheckpointIsRetriedWithoutLoss(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))
	apply(t, s, snapshotEvent(t0.Add(time.Minute), 1100))
	if err := s.SetMark(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, snapshotEvent(t0.Add(2*time.Minute), 1200))
	dirtyBefore := s.Stats().DirtyRows

	// Break the database underneath the store: the checkpoint's transaction must
	// fail and roll back whole.
	if _, err := db.Exec(`ALTER TABLE wm_marks RENAME TO wm_marks_broken`); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx); err == nil {
		t.Fatal("expected the checkpoint to fail")
	}
	st := s.Stats()
	if st.CheckpointFailures != 1 || st.Checkpoints != 0 || st.BufferedLog == 0 || st.DirtyRows != dirtyBefore {
		t.Fatalf("a failed checkpoint must change nothing in memory: %+v (dirty was %d)", st, dirtyBefore)
	}
	if tableCount(t, db, "wm_facts") != 0 || tableCount(t, db, "wm_changes") != 0 {
		t.Fatal("a failed checkpoint left a partial write")
	}
	// The in-memory state is still authoritative and still answers.
	if pv, _ := s.View().Portfolio(); pv.TotalUSD != 1200 {
		t.Fatalf("%+v", pv)
	}
	if d, err := s.SinceMark(ctx, "m"); err != nil || len(d.Changes) == 0 {
		t.Fatalf("mark lost: %+v %v", d, err)
	}

	// Mend it; the retry writes everything.
	if _, err := db.Exec(`ALTER TABLE wm_marks_broken RENAME TO wm_marks`); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	st = s.Stats()
	if st.Checkpoints != 1 || st.BufferedLog != 0 || st.DirtyRows != 0 {
		t.Fatalf("%+v", st)
	}
	if tableCount(t, db, "wm_facts") != 1 || tableCount(t, db, "wm_changes") == 0 || tableCount(t, db, "wm_marks") != 1 {
		t.Fatal("rows missing after the retry")
	}
	s2 := newDBStore(t, db, clk)
	if pv, _ := s2.View().Portfolio(); pv.TotalUSD != 1200 || pv.DayStartUSD != 1000 {
		t.Fatalf("%+v", pv)
	}
	if d, _ := s2.SinceMark(ctx, "m"); len(d.Changes) == 0 {
		t.Fatal("log lost across the failed checkpoint")
	}
}

// TestSinceMark_Aggregates runs the same script against a memory-only store, a
// store whose log is entirely in the database and one whose log is split between
// the two; the answer must be the same.
func TestSinceMark_Aggregates(t *testing.T) {
	for name, mode := range map[string]string{"memory": "mem", "flushed": "flushed", "split": "split"} {
		t.Run(name, func(t *testing.T) {
			clk := newClock()
			var s *Store
			if mode == "mem" {
				s = newMemStore(t, clk)
			} else {
				s = newDBStore(t, openDB(t), clk)
			}
			ctx := context.Background()
			apply(t, s, snapshotEvent(t0, 1000))
			if err := s.SetMark(ctx, "m"); err != nil {
				t.Fatal(err)
			}
			apply(t, s, snapshotEvent(t0.Add(time.Minute), 1100))
			if mode != "mem" {
				if err := s.Checkpoint(ctx); err != nil {
					t.Fatal(err)
				}
			}
			apply(t, s, snapshotEvent(t0.Add(2*time.Minute), 1200))
			if mode == "flushed" {
				if err := s.Checkpoint(ctx); err != nil {
					t.Fatal(err)
				}
			}

			d, err := s.SinceMark(ctx, "m")
			if err != nil {
				t.Fatal(err)
			}
			if d.Truncated {
				t.Fatal("not truncated")
			}
			var total *Change
			for i := range d.Changes {
				if d.Changes[i].Key == "total_usd" {
					total = &d.Changes[i]
				}
			}
			// Two moves, 1000 -> 1100 -> 1200, read as one.
			if total == nil || total.Old != 1000.0 || total.New != 1200.0 || total.Trust != proactive.TrustSystem {
				t.Fatalf("aggregate = %+v in %+v", total, d.Changes)
			}
			for i := 1; i < len(d.Changes); i++ {
				a, b := d.Changes[i-1], d.Changes[i]
				if a.Subject > b.Subject || (a.Subject == b.Subject && a.Key >= b.Key) {
					t.Fatalf("not sorted: %+v", d.Changes)
				}
			}

			// A fact that ends where it started disappears.
			apply(t, s, snapshotEvent(t0.Add(3*time.Minute), 1000))
			d, _ = s.SinceMark(ctx, "m")
			if !d.Empty() {
				t.Fatalf("everything reverted, yet %+v", d)
			}
		})
	}
}

func TestSinceMark_QuietHourIsEmpty(t *testing.T) {
	for _, db := range []*sql.DB{nil, openDB(t)} {
		clk := newClock()
		s, err := NewStore(context.Background(), db, clk, WithZone(zoneBKK))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		apply(t, s, snapshotEvent(t0, 1000))
		feedMinutes(t, s, t0, "binance", "BTC/USDT", 5, func(int) float64 { return 100 })
		if err := s.SetMark(ctx, "proactive"); err != nil {
			t.Fatal(err)
		}
		// An hour of the same price, and a snapshot of the same value.
		feedMinutes(t, s, t0.Add(5*time.Minute), "binance", "BTC/USDT", 60, func(int) float64 { return 100 })
		apply(t, s, snapshotEvent(t0.Add(30*time.Minute), 1000))
		clk.Advance(0)
		d, err := s.SinceMark(ctx, "proactive")
		if err != nil {
			t.Fatal(err)
		}
		if !d.Empty() || d.Truncated {
			t.Fatalf("a quiet hour reported %+v", d)
		}
	}
}

func TestSinceMark_UnknownMarkAndBadNames(t *testing.T) {
	s := newMemStore(t, newClock())
	if _, err := s.SinceMark(context.Background(), "nope"); err != ErrNoMark {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{"", "has space", "x\nnl", string(make([]byte, 100))} {
		if err := s.SetMark(context.Background(), bad); err == nil {
			t.Fatalf("mark %q accepted", bad)
		}
	}
	if err := s.EnsureMark(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, snapshotEvent(t0, 1000))
	apply(t, s, snapshotEvent(t0.Add(time.Minute), 1100))
	if err := s.EnsureMark(context.Background(), "a"); err != nil { // does not move it
		t.Fatal(err)
	}
	if d, _ := s.SinceMark(context.Background(), "a"); d.Empty() {
		t.Fatal("EnsureMark moved an existing mark")
	}
	// SetMarkAt only moves forward, never past the cursor.
	if err := s.SetMarkAt(context.Background(), "a", 1<<40); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.SinceMark(context.Background(), "a"); !d.Empty() {
		t.Fatalf("%+v", d)
	}
	if err := s.SetMarkAt(context.Background(), "a", 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.SinceMark(context.Background(), "a"); !d.Empty() {
		t.Fatal("a mark moved backwards")
	}
	for i := 0; i < MaxMarks+3; i++ {
		_ = s.SetMark(context.Background(), "mark"+itoa(i))
	}
	s.mu.RLock()
	n := len(s.marks)
	s.mu.RUnlock()
	if n > MaxMarks {
		t.Fatalf("%d marks", n)
	}
}

func TestSinceMark_MarkOlderThanThePruneSaysTruncated(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))
	if err := s.SetMark(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, snapshotEvent(t0.Add(time.Minute), 1100)) // a change that will age out
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if tableCount(t, db, "wm_changes") == 0 {
		t.Fatal("nothing to prune")
	}

	// Eight days on, a new change; the checkpoint prunes what is older than seven.
	apply(t, s, snapshotEvent(t0.Add(8*24*time.Hour), 1500))
	apply(t, s, snapshotEvent(t0.Add(8*24*time.Hour+time.Minute), 1600))
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	var oldest int64
	if err := db.QueryRow(`SELECT MIN(at_ms) FROM wm_changes`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest < t0.Add(7*24*time.Hour).UnixMilli() {
		t.Fatalf("rows older than the retention are still there: %v", time.UnixMilli(oldest))
	}
	d, err := s.SinceMark(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated {
		t.Fatal("a mark older than the prune must say the result was truncated")
	}
	if d.Empty() {
		t.Fatal("what is still there must still be returned")
	}
	// And the same after a restart.
	s2 := newDBStore(t, db, clk)
	if d2, _ := s2.SinceMark(ctx, "old"); !d2.Truncated || d2.Empty() {
		t.Fatalf("after restart: %+v", d2)
	}
	fresh := "fresh"
	if err := s2.SetMark(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if d3, _ := s2.SinceMark(ctx, fresh); d3.Truncated {
		t.Fatal("a fresh mark is not truncated")
	}
}

func TestHousekeep_CheckpointsOffThePipelineAndCloseFlushes(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))

	s.Housekeep(ctx, clk.Now()) // not due yet
	time.Sleep(20 * time.Millisecond)
	if s.Stats().Checkpoints != 0 {
		t.Fatal("checkpointed before it was due")
	}
	clk.Advance(CheckpointEvery)
	s.Housekeep(ctx, clk.Now())
	waitFor(t, "the checkpoint", func() bool { return s.Stats().Checkpoints == 1 })
	if tableCount(t, db, "wm_facts") != 1 {
		t.Fatal("not written")
	}
	// Not due again for another period.
	apply(t, s, snapshotEvent(t0.Add(CheckpointEvery+time.Minute), 1100))
	s.Housekeep(ctx, clk.Now())
	time.Sleep(20 * time.Millisecond)
	if s.Stats().Checkpoints != 1 {
		t.Fatal("checkpointed twice in one period")
	}
	// Close flushes what is left, and a second Close is harmless.
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Checkpoints != 2 {
		t.Fatalf("%+v", s.Stats())
	}
	s2 := newDBStore(t, db, clk)
	if pv, _ := s2.View().Portfolio(); pv.TotalUSD != 1100 {
		t.Fatalf("%+v", pv)
	}
	// After Close, Housekeep starts nothing.
	clk.Advance(time.Hour)
	s.Housekeep(ctx, clk.Now())
	time.Sleep(20 * time.Millisecond)
	if s.Stats().Checkpoints != 2 {
		t.Fatal("a closed store checkpointed")
	}
}

func TestHousekeep_FailedCheckpointRetriesSoonerThanAPeriod(t *testing.T) {
	db := openDB(t)
	clk := newClock()
	s := newDBStore(t, db, clk)
	ctx := context.Background()
	apply(t, s, snapshotEvent(t0, 1000))
	if _, err := db.Exec(`ALTER TABLE wm_facts RENAME TO wm_facts_x`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(CheckpointEvery)
	s.Housekeep(ctx, clk.Now())
	waitFor(t, "the failed checkpoint", func() bool { return s.Stats().CheckpointFailures == 1 })
	waitFor(t, "the worker to finish", func() bool { return !s.ckBusy.Load() })
	if _, err := db.Exec(`ALTER TABLE wm_facts_x RENAME TO wm_facts`); err != nil {
		t.Fatal(err)
	}
	clk.Advance(checkpointRetry)
	s.Housekeep(ctx, clk.Now())
	waitFor(t, "the retry", func() bool { return s.Stats().Checkpoints == 1 })
}

func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRestore_IgnoresRowsItCannotRead(t *testing.T) {
	db := openDB(t)
	s := newDBStore(t, db, newClock())
	apply(t, s, tickAt(t0, "binance", "BTC/USDT", 100))
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO wm_facts VALUES ('price:binance:BAD', 'last', 'not json', 0, '', 0)`,
		`INSERT INTO wm_facts VALUES ('price:bin ance:X', 'last', '{"p":1}', 0, '', 0)`,
		`INSERT INTO wm_facts VALUES ('price:binance:NEG', 'last', '{"p":-4}', 0, '', 0)`,
		`INSERT INTO wm_facts VALUES ('portfolio', 'state', '{"TotalUSD":-1}', 0, '', 99)`,
		`INSERT INTO wm_facts VALUES ('mystery', 'x', '{}', 0, '', 0)`,
		`INSERT OR REPLACE INTO wm_detectors VALUES ('price:binance:BTC/USDT', 'ret_1m', '{"ewma":{"mean":1e999}}', 0)`,
		`INSERT INTO wm_detectors VALUES ('price:ghost:GONE', 'ret_1m', '{}', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s2, err := NewStore(context.Background(), db, newClock())
	if err != nil {
		t.Fatalf("garbage in the tables must not stop the store: %v", err)
	}
	if _, _, ok := s2.View().Price("binance", "BTC/USDT"); !ok {
		t.Fatal("the good row was lost")
	}
	if len(s2.series) != 1 {
		t.Fatalf("%d series restored", len(s2.series))
	}
	if _, ok := s2.View().Portfolio(); ok {
		t.Fatal("an invalid portfolio row was restored")
	}
	// The orphan detector rows are removed at the next checkpoint.
	if err := s2.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wm_detectors WHERE series = 'price:ghost:GONE'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan detector rows: %d %v", n, err)
	}
}
