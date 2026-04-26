package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestGetDCASummary_MissingPlanID(t *testing.T) {
	tool := NewGetDCASummaryTool(config.DefaultConfig(), newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when plan_id is missing")
	}
}

func TestGetDCASummary_PlanNotFound(t *testing.T) {
	tool := NewGetDCASummaryTool(config.DefaultConfig(), newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(99999),
	})
	if !result.IsError {
		t.Fatal("expected error for non-existent plan")
	}
}

func TestGetDCASummary_ZeroInvestment(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "SummaryEmpty", true)
	tool := NewGetDCASummaryTool(config.DefaultConfig(), store)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "live price unavailable") {
		t.Errorf("expected 'live price unavailable', got: %s", result.ForUser)
	}
	// totals should be zero
	if !strings.Contains(result.ForUser, "0.0000") {
		t.Errorf("expected zero totals, got: %s", result.ForUser)
	}
}

func TestGetDCASummary_WithStats(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "SummaryStats", true)

	// Two executions: 100 quote for 0.1 base, then 200 quote for 0.2 base.
	if err := store.UpdatePlanStats(context.Background(), planID, 100, 0.1); err != nil {
		t.Fatalf("UpdatePlanStats 1: %v", err)
	}
	if err := store.UpdatePlanStats(context.Background(), planID, 200, 0.2); err != nil {
		t.Fatalf("UpdatePlanStats 2: %v", err)
	}

	tool := NewGetDCASummaryTool(config.DefaultConfig(), store)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	// Total invested = 300, avg cost = 300/0.3 = 1000
	if !strings.Contains(result.ForUser, "300.0000") {
		t.Errorf("expected total invested 300, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "1000.0000") {
		t.Errorf("expected avg cost 1000, got: %s", result.ForUser)
	}
}
