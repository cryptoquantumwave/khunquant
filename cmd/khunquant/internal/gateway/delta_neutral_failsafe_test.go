package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// seedMonitorExposure records a filled futures-sell + spot-buy open for a plan.
func seedMonitorExposure(t *testing.T, store *deltaneutral.Store, planID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	execID, err := store.SaveExecution(ctx, &deltaneutral.Execution{
		PlanID: planID, AttemptID: "seed", State: string(deltaneutral.ExecutionStateRecoveryRequired),
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed execution: %v", err)
	}
	for _, l := range []*deltaneutral.ExecutionLeg{
		{LegType: "futures", Side: "sell", FilledQuantity: 1350},
		{LegType: "spot", Side: "buy", FilledQuantity: 1350},
	} {
		l.ExecutionID, l.State, l.OrderType, l.CreatedAt, l.UpdatedAt = execID, "filled", "market", now, now
		if _, err := store.SaveExecutionLeg(ctx, l); err != nil {
			t.Fatalf("seed leg: %v", err)
		}
	}
}

// TestMonitor_FailedPlanWithExposure_IsMonitored: a failed plan is invisible to unwind, so
// when it still holds a position the monitor must keep watching it; a failed plan with
// no exposure stays skipped.
func TestMonitor_FailedPlanWithExposure_IsMonitored(t *testing.T) {
	spot := func() *monitorSpotProvider {
		return &monitorSpotProvider{
			tickerPrice: 0.033,
			balances:    []broker.Balance{{Asset: "CHZ", Free: 1350.0}},
			earnErr:     errFakeEarnUnavailable,
		}
	}

	t.Run("with exposure", func(t *testing.T) {
		store := newMonitorStore(t)
		id, jobName := seedMonitorPlan(t, store, "mockspot")
		seedMonitorExposure(t, store, id)
		if err := store.UpdatePlanStatus(context.Background(), id, deltaneutral.PlanStatusFailed); err != nil {
			t.Fatalf("set failed: %v", err)
		}
		if snap := runMonitor(t, store, id, jobName, spot()); snap == nil {
			t.Fatal("a failed plan with open exposure must still be monitored (snapshot expected)")
		}
	})

	t.Run("without exposure", func(t *testing.T) {
		store := newMonitorStore(t)
		id, jobName := seedMonitorPlan(t, store, "mockspot")
		if err := store.UpdatePlanStatus(context.Background(), id, deltaneutral.PlanStatusFailed); err != nil {
			t.Fatalf("set failed: %v", err)
		}
		if snap := runMonitor(t, store, id, jobName, spot()); snap != nil {
			t.Fatal("a failed plan without exposure must stay skipped")
		}
	})
}

// TestDNEnabled_IncludesResize: resize_delta_neutral_position must switch the shared
// delta-neutral registration block on by itself.
func TestDNEnabled_IncludesResize(t *testing.T) {
	cfg := &config.Config{}
	for _, name := range dnToolNames {
		setDNToolEnabled(t, cfg, name, false)
	}
	if dnToolsEnabled(cfg) {
		t.Fatal("with every delta-neutral tool disabled, dnToolsEnabled must be false")
	}

	setDNToolEnabled(t, cfg, "resize_delta_neutral_position", true)
	if !dnToolsEnabled(cfg) {
		t.Fatal("resize_delta_neutral_position alone must enable the delta-neutral block")
	}

	// Every tool registered in the block must be covered by dnToolNames.
	cfg2 := &config.Config{}
	for _, name := range dnToolNames {
		setDNToolEnabled(t, cfg2, name, false)
	}
	for _, name := range dnToolNames {
		setDNToolEnabled(t, cfg2, name, true)
		if !dnToolsEnabled(cfg2) {
			t.Fatalf("%s alone must enable the delta-neutral block", name)
		}
		setDNToolEnabled(t, cfg2, name, false)
	}
}

func setDNToolEnabled(t *testing.T, cfg *config.Config, name string, enabled bool) {
	t.Helper()
	switch name {
	case "create_delta_neutral_plan":
		cfg.Tools.CreateDeltaNeutralPlan.Enabled = enabled
	case "list_delta_neutral_plans":
		cfg.Tools.ListDeltaNeutralPlans.Enabled = enabled
	case "get_delta_neutral_plan":
		cfg.Tools.GetDeltaNeutralPlan.Enabled = enabled
	case "update_delta_neutral_plan":
		cfg.Tools.UpdateDeltaNeutralPlan.Enabled = enabled
	case "delete_delta_neutral_plan":
		cfg.Tools.DeleteDeltaNeutralPlan.Enabled = enabled
	case "get_delta_neutral_summary":
		cfg.Tools.GetDeltaNeutralSummary.Enabled = enabled
	case "get_delta_neutral_history":
		cfg.Tools.GetDeltaNeutralHistory.Enabled = enabled
	case "prepare_delta_neutral_plan":
		cfg.Tools.PrepareDeltaNeutralPlan.Enabled = enabled
	case "open_delta_neutral_position":
		cfg.Tools.OpenDeltaNeutralPosition.Enabled = enabled
	case "unwind_delta_neutral_position":
		cfg.Tools.UnwindDeltaNeutralPosition.Enabled = enabled
	case "resize_delta_neutral_position":
		cfg.Tools.ResizeDeltaNeutralPosition.Enabled = enabled
	case "render_delta_neutral_yield_chart":
		cfg.Tools.RenderDeltaNeutralYieldChart.Enabled = enabled
	default:
		t.Fatalf("unknown delta-neutral tool %q", name)
	}
}
