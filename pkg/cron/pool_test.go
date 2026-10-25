package cron

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for the service's now seam.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const testWait = 10 * time.Second

// newClockSvc returns a file-backed service on a fake clock that is marked
// running without a ticker, so tests drive checkJobs by hand.
func newClockSvc(t *testing.T, clock *fakeClock, handler JobHandlerCtx, jobs ...CronJob) *CronService {
	t.Helper()
	cs := NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
	cs.now = clock.Now
	cs.onJob = handler
	cs.store.Jobs = jobs
	cs.running = true
	t.Cleanup(func() {
		cs.mu.Lock()
		p := cs.pool
		cs.pool = nil
		cs.running = false
		cs.mu.Unlock()
		if p != nil {
			p.shutdown(testWait)
		}
	})
	return cs
}

func everyJob(id string, nextMS int64, everyMS int64) CronJob {
	return CronJob{
		ID: id, Name: id, Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(everyMS)},
		State:    CronJobState{NextRunAtMS: int64Ptr(nextMS)},
	}
}

// waitFor polls cond until it holds. The deadline only bounds a failing test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

func isRunning(cs *CronService, id string) bool {
	j := cs.GetJob(id)
	return j != nil && j.State.Running
}

func TestCheckJobs_SlowJobDoesNotDelayOthers(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 8, 59, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	slowStarted := make(chan struct{})
	release := make(chan struct{})
	fastRan := make(chan string, 1)
	svc := newClockSvc(t, clock, func(_ context.Context, job *CronJob) (string, error) {
		if job.ID == "slow" {
			close(slowStarted)
			<-release
			return "", nil
		}
		fastRan <- job.ID
		return "", nil
	},
		// The slow job is first in the store, so a serial runner would run it first.
		everyJob("slow", nowMS-1000, 60_000),
		everyJob("alert", nowMS-1000, 60_000),
	)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	svc.checkJobs() // returns without waiting for either job
	recv(t, slowStarted, "slow job to start")
	if got := recv(t, fastRan, "the other due job"); got != "alert" {
		t.Errorf("ran %q, want alert", got)
	}
	if !isRunning(svc, "slow") {
		t.Error("slow job should still be running while the other job finished")
	}

	releaseOnce.Do(func() { close(release) })
	waitFor(t, "slow job to finish", func() bool { return !isRunning(svc, "slow") })
	if st := svc.GetJob("slow").State; st.LastStatus != StatusOK {
		t.Errorf("slow job LastStatus = %q, want ok", st.LastStatus)
	}
}

func TestCheckJobs_SaturatedPoolDoesNotBlockTicker(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	started := make(chan string, 4)
	release := make(chan struct{})
	svc := newClockSvc(t, clock, func(_ context.Context, job *CronJob) (string, error) {
		started <- job.ID
		if job.ID == "blocker" {
			<-release
		}
		return "", nil
	}, everyJob("blocker", nowMS-1, 60_000), everyJob("queued", nowMS+60_000, 60_000))
	svc.opts.Workers = 1
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	svc.checkJobs()
	recv(t, started, "blocker to start")

	// The only worker is busy. A second due job is claimed and queued, and
	// checkJobs still returns immediately.
	svc.mu.Lock()
	next := nowMS - 1
	svc.store.Jobs[1].State.NextRunAtMS = &next
	svc.mu.Unlock()
	svc.checkJobs()
	if !isRunning(svc, "queued") {
		t.Fatal("queued job should be claimed")
	}
	select {
	case id := <-started:
		t.Fatalf("job %q ran although the only worker is busy", id)
	default:
	}

	releaseOnce.Do(func() { close(release) })
	if got := recv(t, started, "queued job to run once the worker frees up"); got != "queued" {
		t.Errorf("ran %q, want queued", got)
	}
}

