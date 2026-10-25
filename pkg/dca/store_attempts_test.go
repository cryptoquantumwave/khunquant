package dca

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// MR-006 store tests: stats by side, atomic execution+stats writes, instant
// comparison for period counts and history filters, and the side column.

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func saveFill(t *testing.T, s *Store, planID int64, side string, quote, qty float64) {
	t.Helper()
	e := minimalExecution(planID)
	e.Side = side
	e.AmountQuote, e.FilledQuantity = quote, qty
	if _, err := s.SaveExecutionTx(context.Background(), e, &StatsDelta{Side: side, AmountQuote: quote, Quantity: qty}); err != nil {
		t.Fatalf("SaveExecutionTx(%s): %v", side, err)
	}
}

func planTotals(t *testing.T, s *Store, planID int64) (invested, qty, avg float64) {
	t.Helper()
	p, err := s.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	return p.TotalInvested, p.TotalQuantity, p.AvgCost
}

func TestDCAStats_SellReducesQuantity(t *testing.T) {
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("SellStats"))

	saveFill(t, s, planID, "buy", 100, 0.002)
	saveFill(t, s, planID, "buy", 300, 0.006)
	if inv, qty, avg := planTotals(t, s, planID); !near(inv, 400) || !near(qty, 0.008) || !near(avg, 50000) {
		t.Fatalf("after buys: invested %v qty %v avg %v", inv, qty, avg)
	}

	// Selling 0.003 at a price different from the average: quantity falls,
	// invested falls by avg x qty (not by the proceeds), avg cost is unchanged.
	saveFill(t, s, planID, "sell", 180, 0.003)
	if inv, qty, avg := planTotals(t, s, planID); !near(inv, 250) || !near(qty, 0.005) || !near(avg, 50000) {
		t.Errorf("after sell: invested %v qty %v avg %v; want 250 0.005 50000", inv, qty, avg)
	}

	// A buy afterwards averages in from the remaining holding.
	saveFill(t, s, planID, "buy", 110, 0.002) // 360 / 0.007
	if inv, qty, avg := planTotals(t, s, planID); !near(inv, 360) || !near(qty, 0.007) || !near(avg, 360/0.007) {
		t.Errorf("after rebuy: invested %v qty %v avg %v", inv, qty, avg)
	}

	// The side is stored on the row.
	rows, err := s.GetExecutions(context.Background(), QueryFilter{PlanID: &planID})
	if err != nil {
		t.Fatalf("GetExecutions: %v", err)
	}
	sells := 0
	for _, r := range rows {
		if r.Side == "sell" {
			sells++
		}
	}
	if len(rows) != 4 || sells != 1 {
		t.Errorf("rows=%d sells=%d; want 4 and 1", len(rows), sells)
	}
}

func TestDCAStats_SellNeverGoesNegative(t *testing.T) {
	s := newTestStore(t)
	planID, _ := s.SavePlan(context.Background(), minimalPlan("SellClamp"))

	// A sell on an empty plan stays at zero.
	saveFill(t, s, planID, "sell", 50, 0.001)
	if inv, qty, avg := planTotals(t, s, planID); inv != 0 || qty != 0 || avg != 0 {
		t.Fatalf("sell on an empty plan: invested %v qty %v avg %v", inv, qty, avg)
	}

	saveFill(t, s, planID, "buy", 100, 0.002)
	// Selling more than is held empties the holding, never below it. The
	// average cost stays as it was.
	saveFill(t, s, planID, "sell", 500, 0.01)
	inv, qty, avg := planTotals(t, s, planID)
	if inv != 0 || qty != 0 || !near(avg, 50000) {
		t.Errorf("oversell: invested %v qty %v avg %v; want 0 0 50000", inv, qty, avg)
	}
}

func TestStore_UpdatePlanStats_FollowsPlanSide(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("LegacyStats"))
	if err := s.UpdatePlanStats(ctx, planID, 100, 0.002); err != nil {
		t.Fatalf("UpdatePlanStats buy: %v", err)
	}
	sell := minimalPlan("LegacySell")
	sell.Side = "sell"
	sell.TotalInvested, sell.TotalQuantity, sell.AvgCost = 100, 0.002, 50000
	sellID, _ := s.SavePlan(ctx, sell)
	if err := s.UpdatePlanStats(ctx, sellID, 30, 0.001); err != nil {
		t.Fatalf("UpdatePlanStats sell: %v", err)
	}
	if inv, qty, _ := planTotals(t, s, planID); !near(inv, 100) || !near(qty, 0.002) {
		t.Errorf("buy plan totals: %v %v", inv, qty)
	}
	if inv, qty, avg := planTotals(t, s, sellID); !near(inv, 50) || !near(qty, 0.001) || !near(avg, 50000) {
		t.Errorf("sell plan totals: %v %v %v", inv, qty, avg)
	}
	if err := s.UpdatePlanStats(ctx, 9999, 1, 1); err == nil {
		t.Error("UpdatePlanStats on a missing plan should fail")
	}
}

