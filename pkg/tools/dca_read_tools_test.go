package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func saveRow(t *testing.T, store *dca.Store, e dca.Execution) {
	t.Helper()
	e.ExecutedAt = time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	e.CreatedAt = e.ExecutedAt
	e.Symbol = "BTC/THB"
	e.Provider = "bitkub"
	if _, err := store.SaveExecution(context.Background(), &e); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
}

func TestGetDCAHistory_RendersSkippedAndSide(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "HistoryKinds", true)
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "completed", AmountQuote: 100, FilledQuantity: 0.1, FilledPrice: 1000})
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "skipped", ErrorMsg: "period cap reached: 1/1 executions this day"})
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "failed", ErrorMsg: "order placement failed: boom"})
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "completed", Side: "sell", AmountQuote: 50, FilledQuantity: 0.05, FilledPrice: 1000})

	out := NewGetDCAHistoryTool(store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser

	for _, want := range []string{
		"– skipped",
		"↳ skipped: period cap reached",
		"✗ failed",
		"↳ error: order placement failed: boom",
		"✓ completed (sell)",
		"✓ completed\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("history lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "↳ error: period cap") || strings.Contains(out, "✓ skipped") {
		t.Errorf("a skipped row is misreported:\n%s", out)
	}
}

func TestGetDCASummary_ExecutionsHeadlineCountsFillsOnly(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "SummaryCounts", true)
	for i := 0; i < 2; i++ {
		saveRow(t, store, dca.Execution{PlanID: planID, Status: "completed", AmountQuote: 100})
	}
	for i := 0; i < 3; i++ {
		saveRow(t, store, dca.Execution{PlanID: planID, Status: "skipped", ErrorMsg: "x"})
	}
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "failed", ErrorMsg: "y"})

	out := NewGetDCASummaryTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser
	if !strings.Contains(out, "Executions:      2 (3 skipped, 1 failed)") {
		t.Errorf("unexpected executions line:\n%s", out)
	}

	// With nothing but completed rows there is no suffix.
	planID = seedPlan(t, store, "SummaryClean", true)
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "completed", AmountQuote: 100})
	out = NewGetDCASummaryTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser
	if !strings.Contains(out, "Executions:      1\n") {
		t.Errorf("unexpected executions line:\n%s", out)
	}
}

func TestGetDCASummary_SellPlanShowsSellTotals(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "SummarySell", true)
	setDCAPlan(t, store, planID, func(p *dca.Plan) { p.Side = "sell" })
	saveRow(t, store, dca.Execution{PlanID: planID, Side: "sell", Status: "completed", AmountQuote: 100, FilledQuantity: 0.1})
	saveRow(t, store, dca.Execution{PlanID: planID, Side: "sell", Status: "completed", AmountQuote: 300, FilledQuantity: 0.2})
	// A skipped row contributes nothing.
	saveRow(t, store, dca.Execution{PlanID: planID, Side: "sell", Status: "skipped", AmountQuote: 999, FilledQuantity: 9})

	out := NewGetDCASummaryTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser
	for _, want := range []string{"Total sold:      0.30000000", "Total proceeds:  400.0000", "Avg sell price:  1333.3333", "Executions:      2 (1 skipped)"} {
		if !strings.Contains(out, want) {
			t.Errorf("sell summary lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Total invested", "Avg cost", "Total acquired"} {
		if strings.Contains(out, bad) {
			t.Errorf("sell summary still shows %q:\n%s", bad, out)
		}
	}
}
