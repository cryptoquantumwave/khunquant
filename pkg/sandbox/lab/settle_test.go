package lab

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

// A settle journals one entry with its outcome, and an advance settles the registry
// after every step and fails when a component never goes idle.
func TestLabSettle_JournalsAndGatesAdvance(t *testing.T) {
	l := manualLab(t)
	if l.Quiesce == nil || l.Settle == nil {
		t.Fatal("a lab settles over its registry by default")
	}
	rep, err := l.SettleWait(context.Background(), time.Second)
	if err != nil || rep.Polls < 2 {
		t.Fatalf("idle lab: %+v, %v", rep, err)
	}

	var busy = true
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return !busy }))
	_, err = l.SettleWait(context.Background(), 30*time.Millisecond)
	var be *quiesce.BusyError
	if !errors.As(err, &be) || len(be.Names) != 1 || be.Names[0] != "agent" {
		t.Fatalf("err = %v, want a BusyError naming agent", err)
	}

	if err := l.Journal.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	es, err := l.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{KindSettle}})
	if err != nil || len(es) != 2 {
		t.Fatalf("settle entries = %d, %v; want 2", len(es), err)
	}
	if got := string(es[1].Payload); !contains(got, `"ok":false`) || !contains(got, `"busy":["agent"]`) {
		t.Fatalf("payload of the failed settle: %s", got)
	}
	if got := string(es[0].Payload); !contains(got, `"ok":true`) || !contains(got, `"busy":[]`) {
		t.Fatalf("payload of the passed settle: %s", got)
	}

	// An advance settles after each step: with the agent never idle it ends with the error.
	l.Settle = func(ctx context.Context) error { _, err := l.SettleWait(ctx, 30*time.Millisecond); return err }
	if _, err := call(t, l, `{"op":"advance","by":"2m"}`); err == nil {
		t.Fatal("an advance passed a settle that never went idle")
	}
	busy = false
	if _, err := call(t, l, `{"op":"advance","by":"2m"}`); err != nil {
		t.Fatalf("advance on an idle registry: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