func TestSaveExecutionTx_AtomicRollback(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("Atomic"))

	// Make the stats UPDATE fail after the INSERT has succeeded inside the tx.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_stats BEFORE UPDATE ON dca_plans
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	e := minimalExecution(planID)
	e.FilledQuantity = 0.002
	if _, err := s.SaveExecutionTx(ctx, e, &StatsDelta{Side: "buy", AmountQuote: 100, Quantity: 0.002}); err == nil {
		t.Fatal("expected the failing stats update to fail the call")
	}
	if n, _ := s.CountExecutions(ctx, planID); n != 0 {
		t.Errorf("the execution row survived a failed stats update (%d rows)", n)
	}
	if inv, qty, _ := planTotals(t, s, planID); inv != 0 || qty != 0 {
		t.Errorf("totals changed: %v %v", inv, qty)
	}
	if e.ID != 0 {
		t.Errorf("exec.ID set to %d on failure", e.ID)
	}

	// Without the fault both land together.
	if _, err := s.db.Exec(`DROP TRIGGER fail_stats`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveExecutionTx(ctx, e, &StatsDelta{Side: "buy", AmountQuote: 100, Quantity: 0.002}); err != nil {
		t.Fatalf("SaveExecutionTx: %v", err)
	}
	if n, _ := s.CountExecutions(ctx, planID); n != 1 || e.ID == 0 {
		t.Errorf("rows=%d id=%d after a clean write", n, e.ID)
	}
	if inv, qty, _ := planTotals(t, s, planID); !near(inv, 100) || !near(qty, 0.002) {
		t.Errorf("totals = %v %v", inv, qty)
	}

	// A nil delta writes the row alone.
	if _, err := s.SaveExecutionTx(ctx, minimalExecution(planID), nil); err != nil {
		t.Fatal(err)
	}
	if inv, _, _ := planTotals(t, s, planID); !near(inv, 100) {
		t.Errorf("a nil delta changed the totals: %v", inv)
	}
}

func bangkok(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		return time.FixedZone("Asia/Bangkok", 7*3600) // no tzdata; Bangkok has no DST
	}
	return loc
}

// TestCountExecutionsInPeriod_NonUTCHost writes rows in UTC, as production
// does, and evaluates at a fixed Bangkok (UTC+7) instant around local midnight.
// The old code compared "2026-03-11T00:00:00+07:00" with "2026-03-10T17:10:00Z"
// as strings and so missed every row between 17:00Z and 24:00Z.
func TestCountExecutionsInPeriod_NonUTCHost(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("Bangkok"))

	now := time.Date(2026, 3, 11, 6, 30, 0, 0, bangkok(t)) // 2026-03-10T23:30Z
	for _, at := range []string{
		"2026-03-08T16:50:00Z", // Sunday 23:50 local: last week
		"2026-03-08T17:05:00Z", // Monday 00:05 local: this week
		"2026-03-10T16:50:00Z", // Tuesday 23:50 local: yesterday
		"2026-03-10T17:10:00Z", // Wednesday 00:10 local: today
		"2026-03-10T23:00:00Z", // Wednesday 06:00 local: this hour exactly
	} {
		ts, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatal(err)
		}
		e := minimalExecution(planID)
		e.ExecutedAt = ts
		if _, err := s.SaveExecution(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var stored string
	if err := s.db.QueryRow(`SELECT executed_at FROM dca_executions ORDER BY id LIMIT 1`).Scan(&stored); err != nil || !strings.HasSuffix(stored, "Z") {
		t.Fatalf("rows must be stored in UTC, got %q (%v)", stored, err)
	}

	for period, want := range map[string]int{"hour": 1, "day": 2, "week": 4} {
		got, err := s.CountExecutionsInPeriodAt(ctx, planID, period, now, false)
		if err != nil {
			t.Fatalf("%s: %v", period, err)
		}
		if got != want {
			t.Errorf("%s count = %d, want %d", period, got, want)
		}
	}
}

func TestCountExecutionsInPeriod_PaperRowsAndStatuses(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("PaperCount"))
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	add := func(status, reason string) {
		e := minimalExecution(planID)
		e.ExecutedAt, e.Status, e.ErrorMsg = now.Add(-time.Minute), status, reason
		if _, err := s.SaveExecution(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	add("completed", "")
	add("skipped", PaperSkipReason)
	add("skipped", "period cap reached: 1/1 executions this day")
	add("failed", "order placement failed")

	for _, tc := range []struct {
		paper bool
		want  int
	}{{false, 1}, {true, 2}} {
		got, err := s.CountExecutionsInPeriodAt(ctx, planID, "day", now, tc.paper)
		if err != nil || got != tc.want {
			t.Errorf("includePaper=%v: count=%d err=%v; want %d", tc.paper, got, err, tc.want)
		}
	}
}

func TestCountExecutionsInPeriod_UnparseableTimestampIsCounted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("Corrupt"))
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)

	if _, err := s.db.Exec(`INSERT INTO dca_executions
		(plan_id, executed_at, symbol, provider, amount_quote, status, created_at)
		VALUES (?, 'not-a-time', 'BTC/THB', 'bitkub', 1, 'completed', 'x')`, planID); err != nil {
		t.Fatal(err)
	}
	got, err := s.CountExecutionsInPeriodAt(ctx, planID, "day", now, false)
	if err != nil || got != 1 {
		t.Errorf("a corrupt row must keep counting toward the cap: count=%d err=%v", got, err)
	}
}

