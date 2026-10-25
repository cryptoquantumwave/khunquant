package attention

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

func TestNew_Validation(t *testing.T) {
	db := sqlitextest.Open(t)
	prof := &fakeProfile{}
	if _, err := New(nil, prof, liveCfg(), "live", nil); err == nil {
		t.Error("nil database accepted")
	}
	if _, err := New(db, nil, liveCfg(), "live", nil); err == nil {
		t.Error("nil profile source accepted")
	}
	for _, mode := range []string{"", "LIVE", "on", "governed"} {
		if _, err := New(db, prof, liveCfg(), mode, nil); err == nil {
			t.Errorf("mode %q accepted", mode)
		}
	}
	g, err := New(db, prof, liveCfg(), "live", nil) // nil clock = time.Now
	if err != nil {
		t.Fatal(err)
	}
	if got := g.clock(); time.Since(got) > time.Minute {
		t.Errorf("default clock returned %v", got)
	}
	// a second governor over the same database re-runs migrations harmlessly,
	// and the schema version is recorded.
	if _, err := New(db, prof, liveCfg(), "shadow", nil); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, Component).Scan(&v); err != nil || v != 3 {
		t.Fatalf("schema version = %d, err %v; want 3", v, err)
	}
	// an unwritable database makes New fail rather than panic.
	closed := sqlitextest.Open(t)
	_ = closed.Close()
	if _, err := New(closed, prof, liveCfg(), "live", nil); err == nil {
		t.Error("closed database accepted")
	}
}

