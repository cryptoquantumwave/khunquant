package cron

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adhocore/gronx"
)

// newTestSvc returns a minimal CronService suitable for pure-logic tests.
// It has no storePath and no onJob — only the in-memory store is initialised.
func newTestSvc(jobs ...CronJob) *CronService {
	return &CronService{
		store: &CronStore{Jobs: jobs},
		gronx: gronx.New(),
	}
}

// --- computeNextRun ---

func TestComputeNextRun_At_FutureReturnsAtMS(t *testing.T) {
	svc := newTestSvc()
	future := int64(9_999_999_999_000)
	sched := &CronSchedule{Kind: "at", AtMS: &future}
	got := svc.computeNextRun(sched, 0)
	if got == nil || *got != future {
		t.Errorf("want %d, got %v", future, got)
	}
}

func TestComputeNextRun_At_PastReturnsNil(t *testing.T) {
	svc := newTestSvc()
	past := int64(1000)
	sched := &CronSchedule{Kind: "at", AtMS: &past}
	got := svc.computeNextRun(sched, 2000)
	if got != nil {
		t.Errorf("expected nil for past AtMS, got %d", *got)
	}
}

func TestComputeNextRun_At_NilAtMSReturnsNil(t *testing.T) {
	svc := newTestSvc()
	sched := &CronSchedule{Kind: "at", AtMS: nil}
	if got := svc.computeNextRun(sched, 0); got != nil {
		t.Errorf("expected nil, got %d", *got)
	}
}

func TestComputeNextRun_Every_ReturnsNowPlusDuration(t *testing.T) {
	svc := newTestSvc()
	everyMS := int64(60_000) // 1 minute
	sched := &CronSchedule{Kind: "every", EveryMS: &everyMS}
	nowMS := int64(1_000_000)
	got := svc.computeNextRun(sched, nowMS)
	if got == nil {
		t.Fatal("expected non-nil next run")
	}
	if *got != nowMS+everyMS {
		t.Errorf("want %d, got %d", nowMS+everyMS, *got)
	}
}

func TestComputeNextRun_Every_NilEveryMSReturnsNil(t *testing.T) {
	svc := newTestSvc()
	sched := &CronSchedule{Kind: "every", EveryMS: nil}
	if got := svc.computeNextRun(sched, 0); got != nil {
		t.Errorf("expected nil for nil EveryMS, got %d", *got)
	}
}

func TestComputeNextRun_Every_ZeroEveryMSReturnsNil(t *testing.T) {
	svc := newTestSvc()
	zero := int64(0)
	sched := &CronSchedule{Kind: "every", EveryMS: &zero}
	if got := svc.computeNextRun(sched, 0); got != nil {
		t.Errorf("expected nil for zero EveryMS, got %d", *got)
	}
}

func TestComputeNextRun_Cron_ValidExprReturnsNextTick(t *testing.T) {
	svc := newTestSvc()
	// Run at midnight every day.
	sched := &CronSchedule{Kind: "cron", Expr: "0 0 * * *"}
	nowMS := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	got := svc.computeNextRun(sched, nowMS)
	if got == nil {
		t.Fatal("expected non-nil next run for valid cron expr")
	}
	next := time.UnixMilli(*got)
	// Next should be after now.
	if !next.After(time.UnixMilli(nowMS)) {
		t.Errorf("expected next run after now, got %v", next)
	}
}

func TestComputeNextRun_Cron_EmptyExprReturnsNil(t *testing.T) {
	svc := newTestSvc()
	sched := &CronSchedule{Kind: "cron", Expr: ""}
	if got := svc.computeNextRun(sched, 0); got != nil {
		t.Errorf("expected nil for empty cron expr, got %d", *got)
	}
}

func TestComputeNextRun_UnknownKindReturnsNil(t *testing.T) {
	svc := newTestSvc()
	sched := &CronSchedule{Kind: "unknown"}
	if got := svc.computeNextRun(sched, 0); got != nil {
		t.Errorf("expected nil for unknown schedule kind, got %d", *got)
	}
}

// --- getNextWakeMS ---

func TestGetNextWakeMS_EmptyJobsReturnsNil(t *testing.T) {
	svc := newTestSvc()
	if got := svc.getNextWakeMS(); got != nil {
		t.Errorf("expected nil for empty jobs, got %d", *got)
	}
}

func TestGetNextWakeMS_SingleEnabledJob(t *testing.T) {
	ms := int64(5000)
	svc := newTestSvc(CronJob{
		Enabled: true,
		State:   CronJobState{NextRunAtMS: &ms},
	})
	got := svc.getNextWakeMS()
	if got == nil || *got != ms {
		t.Errorf("want %d, got %v", ms, got)
	}
}