func TestCheckJobs_FullQueueLeavesJobDue(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	jobs := []CronJob{everyJob("blocker", nowMS-1, 60_000)}
	for i := 0; i < minPoolQueueSize+1; i++ {
		jobs = append(jobs, everyJob("f"+string(rune('A'+i/26))+string(rune('a'+i%26)), nowMS+60_000, 60_000))
	}
	svc := newClockSvc(t, clock, func(_ context.Context, job *CronJob) (string, error) {
		if job.ID == "blocker" {
			close(started)
			<-release
		}
		return "", nil
	}, jobs...)
	svc.opts.Workers = 1
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	svc.checkJobs()
	recv(t, started, "blocker to start")

	// Make every filler due: the queue holds minPoolQueueSize, one job overflows.
	svc.mu.Lock()
	due := nowMS - 1
	for i := 1; i < len(svc.store.Jobs); i++ {
		d := due
		svc.store.Jobs[i].State.NextRunAtMS = &d
	}
	svc.mu.Unlock()
	svc.checkJobs()

	claimed, left := 0, 0
	for _, j := range svc.ListJobs(true)[1:] {
		if j.State.Running {
			claimed++
		} else {
			left++
			if j.State.NextRunAtMS == nil || *j.State.NextRunAtMS != due {
				t.Errorf("unclaimed job %s must keep its due time", j.ID)
			}
		}
	}
	if claimed != minPoolQueueSize || left != 1 {
		t.Errorf("claimed=%d left=%d, want %d and 1", claimed, left, minPoolQueueSize)
	}
	releaseOnce.Do(func() { close(release) })
}

func TestCheckJobs_JobDoesNotOverlapItself(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	var starts atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	svc := newClockSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		starts.Add(1)
		started <- struct{}{}
		<-release
		return "", nil
	}, everyJob("j", nowMS-1, 60_000))
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	svc.checkJobs()
	recv(t, started, "first run")

	// Three further occurrences arrive while the first run is still going.
	for i := 0; i < 3; i++ {
		clock.Advance(61 * time.Second)
		svc.checkJobs()
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("job started %d times while still running, want 1", got)
	}
	// The skipped occurrences did not pile up: the next run is in the future.
	if next := *svc.GetJob("j").State.NextRunAtMS; next <= clock.Now().UnixMilli() {
		t.Errorf("next run %d should be after now %d", next, clock.Now().UnixMilli())
	}

	releaseOnce.Do(func() { close(release) })
	waitFor(t, "first run to finish", func() bool { return !isRunning(svc, "j") })

	// Once finished it is schedulable again.
	clock.Advance(61 * time.Second)
	svc.checkJobs()
	waitFor(t, "second run", func() bool { return starts.Load() == 2 })
}

func TestCheckJobs_EveryKeepsPhaseWhenLate(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	scheduled := clock.Now().UnixMilli() - 130_000 // due 130s ago, interval 60s
	ran := make(chan struct{}, 1)
	svc := newClockSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		ran <- struct{}{}
		return "", nil
	}, everyJob("j", scheduled, 60_000))

	svc.checkJobs()
	recv(t, ran, "run")
	waitFor(t, "run to finish", func() bool { return !isRunning(svc, "j") })

	// Next slot on the original grid (scheduled + 3*60s), not now+60s: no drift, and
	// the two missed intervals are not replayed.
	if got, want := *svc.GetJob("j").State.NextRunAtMS, scheduled+3*60_000; got != want {
		t.Errorf("next run = %d, want %d", got, want)
	}
}

func TestRunJobNow_NoOverlap(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	started := make(chan struct{}, 4)
	release := make(chan struct{})
	svc := newClockSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		started <- struct{}{}
		<-release
		return "", nil
	}, everyJob("j", nowMS+3_600_000, 3_600_000))
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	if err := svc.RunJobNow("j"); err != nil {
		t.Fatalf("first RunJobNow: %v", err)
	}
	recv(t, started, "manual run to start")

	err := svc.RunJobNow("j")
	if err != ErrJobRunning {
		t.Fatalf("second RunJobNow = %v, want ErrJobRunning", err)
	}
	// A due tick does not start it either.
	svc.mu.Lock()
	due := nowMS - 1
	svc.store.Jobs[0].State.NextRunAtMS = &due
	svc.mu.Unlock()
	svc.checkJobs()
	select {
	case <-started:
		t.Fatal("scheduled tick started a job that a manual run still holds")
	default:
	}

	releaseOnce.Do(func() { close(release) })
	waitFor(t, "manual run to finish", func() bool { return !isRunning(svc, "j") })
	if err := svc.RunJobNow("j"); err != nil {
		t.Errorf("RunJobNow after completion: %v", err)
	}
	recv(t, started, "third run")
}

