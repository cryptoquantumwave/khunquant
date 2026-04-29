package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func newTestUpdatePlanTool(t *testing.T, store *dca.Store, cronSvc *cron.CronService) *UpdateDCAPlanTool {
	t.Helper()
	return NewUpdateDCAPlanTool(store, cronSvc)
}

// seedPlanWithJob creates a plan + registers a matching cron job. Returns planID and jobID.
func seedPlanWithJob(t *testing.T, store *dca.Store, cronSvc *cron.CronService) (int64, string) {
	t.Helper()
	now := time.Now().UTC()
	plan := &dca.Plan{
		Name:           fmt.Sprintf("Seeded-%d", now.UnixNano()),
		Provider:       "bitkub",
		Symbol:         "BTC/THB",
		AmountPerOrder: 100,
		FrequencyExpr:  "0 9 * * 1",
		Timezone:       "UTC",
		Enabled:        true,
		Side:           "buy",
		StartDate:      now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	planID, err := store.SavePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("seedPlanWithJob SavePlan: %v", err)
	}

	job, err := cronSvc.AddJob(
		fmt.Sprintf("dca:%d:%s", planID, plan.Name),
		cron.CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "UTC"},
		fmt.Sprintf("[DCA-AUTO] Execute plan: %s plan_id=%d", plan.Name, planID),
		false, "test", "user-1",
	)
	if err != nil {
		t.Fatalf("seedPlanWithJob AddJob: %v", err)
	}
	job.Payload.NoHistory = true
	cronSvc.UpdateJob(job)

	plan.CronJobID = job.ID
	if err := store.UpdatePlan(context.Background(), plan); err != nil {
		t.Fatalf("seedPlanWithJob UpdatePlan: %v", err)
	}

	return planID, job.ID
}

func TestUpdateDCAPlan_MissingPlanID(t *testing.T) {
	tool := newTestUpdatePlanTool(t, newTestDCAStore(t), newTestCronService(t))
	result := tool.Execute(testCtx(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when plan_id is missing")
	}
}

func TestUpdateDCAPlan_PlanNotFound(t *testing.T) {
	tool := newTestUpdatePlanTool(t, newTestDCAStore(t), newTestCronService(t))
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(99999),
		"enabled": true,
	})
	if !result.IsError {
		t.Fatal("expected error for non-existent plan")
	}
}

func TestUpdateDCAPlan_NoChanges(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)

	tool := newTestUpdatePlanTool(t, store, cronSvc)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "No changes") {
		t.Errorf("expected 'No changes', got: %s", result.ForUser)
	}
}

func TestUpdateDCAPlan_EnabledToggle(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	// Disable
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"enabled": false,
	})
	if result.IsError {
		t.Fatalf("unexpected error disabling: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.Enabled {
		t.Error("expected plan to be disabled")
	}

	// Re-enable
	result = tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"enabled": true,
	})
	if result.IsError {
		t.Fatalf("unexpected error enabling: %s", result.ForLLM)
	}
	got, _ = store.GetPlan(context.Background(), planID)
	if !got.Enabled {
		t.Error("expected plan to be enabled")
	}
}

func TestUpdateDCAPlan_EndDateSet(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"end_date": "2099-06-30",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.EndDate == nil {
		t.Fatal("expected EndDate to be set")
	}
	if got.EndDate.Month() != 6 || got.EndDate.Year() != 2099 {
		t.Errorf("EndDate = %v, want 2099-06", got.EndDate)
	}
}

func TestUpdateDCAPlan_EndDateCleared(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	// Set an end date first
	tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID), "end_date": "2099-06-30"})

	// Clear it
	result := tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"end_date": "none",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.EndDate != nil {
		t.Errorf("expected EndDate to be cleared, got %v", got.EndDate)
	}
}

func TestUpdateDCAPlan_InvalidEndDate(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"end_date": "not-a-date",
	})
	if !result.IsError {
		t.Fatal("expected error for invalid end_date")
	}
}

func TestUpdateDCAPlan_SideChange(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"side":    "sell",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.Side != "sell" {
		t.Errorf("Side = %q, want sell", got.Side)
	}
}

func TestUpdateDCAPlan_InvalidSide(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"side":    "hold",
	})
	if !result.IsError {
		t.Fatal("expected error for invalid side")
	}
}

func TestUpdateDCAPlan_GuardrailUpdate(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"guardrails": map[string]any{
			"max_executions_per_period": float64(3),
			"period":                   "day",
		},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.MaxExecPerPeriod != 3 {
		t.Errorf("MaxExecPerPeriod = %d, want 3", got.MaxExecPerPeriod)
	}
	if got.ExecPeriod != "day" {
		t.Errorf("ExecPeriod = %q, want day", got.ExecPeriod)
	}
}

func TestUpdateDCAPlan_NotifyRouting(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id": float64(planID),
		"notify":  map[string]any{"channel": "line", "chat_id": "line-user-42"},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)
	if got.NotifyChannel != "line" {
		t.Errorf("NotifyChannel = %q, want line", got.NotifyChannel)
	}
	if got.NotifyChatID != "line-user-42" {
		t.Errorf("NotifyChatID = %q, want line-user-42", got.NotifyChatID)
	}
}

func TestUpdateDCAPlan_ScheduleChange(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, oldJobID := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"schedule": map[string]any{"cron": "0 12 * * *"}, // daily at noon
	})
	if result.IsError {
		t.Fatalf("unexpected error on schedule change: %s", result.ForLLM)
	}

	got, _ := store.GetPlan(context.Background(), planID)

	// CronJobID must have changed
	if got.CronJobID == oldJobID {
		t.Error("expected CronJobID to change after schedule update")
	}
	if got.CronJobID == "" {
		t.Error("expected new CronJobID to be set")
	}
	if got.FrequencyExpr != "0 12 * * *" {
		t.Errorf("FrequencyExpr = %q, want 0 12 * * *", got.FrequencyExpr)
	}

	// Old job must be removed from cron service
	for _, j := range cronSvc.ListJobs(false) {
		if j.ID == oldJobID {
			t.Errorf("old cron job %q still exists after schedule change", oldJobID)
		}
	}
}

func TestUpdateDCAPlan_ScheduleChange_NoHistory(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"schedule": map[string]any{"cron": "*/30 * * * *"},
	})

	got, _ := store.GetPlan(context.Background(), planID)

	// Find new cron job
	var newJob *cron.CronJob
	for i := range cronSvc.ListJobs(false) {
		j := cronSvc.ListJobs(false)[i]
		if j.ID == got.CronJobID {
			newJob = &j
			break
		}
	}
	if newJob == nil {
		t.Fatal("new cron job not found in service")
	}
	if !newJob.Payload.NoHistory {
		t.Error("expected NoHistory=true on new cron job after schedule change")
	}
}

func TestUpdateDCAPlan_InvalidCronExpr(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	planID, _ := seedPlanWithJob(t, store, cronSvc)
	tool := newTestUpdatePlanTool(t, store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_id":  float64(planID),
		"schedule": map[string]any{"cron": "not-a-cron"},
	})
	if !result.IsError {
		t.Fatal("expected error for invalid cron expression")
	}
}