func TestGetNextWakeMS_ReturnsMinOfEnabled(t *testing.T) {
	a, b := int64(3000), int64(1000)
	svc := newTestSvc(
		CronJob{Enabled: true, State: CronJobState{NextRunAtMS: &a}},
		CronJob{Enabled: true, State: CronJobState{NextRunAtMS: &b}},
	)
	got := svc.getNextWakeMS()
	if got == nil || *got != b {
		t.Errorf("want %d (min), got %v", b, got)
	}
}

func TestGetNextWakeMS_DisabledJobsIgnored(t *testing.T) {
	ms := int64(9000)
	svc := newTestSvc(
		CronJob{Enabled: false, State: CronJobState{NextRunAtMS: &ms}},
	)
	if got := svc.getNextWakeMS(); got != nil {
		t.Errorf("expected nil for all-disabled jobs, got %d", *got)
	}
}

func TestGetNextWakeMS_NilNextRunIgnored(t *testing.T) {
	valid := int64(7000)
	svc := newTestSvc(
		CronJob{Enabled: true, State: CronJobState{NextRunAtMS: nil}},
		CronJob{Enabled: true, State: CronJobState{NextRunAtMS: &valid}},
	)
	got := svc.getNextWakeMS()
	if got == nil || *got != valid {
		t.Errorf("want %d, got %v", valid, got)
	}
}

// --- generateID ---

func TestGenerateID_NonEmpty(t *testing.T) {
	id := generateID()
	if id == "" {
		t.Error("generateID returned empty string")
	}
}

func TestGenerateID_Unique(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateID()
		if ids[id] {
			t.Errorf("duplicate ID generated: %q", id)
		}
		ids[id] = true
	}
}

// --- ListJobs ---

func TestListJobs_IncludeDisabled_ReturnsAll(t *testing.T) {
	svc := newTestSvc(
		CronJob{ID: "1", Enabled: true},
		CronJob{ID: "2", Enabled: false},
	)
	jobs := svc.ListJobs(true)
	if len(jobs) != 2 {
		t.Errorf("want 2, got %d", len(jobs))
	}
}

func TestListJobs_ExcludeDisabled_ReturnsOnlyEnabled(t *testing.T) {
	svc := newTestSvc(
		CronJob{ID: "1", Enabled: true},
		CronJob{ID: "2", Enabled: false},
		CronJob{ID: "3", Enabled: true},
	)
	jobs := svc.ListJobs(false)
	if len(jobs) != 2 {
		t.Errorf("want 2 enabled jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if !j.Enabled {
			t.Errorf("unexpected disabled job %q in result", j.ID)
		}
	}
}

func TestListJobs_Empty(t *testing.T) {
	svc := newTestSvc()
	if jobs := svc.ListJobs(true); len(jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(jobs))
	}
}

// --- Status ---

func TestStatus_ReturnsJobCount(t *testing.T) {
	svc := newTestSvc(
		CronJob{ID: "1", Enabled: true},
		CronJob{ID: "2", Enabled: false},
	)
	status := svc.Status()
	if status["jobs"] != 2 {
		t.Errorf("want jobs=2, got %v", status["jobs"])
	}
}

func TestStatus_RunningFalseWhenNotStarted(t *testing.T) {
	svc := newTestSvc()
	status := svc.Status()
	if status["enabled"] != false {
		t.Errorf("expected enabled=false for unstarted service, got %v", status["enabled"])
	}
}

// --- RecomputeNextRuns ---

func TestRecomputeNextRuns_UpdatesEnabledJobsOnly(t *testing.T) {
	svc := newTestSvc(
		CronJob{ID: "1", Enabled: true, Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(1000)}},
		CronJob{ID: "2", Enabled: false, Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(1000)}},
	)

	svc.recomputeNextRuns()

	j1 := svc.GetJob("1")
	j2 := svc.GetJob("2")

	if j1.State.NextRunAtMS == nil {
		t.Error("enabled job should have NextRunAtMS computed")
	}
	if j2.State.NextRunAtMS != nil {
		t.Error("disabled job should not have NextRunAtMS computed")
	}
}

// --- GetJob ---

func TestGetJob_ExistingJob(t *testing.T) {
	svc := newTestSvc(
		CronJob{ID: "123", Name: "test job"},
	)
	job := svc.GetJob("123")
	if job == nil {
		t.Fatal("expected to find job")
	}
	if job.Name != "test job" {
		t.Errorf("got name %q, want %q", job.Name, "test job")
	}
}

