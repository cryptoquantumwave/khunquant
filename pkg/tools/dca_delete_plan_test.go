package tools

import (
	"context"
	"testing"
)

func TestDeleteDCAPlan_MissingPlanID(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewDeleteDCAPlanTool(store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when plan_id is missing")
	}
}

func TestDeleteDCAPlan_PlanNotFound(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewDeleteDCAPlanTool(store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(99999),
	})
	if !result.IsError {
		t.Fatal("expected error for non-existent plan")
	}
}

func TestDeleteDCAPlan_Success(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, jobID := seedPlanWithJob(t, store, cronSvc)
	tool := NewDeleteDCAPlanTool(store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	// Plan must be gone from store.
	if _, err := store.GetPlan(context.Background(), planID); err == nil {
		t.Error("expected GetPlan to error after deletion")
	}

	// Cron job must be removed.
	for _, j := range cronSvc.ListJobs(false) {
		if j.ID == jobID {
			t.Errorf("cron job %q still exists after plan deletion", jobID)
		}
	}
}