func TestExecuteJob_PanicRecovered(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	ranAfter := make(chan struct{}, 1)
	svc := newClockSvc(t, clock, func(_ context.Context, job *CronJob) (string, error) {
		if job.ID == "boom" {
			panic("kaboom")
		}
		ranAfter <- struct{}{}
		return "", nil
	}, everyJob("boom", nowMS-1, 60_000), everyJob("next", nowMS-1, 60_000))
	svc.opts.Workers = 1 // the same worker must survive the panic and run the next job

	svc.checkJobs()
	recv(t, ranAfter, "the job after the panicking one")
	waitFor(t, "state update", func() bool { return !isRunning(svc, "boom") && !isRunning(svc, "next") })

	st := svc.GetJob("boom").State
	if st.LastStatus != StatusError {
		t.Errorf("LastStatus = %q, want error", st.LastStatus)
	}
	if st.LastError == "" || !strings.Contains(st.LastError, "kaboom") {
		t.Errorf("LastError = %q, want it to carry the panic value", st.LastError)
	}
	if st.NextRunAtMS == nil {
		t.Error("a panicking recurring job must keep a next run")
	}
	if got := svc.GetJob("next").State.LastStatus; got != StatusOK {
		t.Errorf("neighbouring job LastStatus = %q, want ok", got)
	}
}

func TestExecuteJob_TimeoutRecordedAsError(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	svc := newClockSvc(t, clock, func(ctx context.Context, _ *CronJob) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, everyJob("slow", nowMS-1, 60_000))
	svc.opts.JobTimeout = 20 * time.Millisecond

	svc.checkJobs()
	waitFor(t, "timeout to be recorded", func() bool {
		return svc.GetJob("slow").State.LastStatus == StatusError
	})
	st := svc.GetJob("slow").State
	if !strings.Contains(st.LastError, "timed out") {
		t.Errorf("LastError = %q, want it to say the job timed out", st.LastError)
	}
	if st.Running {
		t.Error("Running must be cleared after a timeout")
	}
}

func TestExecuteJob_NoTimeoutWhenZero(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()
	hadDeadline := make(chan bool, 1)
	svc := newClockSvc(t, clock, func(ctx context.Context, _ *CronJob) (string, error) {
		_, ok := ctx.Deadline()
		hadDeadline <- ok
		return "", nil
	}, everyJob("j", nowMS-1, 60_000))
	svc.opts.JobTimeout = 0
	svc.checkJobs()
	if recv(t, hadDeadline, "handler") {
		t.Error("JobTimeout=0 must not impose a deadline")
	}
}

func TestListJobs_ReturnsCopies(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	svc := newClockSvc(t, clock, nil, CronJob{
		ID: "a", Name: "a", Enabled: true,
		Schedule: CronSchedule{Kind: "every", EveryMS: int64Ptr(60_000), AtMS: int64Ptr(5)},
		State:    CronJobState{NextRunAtMS: int64Ptr(1000), LastRunAtMS: int64Ptr(2000)},
	})

	for _, include := range []bool{true, false} {
		got := svc.ListJobs(include)
		if len(got) != 1 {
			t.Fatalf("ListJobs(%v) returned %d jobs", include, len(got))
		}
		got[0].Name = "mutated"
		*got[0].Schedule.EveryMS = 1
		*got[0].Schedule.AtMS = 1
		*got[0].State.NextRunAtMS = 1
		*got[0].State.LastRunAtMS = 1
		got[0].State.Running = true
		_ = append(got, CronJob{ID: "extra"})

		fresh := svc.GetJob("a")
		if fresh.Name != "a" || *fresh.Schedule.EveryMS != 60_000 || *fresh.Schedule.AtMS != 5 ||
			*fresh.State.NextRunAtMS != 1000 || *fresh.State.LastRunAtMS != 2000 || fresh.State.Running {
			t.Fatalf("mutating the ListJobs(%v) result changed the service state: %+v", include, fresh)
		}
	}
	// GetJob is a deep copy as well.
	g := svc.GetJob("a")
	*g.State.NextRunAtMS = 7
	if *svc.GetJob("a").State.NextRunAtMS != 1000 {
		t.Error("GetJob result shares pointers with the store")
	}
}