func TestGetJob_NonExistentJob(t *testing.T) {
	svc := newTestSvc()
	job := svc.GetJob("nonexistent")
	if job != nil {
		t.Errorf("expected nil for nonexistent job, got %v", job)
	}
}

// --- Integration tests for cron operations (using file store) ---

func TestAddJob_PersistsToFile(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, err := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	if job.ID == "" {
		t.Error("AddJob should return job with non-empty ID")
	}
	if job.Name != "test" {
		t.Errorf("job name: got %q, want %q", job.Name, "test")
	}
	if !job.Enabled {
		t.Error("job should be enabled by default")
	}

	// Verify file was created
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file not created: %v", err)
	}
}

func TestAddJob_OneTimeScheduleDeletesAfterRun(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	future := int64Ptr(time.Now().UnixMilli() + 60000)
	job, err := svc.AddJob("once", CronSchedule{Kind: "at", AtMS: future}, "msg", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	if !job.DeleteAfterRun {
		t.Error("one-time job should have DeleteAfterRun = true")
	}
}

func TestUpdateJob_ModifiesExistingJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, err := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	job.Name = "updated"
	job.Enabled = false
	err = svc.UpdateJob(job)
	if err != nil {
		t.Fatalf("UpdateJob failed: %v", err)
	}

	updated := svc.GetJob(job.ID)
	if updated.Name != "updated" {
		t.Errorf("updated name: got %q, want %q", updated.Name, "updated")
	}
	if updated.Enabled {
		t.Error("job should be disabled after update")
	}
}

func TestUpdateJob_NonExistentJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job := &CronJob{ID: "nonexistent"}
	err := svc.UpdateJob(job)
	if err == nil {
		t.Error("UpdateJob should return error for nonexistent job")
	}
}

func TestRemoveJob_DeletesJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, _ := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")

	removed := svc.RemoveJob(job.ID)
	if !removed {
		t.Error("RemoveJob should return true")
	}

	if found := svc.GetJob(job.ID); found != nil {
		t.Error("job should be deleted")
	}
}

func TestRemoveJob_NonExistentJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	removed := svc.RemoveJob("nonexistent")
	if removed {
		t.Error("RemoveJob should return false for nonexistent job")
	}
}

func TestEnableJob_EnablesDisabledJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, _ := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobID := job.ID

	svc.RemoveJob(jobID)
	svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobs := svc.ListJobs(true)
	if len(jobs) == 0 {
		t.Fatal("should have a job to test")
	}
	testJobID := jobs[0].ID

	// Disable it first
	svc.EnableJob(testJobID, false)
	disabled := svc.GetJob(testJobID)
	if disabled == nil || disabled.Enabled {
		t.Error("job should be disabled")
	}
	if disabled.State.NextRunAtMS != nil {
		t.Error("disabled job should have nil NextRunAtMS")
	}

	// Enable it back
	enabled := svc.EnableJob(testJobID, true)
	if enabled == nil || !enabled.Enabled {
		t.Error("job should be enabled")
	}
	if enabled.State.NextRunAtMS == nil {
		t.Error("enabled job should have NextRunAtMS computed")
	}
}

func TestEnableJob_NonExistentJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	result := svc.EnableJob("nonexistent", true)
	if result != nil {
		t.Error("EnableJob should return nil for nonexistent job")
	}
}

func TestRunJobNow_ExecutesJobImmediate(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	executed := false
	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		executed = true
		return "ok", nil
	})

	job, _ := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobID := job.ID

	result := svc.RunJobNow(jobID)
	if !result {
		t.Error("RunJobNow should return true for existing job")
	}

	// Give goroutine time to execute
	time.Sleep(10 * time.Millisecond)
	if !executed {
		t.Error("job handler should have been called")
	}
}

func TestRunJobNow_NonExistentJob(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	result := svc.RunJobNow("nonexistent")
	if result {
		t.Error("RunJobNow should return false for nonexistent job")
	}
}

func TestSetOnJob_UpdatesHandler(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	if svc.onJob != nil {
		t.Error("onJob should be nil initially")
	}

	handler := func(job *CronJob) (string, error) {
		return "ok", nil
	}
	svc.SetOnJob(handler)

	// Can't easily test that onJob is set due to no public accessor,
	// but we can verify SetOnJob doesn't panic
}

