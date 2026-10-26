package tools

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// A pending or unknown row is neither a fill nor a failure: the history marks it
// "?" and labels its note, and the summary counts it next to the completed headline.
func TestGetDCAHistoryAndSummary_RenderPendingAndUnknown(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "HistoryPending", true)
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "pending", OrderID: "o-1", ErrorMsg: "order o-1 is still \"open\""})
	saveRow(t, store, dca.Execution{PlanID: planID, Status: "unknown", OrderID: "o-2", ErrorMsg: "fill could not be confirmed"})

	out := NewGetDCAHistoryTool(store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser
	for _, want := range []string{"? pending", "? unknown", "↳ note: order o-1 is still", "↳ note: fill could not be confirmed"} {
		if !strings.Contains(out, want) {
			t.Errorf("history lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓ pending") || strings.Contains(out, "✗ unknown") || strings.Contains(out, "↳ error:") {
		t.Errorf("a pending/unknown row is misreported:\n%s", out)
	}

	sum := NewGetDCASummaryTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)}).ForUser
	if !strings.Contains(sum, "1 pending") || !strings.Contains(sum, "1 unknown") {
		t.Errorf("summary hides the pending/unknown rows:\n%s", sum)
	}
}

// A pending order counts toward the per-period cap: the next run is skipped by
// the guardrail and places nothing, instead of buying again blind on top of an
// order that may still be live.
func TestExecuteDCAOrder_PendingBlocksNextRunInPeriod(t *testing.T) {
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-10", "open", 0),
		fetchResults: []dcaFetchResult{{order: dcaOrd("o-10", "open", 0)}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)
	setDCAPlan(t, store, planID, func(p *dca.Plan) { p.ExecPeriod, p.MaxExecPerPeriod = "day", 1 })

	if res := dcaRun(t, tool, planID); res.IsError {
		t.Fatalf("first run: %s", res.ForLLM)
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "pending" {
		t.Fatalf("first run rows = %+v; want one pending row", rows)
	}
	res := dcaRun(t, tool, planID)
	if !strings.Contains(res.ForLLM, "guardrail") {
		t.Errorf("second run should hit the period cap:\n%s", res.ForLLM)
	}
	if fake.createCalls != 1 {
		t.Errorf("CreateOrder calls = %d; want 1 (the pending order must block a second buy)", fake.createCalls)
	}
}
