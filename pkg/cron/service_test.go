package cron

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveStore_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not enforced on Windows")
	}

	tmpDir := t.TempDir()
	storePath := filepath.Join(tmpDir, "cron", "jobs.json")

	cs := NewCronService(storePath, nil)

	_, err := cs.AddJob("test", CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, "hello", false, "cli", "direct")
	if err != nil {
		t.Fatalf("AddJob failed: %v", err)
	}

	info, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("cron store has permission %04o, want 0600", perm)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

func TestCheckJobs_NotRunning(t *testing.T) {
	tmpDir := t.TempDir()
	cs := NewCronService(filepath.Join(tmpDir, "cron", "jobs.json"), nil)
	// cs.running is false by default — checkJobs should return early
	cs.checkJobs() // must not panic
}

func TestCheckJobs_NoDueJobs(t *testing.T) {
	tmpDir := t.TempDir()
	cs := NewCronService(filepath.Join(tmpDir, "cron", "jobs.json"), nil)
	cs.running = true

	futureMS := time.Now().Add(1 * time.Hour).UnixMilli()
	cs.store.Jobs = []CronJob{
		{
			ID:      "j1",
			Enabled: true,
			Name:    "future",
			State:   CronJobState{NextRunAtMS: &futureMS},
		},
	}
	cs.checkJobs() // no jobs should execute
}

func TestCheckJobs_DueJob_ExecutesAndUpdates(t *testing.T) {
	tmpDir := t.TempDir()
	executed := make(chan string, 1)
	handler := func(job *CronJob) (string, error) {
		executed <- job.ID
		return "ok", nil
	}
	cs := NewCronService(filepath.Join(tmpDir, "cron", "jobs.json"), handler)
	cs.running = true

	pastMS := time.Now().Add(-1 * time.Second).UnixMilli()
	cs.store.Jobs = []CronJob{
		{
			ID:       "j-due",
			Enabled:  true,
			Name:     "due-job",
			Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)},
			State:    CronJobState{NextRunAtMS: &pastMS},
		},
	}
	cs.checkJobs()

	select {
	case id := <-executed:
		if id != "j-due" {
			t.Errorf("executed job ID = %q, want j-due", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected checkJobs to execute due job")
	}

	// The handler runs on a pool worker; wait for its state update (and the store
	// write it makes) before the test's temp dir is removed.
	waitFor(t, "state update", func() bool { return !isRunning(cs, "j-due") })
	st := cs.GetJob("j-due").State
	if st.LastStatus != StatusOK || st.LastRunAtMS == nil || st.NextRunAtMS == nil {
		t.Errorf("state after run = %+v, want ok with last/next run recorded", st)
	}
}

func TestCheckJobs_DisabledJob_NotExecuted(t *testing.T) {
	tmpDir := t.TempDir()
	executed := make(chan string, 1)
	handler := func(job *CronJob) (string, error) {
		executed <- job.ID
		return "", nil
	}
	cs := NewCronService(filepath.Join(tmpDir, "cron", "jobs.json"), handler)
	cs.running = true

	pastMS := time.Now().Add(-1 * time.Second).UnixMilli()
	cs.store.Jobs = []CronJob{
		{
			ID:      "j-disabled",
			Enabled: false,
			Name:    "disabled",
			State:   CronJobState{NextRunAtMS: &pastMS},
		},
	}
	cs.checkJobs()

	select {
	case id := <-executed:
		t.Errorf("disabled job should not execute, got %q", id)
	case <-time.After(100 * time.Millisecond):
		// expected: no execution
	}
}