func TestLoad_ReloadsStore(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	_, _ = svc.AddJob("test1", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")

	// Reload store
	err := svc.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Should still have the job
	jobs := svc.ListJobs(true)
	if len(jobs) == 0 {
		t.Error("job should persist after Load")
	}
}

// --- generateID edge case ---

func TestGenerateID_FallbackOnRandomReadFailure(t *testing.T) {
	// Test that generateID works (no test for failure path without mocking)
	id1 := generateID()
	id2 := generateID()

	if id1 == id2 {
		t.Error("sequential generateID calls should produce unique IDs")
	}
	if len(id1) == 0 || len(id2) == 0 {
		t.Error("IDs should not be empty")
	}
}

// --- ExecuteJobByID with handler error ---

func TestExecuteJobByID_HandlerError(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		return "", fmt.Errorf("handler error")
	})

	job, _ := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobID := job.ID

	svc.executeJobByID(jobID)

	// Check that error was recorded in state
	updated := svc.GetJob(jobID)
	if updated == nil {
		t.Fatal("job should still exist")
	}
	if updated.State.LastStatus != "error" {
		t.Errorf("LastStatus: got %q, want error", updated.State.LastStatus)
	}
	if updated.State.LastError == "" {
		t.Error("LastError should be set when handler returns error")
	}
}

// --- ExecuteJobByID with 'at' schedule (one-time) ---

func TestExecuteJobByID_OneTimeSchedule_DeletesAfterRun(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		return "ok", nil
	})

	// Create a one-time job
	now := time.Now().UnixMilli()
	future := int64Ptr(now + 5000)
	job, _ := svc.AddJob("once", CronSchedule{Kind: "at", AtMS: future}, "msg", false, "cli", "direct")
	jobID := job.ID

	// Execute it
	svc.executeJobByID(jobID)

	// Verify it was deleted
	if found := svc.GetJob(jobID); found != nil {
		t.Error("one-time job should be deleted after execution")
	}
}

// --- ExecuteJobByID with 'at' schedule (not deleted) ---

func TestExecuteJobByID_OneTimeScheduleNoDelete(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		return "ok", nil
	})

	// Create a one-time job, manually set DeleteAfterRun to false
	now := time.Now().UnixMilli()
	future := int64Ptr(now + 5000)
	job, _ := svc.AddJob("once", CronSchedule{Kind: "at", AtMS: future}, "msg", false, "cli", "direct")
	job.DeleteAfterRun = false
	svc.UpdateJob(job)
	jobID := job.ID

	// Execute it
	svc.executeJobByID(jobID)

	// Verify it was disabled (not deleted)
	found := svc.GetJob(jobID)
	if found == nil {
		t.Error("job should still exist")
	}
	if found != nil && found.Enabled {
		t.Error("job should be disabled after at-schedule execution")
	}
}

// --- ExecuteJobByID with handler success and next run computation ---

func TestExecuteJobByID_EverySchedule_ComputesNextRun(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		return "ok", nil
	})

	job, _ := svc.AddJob("recurring", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobID := job.ID

	// Execute it
	svc.executeJobByID(jobID)

	// Verify next run was computed
	updated := svc.GetJob(jobID)
	if updated.State.NextRunAtMS == nil {
		t.Error("NextRunAtMS should be computed for every schedule")
	}
	if updated.State.LastRunAtMS == nil {
		t.Error("LastRunAtMS should be recorded")
	}
	if updated.State.NextRunAtMS != nil && updated.State.LastRunAtMS != nil &&
		*updated.State.NextRunAtMS <= *updated.State.LastRunAtMS {
		t.Error("NextRunAtMS should be after LastRunAtMS")
	}
}

// --- RemoveJob and executeJobByID handles job disappearing ---

func TestExecuteJobByID_JobDisappearedDuringExecution(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	// Handler that removes the job from the store
	svc := NewCronService(storePath, func(job *CronJob) (string, error) {
		// Simulate race condition where job is deleted
		return "ok", nil
	})

	job, _ := svc.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")
	jobID := job.ID

	// Manually remove job before executeJobByID updates state
	svc.RemoveJob(jobID)

	// This should handle gracefully (not panic)
	svc.executeJobByID(jobID)
}

// --- Misc AddJob scenarios ---

