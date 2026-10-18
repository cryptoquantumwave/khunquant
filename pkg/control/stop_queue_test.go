package control_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// /stop drops what waits for a model turn: a queued proactive job and a queued
// user job never run, no fallback runs for either, a running job is cancelled,
// and a job submitted after /stop runs as usual.
func TestStop_DropsTheCognitionQueue(t *testing.T) {
	sched, err := cognition.New(
		config.CognitionConfig{Enabled: true, MaxConcurrentLLM: 1, WakesPerHour: 100, TokensPerDay: 1_000_000},
		sqlitextest.Open(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = sched.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	ran := make(chan string, 8)
	fellBack := make(chan string, 8)
	running := make(chan struct{})
	job := func(name string, kind proactive.TurnKind, pri cognition.Priority, session string) cognition.Job {
		return cognition.Job{
			Kind: kind, Priority: pri, SessionKey: session,
			Run: func(ctx context.Context) (cognition.Usage, error) {
				ran <- name
				if name == "running" {
					close(running)
					<-ctx.Done()
					return cognition.Usage{}, ctx.Err()
				}
				return cognition.Usage{}, nil
			},
			Fallback: func(context.Context, string) error { fellBack <- name; return nil },
		}
	}
	submit := func(j cognition.Job) <-chan cognition.Result {
		ch, err := sched.Submit(context.Background(), j)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	runningRes := submit(job("running", proactive.TurnUser, cognition.PriUser, "s0"))
	<-running
	queuedProactive := submit(job("proactive", proactive.TurnProactive, cognition.PriProactive, "s1"))
	queuedUser := submit(job("user", proactive.TurnUser, cognition.PriUser, "s2"))
	<-ran // "running"

	r := newKillRig(t)
	r.k.Queue = sched
	if sum := r.stop(); !sum.Engaged {
		t.Fatalf("summary = %+v", sum)
	}

	for name, ch := range map[string]<-chan cognition.Result{"running": runningRes, "proactive": queuedProactive, "user": queuedUser} {
		select {
		case res := <-ch:
			if !errors.Is(res.Err, cognition.ErrCancelled) || res.Fallback {
				t.Errorf("%s job: %+v, want cancelled with no fallback", name, res)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s job never finished after /stop", name)
		}
	}
	select {
	case name := <-ran:
		t.Errorf("job %q ran after /stop", name)
	case name := <-fellBack:
		t.Errorf("job %q fell back after /stop", name)
	case <-time.After(100 * time.Millisecond):
	}

	// A message sent after /stop is queued and runs.
	after := submit(job("after", proactive.TurnUser, cognition.PriUser, "s2"))
	select {
	case res := <-after:
		if res.Err != nil {
			t.Errorf("job after /stop: %v", res.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a job submitted after /stop never ran")
	}
}

// A Kill without a queue (cognition off) stops as before.
func TestStop_WithoutAQueue(t *testing.T) {
	r := newKillRig(t)
	if sum := r.stop(); !sum.Engaged {
		t.Fatalf("summary = %+v", sum)
	}
}