func TestGetExecutions_SinceUntilCompareInstants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	planID, _ := s.SavePlan(ctx, minimalPlan("Instants"))

	// Rows in mixed offsets; string order disagrees with time order.
	times := map[string]string{
		"a": "2026-03-10T17:30:00Z",
		"b": "2026-03-11T00:10:00+07:00", // 2026-03-10T17:10Z
		"c": "2026-03-11T01:00:00+07:00", // 2026-03-10T18:00Z
		"d": "2026-03-10T16:00:00Z",
	}
	ids := map[string]int64{}
	for name, ts := range times {
		res, err := s.db.Exec(`INSERT INTO dca_executions
			(plan_id, executed_at, symbol, provider, amount_quote, status, order_id, created_at)
			VALUES (?, ?, 'BTC/THB', 'bitkub', 1, 'completed', ?, ?)`, planID, ts, name, ts)
		if err != nil {
			t.Fatal(err)
		}
		ids[name], _ = res.LastInsertId()
	}
	query := func(f QueryFilter) string {
		f.PlanID = &planID
		rows, err := s.GetExecutions(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, r := range rows {
			names = append(names, r.OrderID)
		}
		// stable order for comparison
		out := ""
		for _, n := range []string{"a", "b", "c", "d"} {
			for _, got := range names {
				if got == n {
					out += n
				}
			}
		}
		return out
	}

	since := time.Date(2026, 3, 10, 17, 0, 0, 0, time.UTC)
	until := time.Date(2026, 3, 11, 0, 30, 0, 0, bangkok(t)) // 2026-03-10T17:30Z
	if got := query(QueryFilter{Since: &since}); got != "abc" {
		t.Errorf("since 17:00Z = %q; want abc", got)
	}
	if got := query(QueryFilter{Until: &until}); got != "abd" {
		t.Errorf("until 17:30Z = %q; want abd", got)
	}
	if got := query(QueryFilter{Since: &since, Until: &until}); got != "ab" {
		t.Errorf("between = %q; want ab", got)
	}
	// Pagination still applies after the filter.
	rows, err := s.GetExecutions(ctx, QueryFilter{PlanID: &planID, Since: &since, Limit: 1, Offset: 2})
	if err != nil || len(rows) != 1 {
		t.Errorf("paged filtered query: %d rows, %v", len(rows), err)
	}
	_ = ids
}

func TestDCAMigration_AddsSideColumnIdempotently(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory", "dca")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "dca.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A database from before the side column existed.
	oldSchema := strings.Replace(schema, "    side             TEXT    NOT NULL DEFAULT 'buy',\n    created_at       TEXT    NOT NULL\n",
		"    created_at       TEXT    NOT NULL\n", 1)
	if oldSchema == schema {
		t.Fatal("test could not derive the pre-side schema")
	}
	for _, stmt := range []string{
		oldSchema,
		`INSERT INTO dca_plans (name, provider, symbol, amount_per_order, frequency_expr, start_date, created_at, updated_at)
		 VALUES ('legacy', 'bitkub', 'BTC/THB', 100, '0 9 * * 1', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO dca_executions (plan_id, executed_at, symbol, provider, amount_quote, status, created_at)
		 VALUES (1, '2026-01-02T09:00:00Z', 'BTC/THB', 'bitkub', 100, 'completed', '2026-01-02T09:00:00Z')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed old db: %v", err)
		}
	}
	if hasSideColumn(t, db) {
		t.Fatal("the seeded database already has the column")
	}
	db.Close()

	for pass := 1; pass <= 2; pass++ { // the second open proves idempotence
		s, err := NewStore(ws)
		if err != nil {
			t.Fatalf("NewStore pass %d: %v", pass, err)
		}
		if !hasSideColumn(t, s.db) {
			t.Fatalf("pass %d: side column missing", pass)
		}
		rows, err := s.GetExecutions(context.Background(), QueryFilter{})
		if err != nil || len(rows) != pass {
			t.Fatalf("pass %d: rows = %+v, %v", pass, rows, err)
		}
		for _, r := range rows {
			want := "buy" // the legacy row, and anything written without a side
			if r.ID == 2 {
				want = "sell"
			}
			if r.Side != want {
				t.Errorf("pass %d: row %d side = %q; want %q", pass, r.ID, r.Side, want)
			}
		}
		if pass == 1 {
			e := minimalExecution(1)
			e.Side = "sell"
			if _, err := s.SaveExecution(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
	}
}

func hasSideColumn(t *testing.T, db *sql.DB) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(dca_executions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "side" {
			return true
		}
	}
	return false
}
