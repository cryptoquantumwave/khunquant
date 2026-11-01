package scenario

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

func TestRunUntil_ChunksAndStopsOnTarget(t *testing.T) {
	sup := newFakeSup(t)
	info, _ := sup.Up(context.Background(), supervisor.UpOptions{ClockStart: "2026-10-20T09:00:00Z", ClockMode: "manual"})
	c, _ := sup.Client(info.Name)
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

	res, err := RunUntil(context.Background(), c, UntilOptions{Target: start.Add(150 * time.Minute), Step: time.Hour})
	if err != nil || !res.Reached.Equal(start.Add(150*time.Minute)) || res.Chunks != 3 || res.Matched || res.Stopped != "" {
		t.Fatalf("to a time: %+v, %v (1h, 1h, then 30m)", res, err)
	}

	// A check ends it at the first chunk whose journal matches.
	checks := 0
	res, err = RunUntil(context.Background(), c, UntilOptions{Step: 10 * time.Minute, Check: func(context.Context) (bool, *labjournal.Entry, error) {
		checks++
		return checks == 4, &labjournal.Entry{Seq: 7}, nil
	}})
	if err != nil || !res.Matched || res.Chunks != 3 || res.Entry == nil || res.Entry.Seq != 7 || !res.Reached.Equal(start.Add(180*time.Minute)) {
		t.Fatalf("until: %+v, %v (check before the first chunk, and after each of three)", res, err)
	}

	// max_sim and max_wall bound a run that never matches.
	res, err = RunUntil(context.Background(), c, UntilOptions{Step: 10 * time.Minute, MaxSim: 25 * time.Minute, Check: func(context.Context) (bool, *labjournal.Entry, error) { return false, nil, nil }})
	if err != nil || res.Stopped != "max_sim" || res.Matched {
		t.Fatalf("max_sim: %+v, %v", res, err)
	}
	res, err = RunUntil(context.Background(), c, UntilOptions{Step: time.Minute, MaxWall: time.Nanosecond, Check: func(context.Context) (bool, *labjournal.Entry, error) { return false, nil, nil }})
	if err != nil || res.Stopped != "max_wall" {
		t.Fatalf("max_wall: %+v, %v", res, err)
	}
}
