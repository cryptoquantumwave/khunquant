package attention

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// A database created by the v1 steps upgrades to v2 with its rows intact, its
// delivered rows still counting against today's budget and its undelivered
// digest items still reaching the owner.
func TestMigration_V1DatabaseUpgradesToV2(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	if len(migrations) < 2 || migrations[0].Version != 1 || migrations[1].Version != 2 {
		t.Fatalf("migrations = %+v", migrations)
	}
	if err := sqlitex.Migrate(ctx, db, migrations[0]); err != nil {
		t.Fatalf("v1: %v", err)
	}

	now := bkkAt(0, 12, 0)
	nowMs := now.UnixMilli()
	insert := func(id, key, disp, reason string, sev, delivered int, deliveredAt any) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO attention_decisions
			(id, at_ms, dedup_key, trigger_id, severity, disposition, reason, shadow, candidate_json, delivered, delivered_at_ms)
			VALUES (?, ?, ?, 't', ?, ?, ?, 0, ?, ?, ?)`,
			id, nowMs-60000, key, sev, disp, reason,
			`{"DedupKey":"`+key+`","Title":"old `+key+`","Severity":"info","Subject":"BTC/USDT"}`, delivered, deliveredAt); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("OLD1", "old:sent", "notify", ReasonTemplate, 1, 1, nowMs-30000)
	insert("OLD2", "old:released", "notify", ReasonTemplate, 1, 0, nowMs-30000)
	insert("OLD3", "old:queued", "digest", ReasonInfoToDigest, 0, 0, nil)
	insert("OLD4", "old:digested", "digest", ReasonInfoToDigest, 0, 1, nowMs-20000)
	if _, err := db.Exec(`INSERT INTO attention_state VALUES ('old:sent', 1, ?, '{}', 1)`, nowMs-30000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO attention_mutes VALUES ('mute:', ?, 'r', 0)`, nowMs+3600000); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{t: now}
	prof := &fakeProfile{p: intent.DefaultProfile()}
	prof.p.NotificationBudgetPerDay = 1
	cfg := digestCfg()
	g, err := New(db, prof, cfg, config.ProactiveModeLive, clock.Now)
	if err != nil {
		t.Fatalf("open with the new code: %v", err)
	}

	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, Component).Scan(&v); err != nil || v != 2 {
		t.Fatalf("schema version = %d, %v", v, err)
	}
	// the old rows are intact, and the new columns are null on them.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE id LIKE 'OLD%' AND intervention_id IS NULL AND base_asset IS NULL`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("old rows = %d, %v", n, err)
	}
	var js string
	if err := db.QueryRow(`SELECT candidate_json FROM attention_decisions WHERE id = 'OLD1'`).Scan(&js); err != nil || js == "" {
		t.Fatalf("old candidate json lost: %q, %v", js, err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM attention_state`, `SELECT COUNT(*) FROM attention_mutes`,
	} {
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s = %d, %v", q, n, err)
		}
	}

	// the delivered v1 row (no intervention id) is a message of its own and
	// uses the whole budget of 1.
	d, err := g.Decide(ctx, cand("fresh", notice))
	if err != nil {
		t.Fatal(err)
	}
	expect(t, d, proactive.DispDigest, ReasonBudget)

	// the undelivered v1 digest item was backfilled into the queue, and the
	// delivered one was not.
	if err := db.QueryRow(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = 'OLD3' AND flushed_ms IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old digest item queue rows = %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM attention_digest_queue WHERE decision_id = 'OLD4'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delivered digest item queued: %d, %v", n, err)
	}
	// the digest sends it, together with the new item.
	del := &fakeDeliverer{}
	ob := NewOutbox(g, del, time.Minute, clock.Now)
	clock.Set(bkkAt(1, 8, 0))
	if err := ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	msgs := del.messages()
	if len(msgs) != 1 || !msgs[0].Digest || len(msgs[0].Decisions) != 2 {
		t.Fatalf("digest after the upgrade = %+v", msgs)
	}

	// running the new code again is harmless.
	if _, err := New(db, prof, cfg, config.ProactiveModeLive, clock.Now); err != nil {
		t.Fatal(err)
	}
}

// Other readers select from attention_decisions: v2 only adds to it.
func TestMigration_V2OnlyAddsColumns(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	if err := sqlitex.Migrate(ctx, db, migrations[0]); err != nil {
		t.Fatal(err)
	}
	cols := func() map[string]bool {
		rows, err := db.Query(`SELECT name FROM pragma_table_info('attention_decisions')`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out[name] = true
		}
		return out
	}
	before := cols()
	if err := sqlitex.Migrate(ctx, db, migrations...); err != nil {
		t.Fatal(err)
	}
	after := cols()
	for c := range before {
		if !after[c] {
			t.Errorf("v2 dropped column %s", c)
		}
	}
	for _, c := range []string{"intervention_id", "base_asset"} {
		if before[c] || !after[c] {
			t.Errorf("column %s: before %v, after %v", c, before[c], after[c])
		}
	}
}