// --- Start / persistence ---

// startedSvc persists jobs, then returns a service on the fake clock that has
// NOT been started, so a test can inspect or Start it.
func storedSvc(t *testing.T, clock *fakeClock, handler JobHandlerCtx, jobs ...CronJob) *CronService {
	t.Helper()
	cs := NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), nil)
	cs.now = clock.Now
	cs.onJob = handler
	cs.store.Jobs = jobs
	if err := cs.saveStoreUnsafe(); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	return cs
}

func startOrFail(t *testing.T, cs *CronService) {
	t.Helper()
	if err := cs.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(cs.Stop)
}

func TestStart_PreservesEveryPhase(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()
	future := nowMS + 17_000 // 17s into a 60s interval

	cs := storedSvc(t, clock, nil, everyJob("j", future, 60_000))
	startOrFail(t, cs)
	if got := *cs.GetJob("j").State.NextRunAtMS; got != future {
		t.Errorf("Start moved the persisted next run: got %d, want %d", got, future)
	}

	// A restart (config reload builds a new service on the same store) keeps it too.
	cs.Stop()
	cs2 := NewCronService(cs.storePath, nil)
	cs2.now = clock.Now
	startOrFail(t, cs2)
	if got := *cs2.GetJob("j").State.NextRunAtMS; got != future {
		t.Errorf("reload moved the persisted next run: got %d, want %d", got, future)
	}

	// A next run in the past means "due now": Start leaves it for the first tick.
	past := nowMS - 5_000
	cs3 := storedSvc(t, clock, nil, everyJob("p", past, 60_000))
	cs3.mu.Lock()
	cs3.reconcileOnStart()
	cs3.mu.Unlock()
	if got := *cs3.GetJob("p").State.NextRunAtMS; got != past {
		t.Errorf("a past every-job next run must be kept (due now), got %d want %d", got, past)
	}

	// An interval-less / implausibly distant value is repaired rather than trusted.
	far := nowMS + 10*60_000
	cs4 := storedSvc(t, clock, nil, everyJob("far", far, 60_000))
	cs4.mu.Lock()
	cs4.reconcileOnStart()
	cs4.mu.Unlock()
	if got := *cs4.GetJob("far").State.NextRunAtMS; got != nowMS+60_000 {
		t.Errorf("implausible next run should restart the phase, got %d", got)
	}
}

func TestStart_ClearsStaleRunningFlag(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()
	j := everyJob("j", nowMS+30_000, 60_000)
	j.State.Running = true

	cs := storedSvc(t, clock, nil, j)
	startOrFail(t, cs)
	if isRunning(cs, "j") {
		t.Error("Start must clear a Running flag left by a crashed process")
	}
	reloaded := NewCronService(cs.storePath, nil)
	if reloaded.GetJob("j").State.Running {
		t.Error("the cleared flag must be persisted")
	}
}

func TestStart_RecomputesNilNextRunForRecurring(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	nowMS := clock.Now().UnixMilli()

	every := everyJob("every", 0, 60_000)
	every.State.NextRunAtMS = nil // what a crash after the old claim step left behind
	cron := CronJob{
		ID: "cron", Name: "cron", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "0 12 * * *", TZ: "UTC"},
	}
	staleCron := CronJob{
		ID: "stale", Name: "stale", Enabled: true,
		Schedule: CronSchedule{Kind: "cron", Expr: "0 12 * * *", TZ: "UTC"},
		State:    CronJobState{NextRunAtMS: int64Ptr(nowMS - 3*3_600_000)}, // beyond the 60m grace
	}
	disabled := everyJob("off", 0, 60_000)
	disabled.Enabled = false
	disabled.State.NextRunAtMS = nil

	cs := storedSvc(t, clock, nil, every, cron, staleCron, disabled)
	startOrFail(t, cs)

	if got := cs.GetJob("every").State.NextRunAtMS; got == nil || *got != nowMS+60_000 {
		t.Errorf("every job next run = %v, want now+60s", got)
	}
	want := time.Date(2025, 6, 2, 12, 0, 0, 0, time.UTC).UnixMilli()
	for _, id := range []string{"cron", "stale"} {
		if got := cs.GetJob(id).State.NextRunAtMS; got == nil || *got != want {
			t.Errorf("%s job next run = %v, want %d", id, got, want)
		}
	}
	if got := cs.GetJob("off").State.NextRunAtMS; got != nil {
		t.Errorf("disabled job must not get a next run, got %d", *got)
	}
}