func TestMute_Validation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	future := e.clock.Now().Add(time.Hour)

	bad := []struct {
		name   string
		prefix string
		until  time.Time
	}{
		{"empty prefix", "", future},
		{"one character", "d", future},
		{"only spaces", "   ", future},
		{"one character padded", "  d ", future},
		{"deadline in the past", "dn:", e.clock.Now().Add(-time.Minute)},
		{"deadline is now", "dn:", e.clock.Now()},
		{"zero deadline", "dn:", time.Time{}},
	}
	for _, c := range bad {
		if err := e.g.Mute(ctx, c.prefix, c.until, "x"); err == nil {
			t.Errorf("%s: Mute succeeded", c.name)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 0 {
		t.Fatalf("rejected mutes left %d rows", n)
	}

	if err := e.g.Mute(ctx, " dn: ", future, strings.Repeat("r", 1000)); err != nil {
		t.Fatalf("valid mute: %v", err)
	}
	var key, reason string
	var until int64
	if err := e.db.QueryRow(`SELECT key, until_ms, reason FROM attention_mutes`).Scan(&key, &until, &reason); err != nil {
		t.Fatal(err)
	}
	if key != "dn:" || until != future.UnixMilli() || len(reason) != maxMuteReasonRunes {
		t.Fatalf("stored mute = %q until %d reason len %d", key, until, len(reason))
	}

	// re-muting replaces the deadline; it does not add a row.
	later := future.Add(5 * time.Hour)
	if err := e.g.Mute(ctx, "dn:", later, "again"); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 1 {
		t.Fatalf("re-mute produced %d rows", n)
	}
	if err := e.db.QueryRow(`SELECT until_ms, reason FROM attention_mutes WHERE key = 'dn:'`).Scan(&until, &reason); err != nil || until != later.UnixMilli() || reason != "again" {
		t.Fatalf("re-mute stored until=%d reason=%q err=%v", until, reason, err)
	}

	if err := e.g.Unmute(ctx, ""); err == nil {
		t.Error("Unmute with empty prefix succeeded")
	}
	if err := e.g.Unmute(ctx, "never-muted"); err != nil {
		t.Errorf("Unmute of an unknown prefix: %v", err)
	}
	if err := e.g.Unmute(ctx, "dn:"); err != nil {
		t.Fatal(err)
	}
	if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 0 {
		t.Fatalf("Unmute left %d rows", n)
	}
}

// insertRaw inserts a decision row with explicit age and state.
func (e *env) insertRaw(id, disp string, age time.Duration, delivered bool) {
	e.t.Helper()
	var dat any
	del := 0
	if delivered {
		del, dat = 1, e.clock.Now().Add(-age).UnixMilli()
	}
	_, err := e.db.Exec(`INSERT INTO attention_decisions
		(id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered, delivered_at_ms)
		VALUES (?, ?, 'raw', 't', 1, ?, 'r', 0, '{}', ?, ?)`,
		id, e.clock.Now().Add(-age).UnixMilli(), disp, del, dat)
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) has(id string) bool {
	e.t.Helper()
	return e.count("SELECT COUNT(*) FROM attention_decisions WHERE id = ?", id) == 1
}

func TestPrune_KeepsRecentAndPendingDigest(t *testing.T) {
	e := newEnv(t)
	day := 24 * time.Hour
	// A retention shorter than the pending-digest window makes the exemption
	// observable; in production (180d vs 7d) it is a belt-and-braces clause.
	e.g.retention = 3 * day
	e.g.pendingDigestKeep = 7 * day
	e.g.shadowRetention = day

	e.insertRaw("old-delivered", "notify", 5*day, true)
	e.insertRaw("old-drop", "drop", 5*day, false)
	e.insertRaw("old-digest-pending", "digest", 5*day, false) // younger than 7d: kept
	e.insertRaw("ancient-digest-pending", "digest", 8*day, false)
	e.insertRaw("old-digest-sent", "digest", 5*day, true)
	e.insertRaw("recent-delivered", "notify", day, true)
	e.insertRaw("recent-drop", "drop", time.Hour, false)
	e.insertRaw("shadow-2d", "log", 2*day, false)
	e.insertRaw("shadow-1h", "log", time.Hour, false)
	if _, err := e.db.Exec(`UPDATE attention_decisions SET reason = 'shadow' WHERE id IN ('shadow-2d', 'shadow-1h')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO attention_state VALUES ('stale', 1, ?, '{}', 1), ('fresh', 1, ?, '{}', 1)`,
		e.clock.Now().Add(-5*day).UnixMilli(), e.clock.Now().Add(-day).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	e.decide(cand("trigger-prune", notice)) // first decision prunes
	for id, want := range map[string]bool{
		"old-delivered": false, "old-drop": false, "old-digest-pending": true,
		"ancient-digest-pending": false, "old-digest-sent": false,
		"recent-delivered": true, "recent-drop": true,
		"shadow-2d": false, "shadow-1h": true,
	} {
		if got := e.has(id); got != want {
			t.Errorf("row %s present = %v, want %v", id, got, want)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM attention_state WHERE dedup_key = 'stale'"); n != 0 {
		t.Error("stale state row survived")
	}
	if n := e.count("SELECT COUNT(*) FROM attention_state WHERE dedup_key = 'fresh'"); n != 1 {
		t.Error("fresh state row was pruned")
	}

	// at most once a day: an old row added now survives the next decisions...
	e.insertRaw("old-again", "drop", 5*day, false)
	e.clock.Add(time.Hour)
	e.decide(cand("another", notice))
	if !e.has("old-again") {
		t.Fatal("prune ran twice within a day")
	}
	// ...and goes at the next daily pass.
	e.clock.Add(day)
	e.decide(cand("another2", notice))
	if e.has("old-again") {
		t.Fatal("prune did not run after a day")
	}
}

func TestPrune_ProductionRetentionIs180Days(t *testing.T) {
	e := newEnv(t)
	day := 24 * time.Hour
	e.insertRaw("d179", "drop", 179*day, false)
	e.insertRaw("d181", "drop", 181*day, false)
	e.insertRaw("s29", "log", 29*day, false)
	e.insertRaw("s31", "log", 31*day, false)
	e.insertRaw("n31", "drop", 31*day, false)
	if _, err := e.db.Exec(`UPDATE attention_decisions SET reason = 'shadow' WHERE id IN ('s29', 's31')`); err != nil {
		t.Fatal(err)
	}
	e.decide(cand("k", notice))
	if !e.has("d179") || e.has("d181") {
		t.Fatalf("179d present=%v 181d present=%v", e.has("d179"), e.has("d181"))
	}
	if !e.has("s29") || e.has("s31") || !e.has("n31") {
		t.Fatalf("shadow retention is 30d: 29d present=%v 31d present=%v, non-shadow 31d present=%v", e.has("s29"), e.has("s31"), e.has("n31"))
	}
}

func TestEncodeCandidate(t *testing.T) {
	decode := func(t *testing.T, s string) map[string]any {
		t.Helper()
		if len(s) > maxCandidateJSON {
			t.Fatalf("stored %d bytes, cap is %d", len(s), maxCandidateJSON)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		return m
	}
	t.Run("small candidate is stored whole", func(t *testing.T) {
		c := cand("k", notice)
		c.Evidence = map[string]any{"pct": -8.5}
		m := decode(t, encodeCandidate(c))
		if m["Evidence"] == nil || m["DedupKey"] != "k" {
			t.Fatalf("stored %v", m)
		}
	})
	t.Run("oversized evidence is replaced", func(t *testing.T) {
		c := cand("k", notice)
		c.Evidence = map[string]any{"blob": strings.Repeat("x", 3*maxCandidateJSON)}
		m := decode(t, encodeCandidate(c))
		ev, _ := m["Evidence"].(map[string]any)
		if ev["truncated"] != true || m["DedupKey"] != "k" {
			t.Fatalf("stored %v", m)
		}
	})
	t.Run("unmarshalable evidence is replaced", func(t *testing.T) {
		c := cand("k", notice)
		c.Evidence = map[string]any{"x": math.NaN()}
		m := decode(t, encodeCandidate(c))
		if ev, _ := m["Evidence"].(map[string]any); ev["truncated"] != true {
			t.Fatalf("stored %v", m)
		}
	})
	t.Run("oversized text is clamped", func(t *testing.T) {
		c := cand("k", notice)
		c.Title = strings.Repeat("é", 2*maxCandidateJSON)
		m := decode(t, encodeCandidate(c))
		if m["DedupKey"] != "k" {
			t.Fatalf("stored %v", m)
		}
	})
	t.Run("hopeless candidate still yields valid JSON", func(t *testing.T) {
		c := cand("k", notice)
		c.Severity = proactive.Severity(42) // cannot be marshalled
		m := decode(t, encodeCandidate(c))
		if m["truncated"] != true {
			t.Fatalf("stored %v", m)
		}
	})
	t.Run("a decision with huge evidence is made and recorded", func(t *testing.T) {
		e := newEnv(t)
		c := cand("k", notice)
		c.Evidence = map[string]any{"blob": strings.Repeat("x", 3*maxCandidateJSON), "nan": math.Inf(1)}
		d := e.decide(c)
		expect(t, d, proactive.DispNotify, ReasonTemplate)
		if d.Candidate.Evidence["blob"] == nil {
			t.Error("returned candidate lost its evidence")
		}
		var js string
		if err := e.db.QueryRow(`SELECT candidate_json FROM attention_decisions WHERE id = ?`, d.ID).Scan(&js); err != nil {
			t.Fatal(err)
		}
		decode(t, js)
		e.mark(d, true)
		// the delivered (truncated) candidate is what a later re-alert cites.
		e.clock.Add(time.Hour)
		d2 := e.decide(cand("k", warning))
		if d2.Previous == nil || d2.Previous.DedupKey != "k" {
			t.Fatalf("Previous = %+v", d2.Previous)
		}
	})
}

func TestDecide_GarbageStateJSONDoesNotBreakDecision(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.Exec(`INSERT INTO attention_state VALUES ('k', 1, ?, 'not json', 1)`,
		e.clock.Now().Add(-48*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	d := e.decide(cand("k", warning))
	expect(t, d, proactive.DispNotify, ReasonTemplate)
	if d.Previous != nil {
		t.Fatalf("Previous = %+v from garbage", d.Previous)
	}
}

func TestDatabaseErrorsAreReturned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.decide(cand("k", notice))
	_ = e.db.Close()

	if _, err := e.g.Decide(ctx, cand("k2", notice)); err == nil {
		t.Error("Decide on a closed database succeeded")
	}
	if _, err := e.g.Decide(ctx, cand("k3", notice, func(c *proactive.Candidate) { c.Shadow = true })); err == nil {
		t.Error("shadow Decide on a closed database succeeded")
	}
	if err := e.g.MarkDelivered(ctx, d, Delivery{Delivered: true}); err == nil {
		t.Error("MarkDelivered on a closed database succeeded")
	}
	if err := e.g.Mute(ctx, "dn:", e.clock.Now().Add(time.Hour), ""); err == nil {
		t.Error("Mute on a closed database succeeded")
	}
	if err := e.g.Unmute(ctx, "dn:"); err == nil {
		t.Error("Unmute on a closed database succeeded")
	}
	if err := pruneDecisions(ctx, e.db, e.clock.Now(), time.Hour, time.Hour, time.Hour); err == nil {
		t.Error("prune on a closed database succeeded")
	}
}

func TestDatabaseErrorsInEachQuery(t *testing.T) {
	// Break one table at a time so every read in the decision path reports its
	// error instead of deciding on partial facts.
	for _, tc := range []struct{ name, table string }{
		{"mutes", "attention_mutes"},
		{"state", "attention_state"},
		{"decisions", "attention_decisions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if _, err := e.db.Exec(`DROP TABLE ` + tc.table); err != nil {
				t.Fatal(err)
			}
			if _, err := e.g.Decide(context.Background(), cand("k", notice)); err == nil {
				t.Fatalf("Decide succeeded with %s missing", tc.table)
			}
		})
	}
}

func TestDecide_ContextCancelled(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.g.Decide(ctx, cand("k", notice)); err == nil {
		t.Fatal("Decide with a cancelled context succeeded")
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions"); n != 0 {
		t.Fatalf("cancelled Decide left %d rows", n)
	}
}

func TestDecide_ProfileReadBeforeTransaction(t *testing.T) {
	// The profile is another package's store: it must be read while no
	// transaction of ours holds the write lock. A source that itself writes to
	// the shared database would deadlock the pool otherwise.
	db := sqlitextest.Open(t)
	clock := &fakeClock{t: bkkAt(0, 12, 0)}
	src := profileFunc(func(ctx context.Context) (p intent.Profile, err error) {
		// take the write lock ourselves: blocks forever if Decide holds it.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return p, err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `INSERT INTO attention_mutes VALUES ('zz', 1, '', 0)`); err != nil {
			return p, err
		}
		return intent.DefaultProfile(), tx.Commit()
	})
	g, err := New(db, src, liveCfg(), config.ProactiveModeLive, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := g.Decide(context.Background(), cand("k", notice))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Decide deadlocked: the profile was read inside the transaction")
	}
}

func TestEvaluate_Table(t *testing.T) {
	type row struct {
		name   string
		c      proactive.Candidate
		f      facts
		disp   proactive.Disposition
		reason string
	}
	live := facts{Mode: modeLive, InActiveHours: true, Budget: 3}
	with := func(f facts, mut func(*facts)) facts { mut(&f); return f }
	rows := []row{
		{"off", cand("k", critical, protective), facts{Mode: modeOff}, proactive.DispDrop, ReasonOff},
		{"shadow mode", cand("k", critical, protective), facts{Mode: modeShadow}, proactive.DispLog, ReasonShadow},
		{"shadow candidate", cand("k", notice, func(c *proactive.Candidate) { c.Shadow = true }), live, proactive.DispLog, ReasonShadow},
		{"muted", cand("k", notice), with(live, func(f *facts) { f.Muted = true }), proactive.DispDrop, ReasonMuted},
		{"dedup", cand("k", notice), with(live, func(f *facts) { f.DedupHit = true }), proactive.DispDrop, ReasonDedupSame},
		{"dedup beats protective", cand("k", critical, protective), with(live, func(f *facts) { f.DedupHit = true }), proactive.DispDrop, ReasonDedupSame},
		{"queued", cand("k", notice), with(live, func(f *facts) { f.QueuedHit = true }), proactive.DispDrop, ReasonDedupQueued},
		{"queued ignores protective critical", cand("k", critical, protective), with(live, func(f *facts) { f.QueuedHit = true }), proactive.DispNotify, ReasonProtective},
		{"protective", cand("k", critical, protective), with(live, func(f *facts) { f.InActiveHours = false; f.SendsToday = 99 }), proactive.DispNotify, ReasonProtective},
		{"protective llm", cand("k", critical, protective, composeLLM), live, proactive.DispEscalate, ReasonProtective},
		{"quiet", cand("k", warning), with(live, func(f *facts) { f.InActiveHours = false }), proactive.DispDigest, ReasonQuietHours},
		{"info", cand("k", info), live, proactive.DispDigest, ReasonInfoToDigest},
		{"budget", cand("k", notice), with(live, func(f *facts) { f.SendsToday = 3 }), proactive.DispDigest, ReasonBudget},
		{"warning extra", cand("k", warning), with(live, func(f *facts) { f.SendsToday = 3 }), proactive.DispNotify, ReasonTemplate},
		{"warning cap", cand("k", warning), with(live, func(f *facts) { f.SendsToday = 4 }), proactive.DispDigest, ReasonBudget},
		{"llm", cand("k", notice, composeLLM), live, proactive.DispEscalate, ReasonComposeLLM},
		{"template", cand("k", notice), live, proactive.DispNotify, ReasonTemplate},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			d, why := evaluate(r.c, r.f)
			if d != r.disp || why != r.reason {
				t.Fatalf("evaluate = %s/%s, want %s/%s", d, why, r.disp, r.reason)
			}
		})
	}
}
