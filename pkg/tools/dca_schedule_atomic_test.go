package tools

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// The gateway runs a "dca:" job only if its ID is the one the plan records, so
// a plan's stored job ID and the live jobs must never disagree. These tests pin
// the producers: no failure may leave the plan pointing at a removed job, or a
// live job that the plan does not know.

func newDCAStoreAt(t *testing.T) (*dca.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := dca.NewStore(dir)
	if err != nil {
		t.Fatalf("dca.NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, filepath.Join(dir, "memory", "dca", "dca.db")
}

// failPlanUpdates makes every UPDATE of dca_plans fail inside SQLite, so a test
// can fail UpdatePlan after the scheduler was touched. Inserts and deletes still work.
func failPlanUpdates(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_plan_updates BEFORE UPDATE ON dca_plans
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
}

func dcaJobs(svc *cron.CronService) []cron.CronJob {
	var out []cron.CronJob
	for _, j := range svc.ListJobs(true) {
		if strings.HasPrefix(j.Name, JobPrefixDCA) {
			out = append(out, j)
		}
	}
	return out
}

func assertPlanLinked(t *testing.T, store *dca.Store, svc *cron.CronService, planID int64, wantJobID, wantExpr string) {
	t.Helper()
	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	jobs := dcaJobs(svc)
	if len(jobs) != 1 {
		t.Fatalf("%d dca: jobs, want exactly 1: %+v", len(jobs), jobs)
	}
	if plan.CronJobID != jobs[0].ID {
		t.Fatalf("plan records job %q but the live job is %q", plan.CronJobID, jobs[0].ID)
	}
	if wantJobID != "" && plan.CronJobID != wantJobID {
		t.Errorf("plan job = %q, want %q", plan.CronJobID, wantJobID)
	}
	if plan.FrequencyExpr != wantExpr {
		t.Errorf("plan schedule = %q, want %q", plan.FrequencyExpr, wantExpr)
	}
	if jobs[0].Schedule.Expr != wantExpr {
		t.Errorf("job schedule = %q, want %q", jobs[0].Schedule.Expr, wantExpr)
	}
}

func TestUpdateDCAPlan_ScheduleChangeIsAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("a later validation error leaves everything as it was", func(t *testing.T) {
		store, _ := newDCAStoreAt(t)
		svc := newTestCronService(t)
		planID, oldJob := seedPlanWithJob(t, store, svc)
		tool := newTestUpdatePlanTool(t, store, svc)

		res := tool.Execute(ctx, map[string]any{
			"plan_id":  float64(planID),
			"schedule": map[string]any{"cron": "0 10 * * 2"},
			"end_date": "not-a-date",
		})
		if !res.IsError {
			t.Fatalf("expected an error, got %q", res.ForLLM)
		}
		assertPlanLinked(t, store, svc, planID, oldJob, "0 9 * * 1")
	})

	t.Run("a failed plan write removes the new job and keeps the old one", func(t *testing.T) {
		store, dbPath := newDCAStoreAt(t)
		svc := newTestCronService(t)
		planID, oldJob := seedPlanWithJob(t, store, svc)
		tool := newTestUpdatePlanTool(t, store, svc)
		failPlanUpdates(t, dbPath)

		res := tool.Execute(ctx, map[string]any{
			"plan_id":  float64(planID),
			"schedule": map[string]any{"cron": "0 10 * * 2"},
		})
		if !res.IsError || !strings.Contains(res.ForLLM, "failed to update plan") {
			t.Fatalf("expected the plan write to fail, got %+v", res)
		}
		assertPlanLinked(t, store, svc, planID, oldJob, "0 9 * * 1")
	})

	t.Run("success leaves exactly one job and the plan points at it", func(t *testing.T) {
		store, _ := newDCAStoreAt(t)
		svc := newTestCronService(t)
		planID, oldJob := seedPlanWithJob(t, store, svc)
		tool := newTestUpdatePlanTool(t, store, svc)

		res := tool.Execute(ctx, map[string]any{
			"plan_id":  float64(planID),
			"name":     "Renamed",
			"schedule": map[string]any{"cron": "0 10 * * 2"},
		})
		if res.IsError {
			t.Fatalf("unexpected error: %s", res.ForLLM)
		}
		assertPlanLinked(t, store, svc, planID, "", "0 10 * * 2")
		jobs := dcaJobs(svc)
		if jobs[0].ID == oldJob {
			t.Error("the plan still uses the old job")
		}
		if svc.GetJob(oldJob) != nil {
			t.Error("the old job was not removed")
		}
		if !strings.HasSuffix(jobs[0].Name, ":Renamed") || !jobs[0].Enabled {
			t.Errorf("new job = %+v, want name ending :Renamed and enabled", jobs[0])
		}
	})

	t.Run("a disabled plan keeps its new job disabled", func(t *testing.T) {
		store, _ := newDCAStoreAt(t)
		svc := newTestCronService(t)
		planID, _ := seedPlanWithJob(t, store, svc)
		tool := newTestUpdatePlanTool(t, store, svc)

		if res := tool.Execute(ctx, map[string]any{"plan_id": float64(planID), "enabled": false}); res.IsError {
			t.Fatal(res.ForLLM)
		}
		if res := tool.Execute(ctx, map[string]any{
			"plan_id": float64(planID), "schedule": map[string]any{"cron": "0 10 * * 2"},
		}); res.IsError {
			t.Fatal(res.ForLLM)
		}
		assertPlanLinked(t, store, svc, planID, "", "0 10 * * 2")
		if dcaJobs(svc)[0].Enabled {
			t.Error("a disabled plan's new job is enabled")
		}
	})

	t.Run("enable and disable follow the plan", func(t *testing.T) {
		store, _ := newDCAStoreAt(t)
		svc := newTestCronService(t)
		planID, jobID := seedPlanWithJob(t, store, svc)
		tool := newTestUpdatePlanTool(t, store, svc)

		if res := tool.Execute(ctx, map[string]any{"plan_id": float64(planID), "enabled": false}); res.IsError {
			t.Fatal(res.ForLLM)
		}
		if svc.GetJob(jobID).Enabled {
			t.Error("the job is still enabled")
		}
	})
}

func TestUpdateDCAPlan_FailedWriteDoesNotToggleTheJob(t *testing.T) {
	store, dbPath := newDCAStoreAt(t)
	svc := newTestCronService(t)
	planID, jobID := seedPlanWithJob(t, store, svc)
	tool := newTestUpdatePlanTool(t, store, svc)
	failPlanUpdates(t, dbPath)

	res := tool.Execute(context.Background(), map[string]any{"plan_id": float64(planID), "enabled": false})
	if !res.IsError {
		t.Fatal("expected the plan write to fail")
	}
	if !svc.GetJob(jobID).Enabled {
		t.Error("the job was disabled although the plan was not updated")
	}
}

func TestCreateDCAPlan_RollsBackWhenJobIDCannotBeRecorded(t *testing.T) {
	store, dbPath := newDCAStoreAt(t)
	svc := newTestCronService(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, svc)
	failPlanUpdates(t, dbPath) // SavePlan inserts and still works; recording the job ID updates

	res := tool.Execute(testCtx(), map[string]any{
		"plan_name": "Weekly BTC", "provider": "bitkub", "symbol": "BTC/THB",
		"amount_per_order": float64(500),
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"notify":           map[string]any{"channel": "test", "chat_id": "user-1"},
	})
	if !res.IsError || !strings.Contains(res.ForLLM, "cron job ID") {
		t.Fatalf("expected the job-ID write to fail, got %+v", res)
	}
	plans, err := store.ListPlans(context.Background(), dca.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Errorf("%d plan(s) left behind: %+v", len(plans), plans)
	}
	if jobs := dcaJobs(svc); len(jobs) != 0 {
		t.Errorf("%d orphan job(s) left behind: %+v", len(jobs), jobs)
	}
}
