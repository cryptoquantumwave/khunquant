package tools

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func TestExecuteDCAOrder_MissingPlanID(t *testing.T) {
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when plan_id is missing")
	}
}

func TestExecuteDCAOrder_PlanNotFound(t *testing.T) {
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(99999),
	})
	if !result.IsError {
		t.Fatal("expected error for non-existent plan")
	}
}

func TestExecuteDCAOrder_DisabledPlan(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "DisabledPlan", false)
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), store)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if !result.IsError {
		t.Fatal("expected error for disabled plan")
	}
}

func TestExecuteDCAOrder_ExpiredPlan(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "ExpiredPlan", true)

	plan, _ := store.GetPlan(context.Background(), planID)
	past := time.Now().UTC().Add(-24 * time.Hour)
	plan.EndDate = &past
	if err := store.UpdatePlan(context.Background(), plan); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}

	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), store)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if !result.IsError {
		t.Fatal("expected error for expired plan")
	}
}

// TestExecuteDCAOrder_GuardrailExceeded drives the tool through the period
// cap. MR-006: the cap used to skip silently with no row; it now stays a
// silent result for the agent but leaves a "skipped" row behind.
func TestExecuteDCAOrder_GuardrailExceeded(t *testing.T) {
	store, mock, planID := newDCAGateFixture(t, "dca-guardrail")
	now := fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.MaxExecPerPeriod = 1
		p.ExecPeriod = "day"
	})
	seedDCARow(t, store, planID, "completed", "", now.Add(-time.Hour))

	result := NewExecuteDCAOrderTool(config.DefaultConfig(), store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)})

	if result.IsError || !result.Silent {
		t.Fatalf("expected a silent skip, got %+v", result)
	}
	if mock.ordered {
		t.Error("an order was placed past the period cap")
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 2 {
		t.Fatalf("expected the seeded row plus one skipped row, got %d", len(rows))
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		symbol string
		want   string
	}{
		{"BTC/USDT", "BTC"},
		{"ETH/THB", "ETH"},
		{"noSlash", "noSlash"},
	}
	for _, tc := range tests {
		got := split(tc.symbol)
		if got != tc.want {
			t.Errorf("split(%q) = %q, want %q", tc.symbol, got, tc.want)
		}
	}
}

func TestExecuteDCAOrderTool_Name(t *testing.T) {
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), newTestDCAStore(t))
	if tool.Name() != NameExecuteDCAOrder {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameExecuteDCAOrder)
	}
}

func TestExecuteDCAOrderTool_Description(t *testing.T) {
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), newTestDCAStore(t))
	desc := tool.Description()
	if desc == "" {
		t.Fatal("Description() should not be empty")
	}
	if !containsStr(desc, "order") && !containsStr(desc, "Order") {
		t.Errorf("Description should mention orders, got: %s", desc)
	}
}

func TestExecuteDCAOrderTool_Parameters(t *testing.T) {
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), newTestDCAStore(t))
	params := tool.Parameters()

	if params == nil {
		t.Fatal("Parameters() should not return nil")
	}
	if params["type"] != "object" {
		t.Errorf("type should be 'object', got %q", params["type"])
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties to be a map")
	}

	if _, ok := props["plan_id"]; !ok {
		t.Error("expected property 'plan_id' in Parameters")
	}

	required, ok := params["required"].([]string)
	if !ok {
		t.Fatal("required should be a slice")
	}
	if len(required) == 0 || required[0] != "plan_id" {
		t.Errorf("plan_id should be required, got %v", required)
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
