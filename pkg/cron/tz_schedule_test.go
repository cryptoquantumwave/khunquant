package cron

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestComputeNextRun_TZ_Bangkok(t *testing.T) {
	bkk := mustLoc(t, "Asia/Bangkok")
	svc := newTestSvc()

	// Sunday 2025-06-01 20:00 UTC == Monday 03:00 ICT.
	now := time.Date(2025, 6, 1, 20, 0, 0, 0, time.UTC)

	sched := &CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "Asia/Bangkok"}
	got := svc.computeNextRun(sched, now.UnixMilli())
	if got == nil {
		t.Fatal("expected a next run")
	}
	want := time.Date(2025, 6, 2, 9, 0, 0, 0, bkk)
	if !time.UnixMilli(*got).Equal(want) {
		t.Errorf("next run = %s, want %s (09:00 ICT)", time.UnixMilli(*got).UTC(), want.UTC())
	}
	if h := time.UnixMilli(*got).UTC().Hour(); h != 2 {
		t.Errorf("09:00 ICT is 02:00 UTC, got UTC hour %d", h)
	}

	// The same expression without a TZ is evaluated in the process-local zone, and
	// with TZ=UTC fires seven hours later than Bangkok. This proves TZ is read.
	utc := &CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "UTC"}
	gotUTC := svc.computeNextRun(utc, now.UnixMilli())
	if gotUTC == nil {
		t.Fatal("expected a next run for TZ=UTC")
	}
	if want := time.Date(2025, 6, 2, 9, 0, 0, 0, time.UTC); !time.UnixMilli(*gotUTC).Equal(want) {
		t.Errorf("TZ=UTC next run = %s, want %s", time.UnixMilli(*gotUTC).UTC(), want)
	}
	if *gotUTC-*got != int64(7*time.Hour/time.Millisecond) {
		t.Errorf("Bangkok should fire 7h before UTC, delta = %dms", *gotUTC-*got)
	}
}

func TestComputeNextRun_TZ_NewYork_DST(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	sched := CronSchedule{Kind: "cron", Expr: "30 9 * * *", TZ: "America/New_York"}

	next := func(from time.Time) time.Time {
		t.Helper()
		ms, err := NextRun(sched, from)
		if err != nil || ms == nil {
			t.Fatalf("NextRun(%s) = %v, %v", from, ms, err)
		}
		return time.UnixMilli(*ms).UTC()
	}

	// Spring forward (2025-03-09): 09:30 local is 14:30 UTC before, 13:30 UTC after.
	d1 := next(time.Date(2025, 3, 7, 12, 0, 0, 0, ny)) // -> 03-08 09:30 EST
	d2 := next(d1)                                     // -> 03-09 09:30 EDT
	d3 := next(d2)                                     // -> 03-10 09:30 EDT
	for i, c := range []struct {
		got  time.Time
		want string
	}{
		{d1, "2025-03-08 14:30"},
		{d2, "2025-03-09 13:30"},
		{d3, "2025-03-10 13:30"},
	} {
		if got := c.got.Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("spring-forward day %d: got %s UTC, want %s UTC", i, got, c.want)
		}
	}
	if d2.Sub(d1) != 23*time.Hour {
		t.Errorf("the day containing spring-forward is 23h long, got %s", d2.Sub(d1))
	}

	// Fall back (2025-11-02): 24h+1h day.
	e1 := next(time.Date(2025, 11, 1, 0, 0, 0, 0, ny)) // -> 11-01 09:30 EDT
	e2 := next(e1)                                     // -> 11-02 09:30 EST
	if e2.Sub(e1) != 25*time.Hour {
		t.Errorf("the day containing fall-back is 25h long, got %s", e2.Sub(e1))
	}
	if got := e2.Format("15:04"); got != "14:30" {
		t.Errorf("09:30 EST is 14:30 UTC, got %s", got)
	}

	// A wall-clock time that does not exist (02:30 on spring-forward day) is
	// skipped for that day rather than stalling the schedule.
	gap := CronSchedule{Kind: "cron", Expr: "30 2 * * *", TZ: "America/New_York"}
	ms, err := NextRun(gap, time.Date(2025, 3, 8, 12, 0, 0, 0, ny))
	if err != nil || ms == nil {
		t.Fatalf("NextRun over a DST gap = %v, %v", ms, err)
	}
	if got := time.UnixMilli(*ms).In(ny); got.Day() != 10 || got.Hour() != 2 || got.Minute() != 30 {
		t.Errorf("DST-gap job should resume on the next valid day, got %s", got)
	}
}

func TestNextRun_InvalidTZ(t *testing.T) {
	_, err := NextRun(CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "Not/AZone"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "Not/AZone") {
		t.Errorf("want an error naming the zone, got %v", err)
	}
}