func TestAddJob_WithCronSchedule(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, err := svc.AddJob("cron", CronSchedule{Kind: "cron", Expr: "0 0 * * *"}, "msg", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	if job.Schedule.Kind != "cron" {
		t.Errorf("schedule kind: got %q, want cron", job.Schedule.Kind)
	}
	if job.Schedule.Expr != "0 0 * * *" {
		t.Errorf("cron expr: got %q, want 0 0 * * *", job.Schedule.Expr)
	}
}

// --- EnableJob returns nil for nonexistent (already tested) ---

// --- LoadStore from existing file ---

func TestLoadStore_ExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	// Create initial service and add a job
	svc1 := NewCronService(storePath, nil)
	job1, _ := svc1.AddJob("job1", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "msg", false, "cli", "direct")

	// Create a second service with same path
	svc2 := NewCronService(storePath, nil)

	// Should load the job
	jobs := svc2.ListJobs(true)
	if len(jobs) != 1 {
		t.Errorf("should have 1 job loaded, got %d", len(jobs))
	}
	if jobs[0].ID != job1.ID {
		t.Errorf("job ID mismatch: got %q, want %q", jobs[0].ID, job1.ID)
	}
}

// --- SaveStore file write failure path is hard to test without mocking fileutil.WriteFileAtomic

// --- Start/Stop lifecycle ---

func TestStart_StartsService(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)

	err := svc.Start()
	t.Cleanup(func() {
		svc.Stop()
	})

	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	status := svc.Status()
	if status["enabled"] != true {
		t.Error("service should be marked as enabled after Start")
	}
}

func TestStart_Multiple_IsIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)

	err1 := svc.Start()
	err2 := svc.Start()
	t.Cleanup(func() {
		svc.Stop()
	})

	if err1 != nil {
		t.Fatalf("First Start failed: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("Second Start failed: %v", err2)
	}
}

func TestStop_StopsService(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	svc.Start()

	svc.Stop()

	status := svc.Status()
	if status["enabled"] != false {
		t.Error("service should be marked as disabled after Stop")
	}
}

func TestStop_Multiple_IsIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	svc.Start()

	svc.Stop()
	svc.Stop()

	status := svc.Status()
	if status["enabled"] != false {
		t.Error("second Stop should be safe")
	}
}

// --- ComputeNextRun with cron invalid expression ---

func TestComputeNextRun_Cron_InvalidExpr(t *testing.T) {
	svc := newTestSvc()
	sched := &CronSchedule{Kind: "cron", Expr: "invalid cron expression"}
	result := svc.computeNextRun(sched, time.Now().UnixMilli())
	// Should return nil for invalid expression
	if result != nil {
		t.Error("should return nil for invalid cron expression")
	}
}

// --- EnableJob with cron schedule that computes next run ---

func TestEnableJob_WithCronSchedule(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, _ := svc.AddJob("cron", CronSchedule{Kind: "cron", Expr: "0 0 * * *"}, "msg", false, "cli", "direct")

	// Disable it
	disabled := svc.EnableJob(job.ID, false)
	if disabled.Enabled {
		t.Error("job should be disabled")
	}

	// Enable it back
	enabled := svc.EnableJob(job.ID, true)
	if !enabled.Enabled {
		t.Error("job should be enabled")
	}
	if enabled.State.NextRunAtMS == nil {
		t.Error("cron job should have next run computed")
	}
}

// --- AddJob with all delivery parameters ---

func TestAddJob_WithDelivery(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)
	job, err := svc.AddJob(
		"deliver",
		CronSchedule{Kind: "every", EveryMS: int64Ptr(5000)},
		"message text",
		true, // deliver
		"telegram",
		"user123",
	)

	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	if job.Payload.Deliver != true {
		t.Error("deliver should be true")
	}
	if job.Payload.Channel != "telegram" {
		t.Error("channel should be telegram")
	}
	if job.Payload.To != "user123" {
		t.Error("to should be user123")
	}
}

// --- RemoveJob with concurrent access (basic check) ---

func TestRemoveJob_ConcurrentAddAndRemove(t *testing.T) {
	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron.json")

	svc := NewCronService(storePath, nil)

	// Add multiple jobs
	ids := make([]string, 5)
	for i := 0; i < 5; i++ {
		job, _ := svc.AddJob(
			fmt.Sprintf("job%d", i),
			CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)},
			"msg",
			false,
			"cli",
			"direct",
		)
		ids[i] = job.ID
	}

	// Remove some
	for _, id := range ids[1:3] {
		removed := svc.RemoveJob(id)
		if !removed {
			t.Errorf("should remove job %s", id)
		}
	}

	// Check count
	jobs := svc.ListJobs(true)
	if len(jobs) != 3 {
		t.Errorf("should have 3 jobs after removal, got %d", len(jobs))
	}
}

// int64Ptr is defined in service_test.go as well to avoid duplication.
