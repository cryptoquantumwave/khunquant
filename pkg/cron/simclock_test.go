package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
)

// A job due at 09:00 runs once after the Sim advances past it, with its next run in sim
// time, and not at all in wall time.
func TestCron_DueJobRunsAfterAdvance(t *testing.T) {
	start := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	sim := simclock.NewSim(start) // the default 1m resolution: a claim happens on a minute boundary
	ran := make(chan string, 4)
	cs := NewCronService(filepath.Join(t.TempDir(), "cron", "jobs.json"), func(j *CronJob) (string, error) {
		ran <- j.Name
		return "ok", nil
	})
	cs.SetClock(sim)
	job, err := cs.AddJob("nine", CronSchedule{Kind: "cron", Expr: "0 9 * * *", TZ: "UTC"}, "hi", false, "cli", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cs.Stop)
	for deadline := time.Now().Add(3 * time.Second); sim.Pending() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the job loop never armed its timer")
		}
	}

	time.Sleep(1200 * time.Millisecond) // more than the 1 s wall tick the loop would otherwise use
	select {
	case <-ran:
		t.Fatal("the job ran in wall time")
	default:
	}

	settle := func(context.Context) error { // let the loop claim, then re-arm
		time.Sleep(20 * time.Millisecond)
		for deadline := time.Now().Add(3 * time.Second); sim.Pending() == 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the job loop did not re-arm")
			}
		}
		return nil
	}
	if _, err := sim.Advance(context.Background(), start.Add(time.Hour+time.Minute), settle); err != nil { // 09:01
		t.Fatal(err)
	}
	select {
	case name := <-ran:
		if name != "nine" {
			t.Fatalf("ran %q", name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the job did not run after the advance")
	}
	select {
	case <-ran:
		t.Fatal("the job ran twice")
	case <-time.After(100 * time.Millisecond):
	}

	got := cs.GetJob(job.ID)
	want := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC).UnixMilli()
	if got == nil || got.State.NextRunAtMS == nil || *got.State.NextRunAtMS != want {
		t.Fatalf("NextRunAtMS = %v, want %d (09:00 the next sim day)", got.State.NextRunAtMS, want)
	}
}
