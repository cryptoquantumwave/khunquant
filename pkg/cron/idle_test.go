package cron

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A job that is due but not yet claimed is a timer (the next tick claims it); one that is
// queued behind a busy worker, or running, keeps the service busy; asking changes nothing.
func TestCron_IdleDueUnclaimedIsTimer(t *testing.T) {
	entered, release := make(chan string, 2), make(chan struct{})
	var once sync.Once
	cs := NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), func(j *CronJob) (string, error) {
		entered <- j.ID
		<-release
		return "ok", nil
	})
	cs.SetOptions(Options{Workers: 1})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	cs.running = true

	past := time.Now().Add(-time.Second).UnixMilli()
	cs.store.Jobs = []CronJob{
		{ID: "a", Enabled: true, Name: "a", Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, State: CronJobState{NextRunAtMS: &past}},
		{ID: "b", Enabled: true, Name: "b", Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(60000)}, State: CronJobState{NextRunAtMS: &past}},
	}
	if !cs.Idle() {
		t.Fatal("jobs that are due but not claimed are a timer, not work")
	}

	cs.checkJobs() // claims both: a runs, b waits in the pool queue behind the only worker
	<-entered
	if cs.Idle() || cs.Idle() {
		t.Fatal("idle with a job running and another queued")
	}
	cs.mu.RLock()
	running := 0
	for _, j := range cs.store.Jobs {
		if j.State.Running {
			running++
		}
	}
	cs.mu.RUnlock()
	if running != 2 {
		t.Fatalf("Idle changed the claims: %d running", running)
	}

	once.Do(func() { close(release) })
	waitFor(t, "both jobs to finish", func() bool { return !isRunning(cs, "a") && !isRunning(cs, "b") })
	waitFor(t, "idle", cs.Idle)
}
