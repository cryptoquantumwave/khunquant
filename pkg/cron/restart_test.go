package cron

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func dailyNoonJob(id string, nextMS int64) CronJob {
	return CronJob{
		ID: id, Name: id, Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "0 12 * * *", TZ: "UTC"},
		State:    CronJobState{NextRunAtMS: int64Ptr(nextMS)},
	}
}

func TestStart_CronJobMissedWithinGrace_RunsOnce(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	missedSlot := clock.Now().Add(-10 * time.Minute).UnixMilli() // 08:50, grace is 60m

	var runs atomic.Int32
	ran := make(chan struct{}, 4)
	cs := storedSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		runs.Add(1)
		ran <- struct{}{}
		return "", nil
	}, dailyNoonJob("weekly", missedSlot))

	startOrFail(t, cs)
	recv(t, ran, "the missed cron slot to run")
	waitFor(t, "run to finish", func() bool { return !isRunning(cs, "weekly") })

	st := cs.GetJob("weekly").State
	if st.LastStatus != StatusOK {
		t.Errorf("LastStatus = %q, want ok", st.LastStatus)
	}
	want := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC).UnixMilli()
	if st.NextRunAtMS == nil || *st.NextRunAtMS != want {
		t.Errorf("after the catch-up the job must move to the next slot (12:00), got %v", st.NextRunAtMS)
	}
	// A further tick at the same time must not run it again: one catch-up, no burst.
	cs.checkJobs()
	select {
	case <-ran:
		t.Error("the job ran a second time for one missed slot")
	case <-time.After(50 * time.Millisecond):
	}
	if n := runs.Load(); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
}

func TestStart_CronJobMissedBeyondGrace_SkipsToNextSlotAndRecordsMissed(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	dueSlot := clock.Now().Add(-3 * time.Hour) // 06:00, beyond the 60m grace

	var runs atomic.Int32
	cs := storedSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		runs.Add(1)
		return "", nil
	}, dailyNoonJob("weekly", dueSlot.UnixMilli()))
	reported := make(chan CronJob, 1)
	cs.SetOnMissed(func(j CronJob) { reported <- j })

	startOrFail(t, cs)
	cs.checkJobs()

	got := cs.GetJob("weekly")
	if !got.Enabled {
		t.Error("a missed recurring job stays enabled")
	}
	if got.State.LastStatus != StatusMissed || !strings.Contains(got.State.LastError, "2025-06-02T06:00:00Z") {
		t.Errorf("state = %+v, want missed with the missed time named", got.State)
	}
	want := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC).UnixMilli()
	if got.State.NextRunAtMS == nil || *got.State.NextRunAtMS != want {
		t.Errorf("next run = %v, want the next slot (12:00)", got.State.NextRunAtMS)
	}
	select {
	case j := <-reported:
		t.Errorf("a recurring job's missed slot is not announced, got %+v", j)
	case <-time.After(50 * time.Millisecond):
	}
	if n := runs.Load(); n != 0 {
		t.Errorf("a slot beyond the grace period must not run, ran %d times", n)
	}
}

func TestStart_NoGraceMeansCronJobsDoNotCatchUp(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	cs := storedSvc(t, clock, nil, dailyNoonJob("weekly", clock.Now().Add(-time.Second).UnixMilli()))
	cs.SetOptions(Options{MisfireGrace: 0})
	cs.mu.Lock()
	cs.reconcileOnStart()
	cs.mu.Unlock()
	if st := cs.GetJob("weekly").State; st.LastStatus != StatusMissed || *st.NextRunAtMS <= clock.Now().UnixMilli() {
		t.Errorf("with a zero grace period nothing catches up, got %+v", st)
	}
}

// A job that was claimed (or running) when the service stopped has already had
// its next run advanced, so Start must neither run it again nor mark it missed:
// an interrupted order-placing turn is never repeated automatically.
func TestStart_ClaimedJobIsNotReRun(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()
	cronNext := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC).UnixMilli()

	cronJob := dailyNoonJob("cron", cronNext)
	cronJob.State.Running = true
	every := everyJob("every", nowMS+30_000, 60_000)
	every.State.Running = true
	atJob := atJob("once", nowMS+60_000)
	atJob.State.Running = true

	var runs atomic.Int32
	cs := storedSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		runs.Add(1)
		return "", nil
	}, cronJob, every, atJob)
	startOrFail(t, cs)
	cs.checkJobs()
	for _, id := range []string{"cron", "every", "once"} {
		j := cs.GetJob(id)
		if j.State.Running {
			t.Errorf("%s: stale Running flag must be cleared", id)
		}
		if j.State.LastStatus != "" {
			t.Errorf("%s: an interrupted job must not be marked missed or run, LastStatus = %q", id, j.State.LastStatus)
		}
	}
	if got := *cs.GetJob("cron").State.NextRunAtMS; got != cronNext {
		t.Errorf("cron next run changed to %d", got)
	}
	<-time.After(50 * time.Millisecond) // a wrongly claimed job would have started by now
	if n := runs.Load(); n != 0 {
		t.Errorf("interrupted jobs were re-run %d times", n)
	}
}

// TestStop_LateRunDoesNotOverwriteStore: a run that outlives Stop (its handler
// ignores ctx) must not write its stale in-memory store over the file that the
// next service - built by a config reload - now owns.
func TestStop_LateRunDoesNotOverwriteStore(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	started := make(chan struct{})
	release := make(chan struct{})
	old := storedSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		close(started)
		<-release // ignores ctx on purpose
		return "", nil
	}, everyJob("j", nowMS+3_600_000, 3_600_000))
	old.SetOptions(Options{Workers: 1, StopTimeout: 50 * time.Millisecond, JobTimeout: time.Minute})
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	old.mu.RLock()
	pool := old.pool
	old.mu.RUnlock()

	if err := old.RunJobNow("j"); err != nil {
		t.Fatal(err)
	}
	recv(t, started, "the run to start")
	old.Stop() // gives up after 50ms with the run still going

	if err := old.RunJobNow("j"); err != ErrServiceStopped {
		t.Errorf("RunJobNow after Stop = %v, want ErrServiceStopped", err)
	}
	old.checkJobs() // not running: must not claim or submit anything

	// The new service takes over the file and saves a change.
	fresh := NewCronService(old.storePath, nil)
	fresh.now = clock.Now
	if _, err := fresh.AddJob("marker", CronSchedule{Kind: "every", EveryMS: int64Ptr(60_000)}, "m", false, "cli", "direct"); err != nil {
		t.Fatal(err)
	}

	close(release)
	pool.wg.Wait() // the old run has now finished, including its (skipped) state update

	reloaded := NewCronService(old.storePath, nil)
	if reloaded.GetJob("j") == nil {
		t.Fatal("job lost")
	}
	found := false
	for _, j := range reloaded.ListJobs(true) {
		if j.Name == "marker" {
			found = true
		}
	}
	if !found {
		t.Error("the stopped service's late run overwrote the new service's change")
	}
	if st := reloaded.GetJob("j").State; st.LastStatus != "" {
		t.Errorf("the late run recorded a result after Stop: %+v", st)
	}
}
