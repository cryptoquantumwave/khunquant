package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func seedExecution(t *testing.T, store *dca.Store, planID int64) int64 {
	t.Helper()
	now := time.Now().UTC()
	exec := &dca.Execution{
		PlanID:         planID,
		ExecutedAt:     now,
		Symbol:         "BTC/THB",
		Provider:       "bitkub",
		AmountQuote:    100,
		FilledPrice:    3000000,
		FilledQuantity: 0.0000333,
		Status:         "completed",
		CreatedAt:      now,
	}
	id, err := store.SaveExecution(context.Background(), exec)
	if err != nil {
		t.Fatalf("seedExecution: %v", err)
	}
	return id
}

func TestGetDCAHistory_MissingPlanID(t *testing.T) {
	tool := NewGetDCAHistoryTool(newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when plan_id is missing")
	}
}

func TestGetDCAHistory_PlanNotFound(t *testing.T) {
	tool := NewGetDCAHistoryTool(newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(99999),
	})
	if !result.IsError {
		t.Fatal("expected error for non-existent plan")
	}
}

func TestGetDCAHistory_NoExecutions(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "HistoryEmpty", true)
	tool := NewGetDCAHistoryTool(store)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "No executions found") {
		t.Errorf("expected 'No executions found', got: %s", result.ForUser)
	}
}

func TestGetDCAHistory_WithExecutions(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "HistoryFull", true)
	seedExecution(t, store, planID)
	seedExecution(t, store, planID)
	seedExecution(t, store, planID)
	tool := NewGetDCAHistoryTool(store)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "3 total") {
		t.Errorf("expected '3 total', got: %s", result.ForUser)
	}
}

func TestGetDCAHistory_LimitCapped(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "LimitPlan", true)
	tool := NewGetDCAHistoryTool(store)

	// limit=200 should not error (internally capped to 100)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"limit":   float64(200),
	})
	if result.IsError {
		t.Fatalf("unexpected error for large limit: %s", result.ForLLM)
	}
}

func TestGetDCAHistory_Pagination(t *testing.T) {
	store := newTestDCAStore(t)
	planID := seedPlan(t, store, "PaginationPlan", true)
	seedExecution(t, store, planID)
	seedExecution(t, store, planID)
	tool := NewGetDCAHistoryTool(store)

	// offset=2 skips both executions
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"offset":  float64(2),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "No executions found") {
		t.Errorf("expected no rows after offset=2, got: %s", result.ForUser)
	}
}