func atJob(id string, dueMS int64) CronJob {
	return CronJob{
		ID: id, Name: id, Enabled: true, DeleteAfterRun: true,
		Schedule: CronSchedule{Kind: "at", AtMS: int64Ptr(dueMS)},
		State:    CronJobState{NextRunAtMS: int64Ptr(dueMS)},
		Payload:  CronPayload{Channel: "telegram", To: "42"},
	}
}

func TestStart_MissedAtJob_WithinGrace_Runs(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	missedBy := 10 * time.Minute
	job := atJob("late", clock.Now().Add(-missedBy).UnixMilli())

	ran := make(chan string, 1)
	cs := storedSvc(t, clock, func(_ context.Context, j *CronJob) (string, error) {
		ran <- j.ID
		return "", nil
	}, job)
	cs.SetOptions(Options{Workers: 2, MisfireGrace: time.Hour, JobTimeout: time.Minute})
	reported := make(chan CronJob, 1)
	cs.SetOnMissed(func(j CronJob) { reported <- j })

	startOrFail(t, cs)
	if got := recv(t, ran, "the within-grace one-shot job to run"); got != "late" {
		t.Errorf("ran %q", got)
	}
	select {
	case j := <-reported:
		t.Errorf("job within grace must not be reported as missed: %+v", j)
	default:
	}
	// One-shot with DeleteAfterRun: removed after its single run.
	waitFor(t, "one-shot cleanup", func() bool { return cs.GetJob("late") == nil })
}

func TestStart_MissedAtJob_Expired_Reported(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	dueMS := clock.Now().Add(-3 * time.Hour).UnixMilli()

	var calls atomic.Int32
	reported := make(chan CronJob, 4)
	ran := make(chan string, 1)
	cs := storedSvc(t, clock, func(_ context.Context, j *CronJob) (string, error) {
		ran <- j.ID
		return "", nil
	}, atJob("old", dueMS))
	cs.SetOptions(Options{Workers: 2, MisfireGrace: time.Hour, JobTimeout: time.Minute})
	cs.SetOnMissed(func(j CronJob) {
		calls.Add(1)
		reported <- j
	})

	startOrFail(t, cs)
	j := recv(t, reported, "missed callback")
	if j.ID != "old" || j.Payload.Channel != "telegram" || j.Payload.To != "42" || *j.Schedule.AtMS != dueMS {
		t.Errorf("callback got %+v", j)
	}
	if j.State.LastStatus != StatusMissed || j.Enabled {
		t.Errorf("callback job should already be marked missed and disabled: %+v", j.State)
	}

	stored := cs.GetJob("old")
	if stored == nil || stored.Enabled || stored.State.LastStatus != StatusMissed || stored.State.NextRunAtMS != nil {
		t.Fatalf("expired one-shot job must be kept, disabled and marked missed, got %+v", stored)
	}
	select {
	case id := <-ran:
		t.Errorf("expired job %q must not run", id)
	default:
	}

	// A restart must not report it again.
	cs.Stop()
	cs2 := NewCronService(cs.storePath, nil)
	cs2.now = clock.Now
	cs2.SetOptions(Options{Workers: 2, MisfireGrace: time.Hour})
	cs2.SetOnMissed(func(CronJob) { calls.Add(1) })
	startOrFail(t, cs2)
	cs2.Stop() // after Stop no callback goroutine is pending that Start spawned
	if n := calls.Load(); n != 1 {
		t.Errorf("missed callback invoked %d times across a restart, want exactly 1", n)
	}
}

func TestStart_NoGraceMeansAnyMissIsExpired(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC))
	cs := storedSvc(t, clock, nil, atJob("old", clock.Now().Add(-time.Second).UnixMilli()))
	cs.SetOptions(Options{MisfireGrace: 0})
	cs.mu.Lock()
	missed := cs.reconcileOnStart()
	cs.mu.Unlock()
	if len(missed) != 1 {
		t.Errorf("with a zero grace period a past one-shot job is missed, got %d", len(missed))
	}
}
