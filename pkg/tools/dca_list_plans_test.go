package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func seedPlan(t *testing.T, store *dca.Store, name string, enabled bool) int64 {
	t.Helper()
	now := time.Now().UTC()
	plan := &dca.Plan{
		Name:           name,
		Provider:       "bitkub",
		Symbol:         "BTC/THB",
		AmountPerOrder: 100,
		FrequencyExpr:  "0 9 * * 1",
		Timezone:       "UTC",
		Enabled:        enabled,
		Side:           "buy",
		StartDate:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	id, err := store.SavePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("seedPlan: %v", err)
	}
	return id
}

func TestListDCAPlans_Empty(t *testing.T) {
	tool := NewListDCAPlansTool(newTestDCAStore(t))
	result := tool.Execute(testCtx(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "No DCA plans found") {
		t.Errorf("expected 'No DCA plans found', got: %s", result.ForUser)
	}
}

func TestListDCAPlans_AllPlans(t *testing.T) {
	store := newTestDCAStore(t)
	seedPlan(t, store, "Plan-A", true)
	seedPlan(t, store, "Plan-B", false)
	tool := NewListDCAPlansTool(store)

	result := tool.Execute(testCtx(), map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "Plan-A") {
		t.Error("expected Plan-A in output")
	}
	if !strings.Contains(result.ForUser, "Plan-B") {
		t.Error("expected Plan-B in output")
	}
	if !strings.Contains(result.ForUser, "2 total") {
		t.Errorf("expected '2 total', got: %s", result.ForUser)
	}
}

func TestListDCAPlans_FilterEnabled(t *testing.T) {
	store := newTestDCAStore(t)
	seedPlan(t, store, "Enabled-Plan", true)
	seedPlan(t, store, "Disabled-Plan", false)
	tool := NewListDCAPlansTool(store)

	result := tool.Execute(testCtx(), map[string]any{"filter_enabled": true})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "Enabled-Plan") {
		t.Error("expected Enabled-Plan in filtered output")
	}
	if strings.Contains(result.ForUser, "Disabled-Plan") {
		t.Error("did not expect Disabled-Plan when filtering enabled")
	}
}

func TestListDCAPlans_FilterDisabled(t *testing.T) {
	store := newTestDCAStore(t)
	seedPlan(t, store, fmt.Sprintf("Enabled-%d", time.Now().UnixNano()), true)
	seedPlan(t, store, "Disabled-Only", false)
	tool := NewListDCAPlansTool(store)

	result := tool.Execute(testCtx(), map[string]any{"filter_enabled": false})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "Disabled-Only") {
		t.Error("expected Disabled-Only in filtered output")
	}
}