func TestAddJob_InvalidTZRejected(t *testing.T) {
	svc := NewCronService(t.TempDir()+"/jobs.json", nil)

	_, err := svc.AddJob("bad", CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "Mars/Olympus"}, "m", false, "cli", "direct")
	if err == nil {
		t.Fatal("AddJob must reject an unknown timezone")
	}
	if n := len(svc.ListJobs(true)); n != 0 {
		t.Errorf("rejected job must not be stored, have %d jobs", n)
	}

	job, err := svc.AddJob("good", CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "Asia/Bangkok"}, "m", false, "cli", "direct")
	if err != nil {
		t.Fatalf("valid timezone rejected: %v", err)
	}
	job.Schedule.TZ = "Mars/Olympus"
	if err := svc.UpdateJob(job); err == nil {
		t.Error("UpdateJob must reject an unknown timezone")
	}
	if got := svc.GetJob(job.ID).Schedule.TZ; got != "Asia/Bangkok" {
		t.Errorf("rejected update must not change the stored job, TZ = %q", got)
	}
}

func TestUpdateJob_RecomputesNextRunOnScheduleChange(t *testing.T) {
	clock := newFakeClock(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	svc := NewCronService(t.TempDir()+"/jobs.json", nil)
	svc.now = clock.Now

	job, err := svc.AddJob("j", CronSchedule{Kind: "every", EveryMS: int64Ptr(60_000)}, "m", false, "cli", "direct")
	if err != nil {
		t.Fatal(err)
	}
	t0 := clock.Now().UnixMilli()
	if got := *job.State.NextRunAtMS; got != t0+60_000 {
		t.Fatalf("initial next run = %d, want %d", got, t0+60_000)
	}

	// A payload-only edit keeps the persisted next run, however much time passed.
	clock.Advance(10 * time.Second)
	job.Payload.Message = "edited"
	if err := svc.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	if got := *svc.GetJob(job.ID).State.NextRunAtMS; got != t0+60_000 {
		t.Errorf("payload edit moved the next run to %d", got)
	}

	// Changing the interval recomputes it from the current time.
	job = svc.GetJob(job.ID)
	job.Schedule.EveryMS = int64Ptr(3_600_000)
	if err := svc.UpdateJob(job); err != nil {
		t.Fatal(err)
	}
	if got, want := *svc.GetJob(job.ID).State.NextRunAtMS, clock.Now().UnixMilli()+3_600_000; got != want {
		t.Errorf("after interval change next run = %d, want %d", got, want)
	}

	// Changing a cron timezone recomputes it too.
	cj, err := svc.AddJob("c", CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "UTC"}, "m", false, "cli", "direct")
	if err != nil {
		t.Fatal(err)
	}
	before := *cj.State.NextRunAtMS
	cj.Schedule.TZ = "Asia/Bangkok"
	if err := svc.UpdateJob(cj); err != nil {
		t.Fatal(err)
	}
	after := *svc.GetJob(cj.ID).State.NextRunAtMS
	if after == before {
		t.Error("changing the timezone must recompute the next run")
	}
	if got := time.UnixMilli(after).In(mustLoc(t, "Asia/Bangkok")).Hour(); got != 9 {
		t.Errorf("next run should be 09:00 ICT, got hour %d", got)
	}
}

// TestAcceptance_BangkokMondayNineFiresAt0900ICT drives the whole path with a
// fake clock: a job with tz Asia/Bangkok and "0 9 * * 1" is not due at 08:59:59
// ICT and is claimed and run at 09:00:00 ICT, whatever the host zone is.
func TestAcceptance_BangkokMondayNineFiresAt0900ICT(t *testing.T) {
	bkk := mustLoc(t, "Asia/Bangkok")
	clock := newFakeClock(time.Date(2025, 5, 28, 12, 0, 0, 0, bkk)) // Wednesday
	ran := make(chan time.Time, 1)
	svc := newClockSvc(t, clock, func(_ context.Context, _ *CronJob) (string, error) {
		ran <- clock.Now()
		return "", nil
	})
	if _, err := svc.AddJob("weekly", CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "Asia/Bangkok"},
		"m", false, "cli", "direct"); err != nil {
		t.Fatal(err)
	}

	clock.Set(time.Date(2025, 6, 2, 8, 59, 59, 0, bkk))
	svc.checkJobs()
	select {
	case at := <-ran:
		t.Fatalf("job ran early at %s", at)
	default:
	}

	clock.Set(time.Date(2025, 6, 2, 9, 0, 0, 0, bkk))
	svc.checkJobs()
	select {
	case at := <-ran:
		if got := at.In(bkk).Format("Mon 15:04"); got != "Mon 09:00" {
			t.Errorf("ran at %s ICT, want Mon 09:00", got)
		}
		if at.UTC().Hour() != 2 {
			t.Errorf("09:00 ICT should be 02:00 UTC, got %s", at.UTC())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("job did not run at 09:00 ICT")
	}
}
