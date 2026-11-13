package deltaneutral

import (
	"context"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// An OKX cross position with no margin ratio reaches the monitor as nil. It must be carried
// as unknown, never as 0% used (which reads as perfectly safe).
func TestMonitorCheck_NilMarginRatioIsUnknownNotZero(t *testing.T) {
	check := func(t *testing.T, mutate func(*ccxt.Position)) MonitorSnapshot {
		t.Helper()
		p := shortPos("CHZ/USDT:USDT", "short", 135, 10)
		mutate(&p)
		fut := &fakeFutures{positions: []ccxt.Position{p}, mark: 0.033, rate: 0.0001}
		m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
		res, err := m.Check(context.Background(), monitorPlan())
		if err != nil {
			t.Fatal(err)
		}
		return res.Eval.Snapshot
	}

	t.Run("no ratio, a liquidation price: unknown, the distance decides", func(t *testing.T) {
		s := check(t, func(p *ccxt.Position) { p.MarginRatio = nil })
		if s.MarginIsKnown() || s.MarginRatioPct != 0 {
			t.Fatalf("known=%v pct=%v, want unknown", s.MarginIsKnown(), s.MarginRatioPct)
		}
		if want := classifyMargin(0, s.LiquidationDistancePct, DefaultRiskPolicy()); s.MarginState != want {
			t.Fatalf("state %q, want the distance's %q", s.MarginState, want)
		}
	})
	t.Run("no ratio and no liquidation price: not safe", func(t *testing.T) {
		s := check(t, func(p *ccxt.Position) { p.MarginRatio, p.LiquidationPrice = nil, nil })
		if s.MarginIsKnown() {
			t.Fatal("margin must be unknown")
		}
		if s.MarginState == "safe" {
			t.Fatalf("margin state %q: an unknown margin with an unknown distance must not be safe", s.MarginState)
		}
		if s.MarginState != "watch" {
			t.Fatalf("margin state %q, want watch", s.MarginState)
		}
	})
	t.Run("a known ratio is unchanged", func(t *testing.T) {
		s := check(t, func(p *ccxt.Position) { p.MarginRatio = fl(0.8) })
		if !s.MarginIsKnown() || !floatAlmostEqual(s.MarginRatioPct, 80, 1e-9) {
			t.Fatalf("known=%v pct=%v, want known 80", s.MarginIsKnown(), s.MarginRatioPct)
		}
		if want := classifyMargin(80, s.LiquidationDistancePct, DefaultRiskPolicy()); s.MarginState != want {
			t.Fatalf("state %q, want %q", s.MarginState, want)
		}
	})
	t.Run("a known ratio of exactly 0 is a reading", func(t *testing.T) {
		s := check(t, func(p *ccxt.Position) { p.MarginRatio = fl(0) })
		if !s.MarginIsKnown() {
			t.Fatal("an exchange-reported 0 is known")
		}
	})
}

// A row stored before margin_ratio_known existed has the flag 0. It reads as known when the
// value is not 0, and as unknown when it is exactly 0 (the old writer stored 0 for both: the
// fail-safe reading).
func TestStore_MarginRatioKnownRoundTripAndLegacyRows(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	planID, err := store.SavePlan(ctx, createTestPlan(t, "margin-unknown"))
	if err != nil {
		t.Fatal(err)
	}
	save := func(pct float64, known bool, at time.Time) {
		t.Helper()
		_, err := store.SaveSnapshot(ctx, &MonitorSnapshot{
			PlanID: planID, CheckedAt: at, MarginRatioPct: pct, MarginRatioKnown: known,
			DataStatus: DataStatusOK, CreatedAt: at,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	save(0, false, now.Add(-5*time.Minute))   // unknown
	save(0, true, now.Add(-4*time.Minute))    // a reading of exactly 0
	save(37.5, true, now.Add(-3*time.Minute)) // a reading
	// A legacy row: the flag column left at its default 0, with a value that is not 0 and one that is.
	for i, pct := range []float64{12.5, 0} {
		at := now.Add(-time.Duration(2-i) * time.Minute).Format(time.RFC3339)
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO delta_neutral_monitor_snapshots (plan_id, checked_at, margin_ratio_pct, created_at) VALUES (?, ?, ?, ?)`,
			planID, at, pct, at); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err := store.ListSnapshots(ctx, planID, 10, 0) // newest first
	if err != nil || len(snaps) != 5 {
		t.Fatalf("snapshots %d, err %v", len(snaps), err)
	}
	want := []struct {
		pct   float64
		known bool
	}{{0, false}, {12.5, true}, {37.5, true}, {0, true}, {0, false}}
	for i, w := range want {
		if snaps[i].MarginRatioPct != w.pct || snaps[i].MarginIsKnown() != w.known {
			t.Errorf("row %d: pct %v known %v, want %v %v", i, snaps[i].MarginRatioPct, snaps[i].MarginIsKnown(), w.pct, w.known)
		}
	}
}
