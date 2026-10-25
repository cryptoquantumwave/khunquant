package tools

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

// addDCANamedJob adds a job with a dca:-prefixed name directly, the way the DCA
// tools do.
func addDCANamedJob(t *testing.T, svc *cron.CronService, name string) string {
	t.Helper()
	job, err := svc.AddJob(name, cron.CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "UTC"}, "[DCA-AUTO]", false, "test", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	return job.ID
}

// A re-link removes every other job named for the plan - a stranded live job from
// before the plan stopped pointing at it, an orphan - and leaves the jobs of
// other plans alone, however alike their IDs are ("dca:1:" is not "dca:12:").
func TestUpdateDCAPlan_RelinkRemovesStrandedJobsOfThatPlanOnly(t *testing.T) {
	store, _ := newDCAStoreAt(t)
	svc := newTestCronService(t)
	planID, recorded := seedPlanWithJob(t, store, svc)
	stranded := addDCANamedJob(t, svc, fmt.Sprintf("dca:%d:old name", planID))
	neighbour := addDCANamedJob(t, svc, fmt.Sprintf("dca:%d2:other plan", planID)) // dca:12: when planID is 1
	every := int64(3_600_000)
	unrelated, err := svc.AddJob("stretch", cron.CronSchedule{Kind: "every", EveryMS: &every}, "x", false, "test", "user-1")
	if err != nil {
		t.Fatal(err)
	}

	tool := newTestUpdatePlanTool(t, store, svc)
	// The remedy the user is given: the same schedule, passed again.
	res := tool.Execute(context.Background(), map[string]any{
		"plan_id": float64(planID), "schedule": map[string]any{"cron": "0 9 * * 1"},
	})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}

	plan, _ := store.GetPlan(context.Background(), planID)
	if plan.CronJobID == recorded {
		t.Error("an unchanged expression did not re-link: the plan still points at the old job")
	}
	if svc.GetJob(recorded) != nil || svc.GetJob(stranded) != nil {
		t.Error("the old or stranded job survived the re-link")
	}
	if svc.GetJob(neighbour) == nil {
		t.Error("another plan's job was removed")
	}
	if svc.GetJob(unrelated.ID) == nil {
		t.Error("an unrelated job was removed")
	}
	own := 0
	for _, j := range svc.ListJobs(true) {
		if strings.HasPrefix(j.Name, fmt.Sprintf("dca:%d:", planID)) {
			own++
			if j.ID != plan.CronJobID {
				t.Errorf("job %s is not the plan's recorded job", j.ID)
			}
		}
	}
	if own != 1 {
		t.Errorf("%d jobs for the plan, want 1", own)
	}
}

func TestDeleteDCAPlan_LeavesNoJobBehind(t *testing.T) {
	store, _ := newDCAStoreAt(t)
	svc := newTestCronService(t)
	planID, recorded := seedPlanWithJob(t, store, svc)
	stranded := addDCANamedJob(t, svc, fmt.Sprintf("dca:%d:stranded", planID))
	neighbour := addDCANamedJob(t, svc, fmt.Sprintf("dca:%d2:other plan", planID))

	res := NewDeleteDCAPlanTool(store, svc).Execute(context.Background(), map[string]any{"plan_id": float64(planID)})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	if svc.GetJob(recorded) != nil || svc.GetJob(stranded) != nil {
		t.Error("a job of the deleted plan survived")
	}
	if svc.GetJob(neighbour) == nil {
		t.Error("another plan's job was removed")
	}
	if _, err := store.GetPlan(context.Background(), planID); err == nil {
		t.Error("the plan still exists")
	}
}

// A failed delete leaves the plan and its job as they were.
func TestDeleteDCAPlan_FailedDeleteKeepsTheJob(t *testing.T) {
	store, dbPath := newDCAStoreAt(t)
	svc := newTestCronService(t)
	planID, recorded := seedPlanWithJob(t, store, svc)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_plan_deletes BEFORE DELETE ON dca_plans
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	res := NewDeleteDCAPlanTool(store, svc).Execute(context.Background(), map[string]any{"plan_id": float64(planID)})
	if !res.IsError {
		t.Fatal("expected the delete to fail")
	}
	if svc.GetJob(recorded) == nil {
		t.Error("the job was removed although the plan was not deleted")
	}
}
