package lab

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
)

func settleEntries(t *testing.T, l *Lab) []string {
	t.Helper()
	if err := l.Journal.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	es, err := l.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{KindSettle}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, string(e.Payload))
	}
	return out
}

// A settle the caller's context ended is journaled cut:true; one the wall cap ended is
// ok:false with cut:false, and a settle that passed has neither.
func TestSettleWait_CutIsJournaledApartFromAFailure(t *testing.T) {
	l := manualLab(t)
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return false }))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := l.SettleWait(ctx, time.Minute)
	if !IsCut(err) {
		t.Fatalf("a settle ended by its context: IsCut(%v) = false", err)
	}
	_, err = l.SettleWait(context.Background(), 20*time.Millisecond)
	var be *quiesce.BusyError
	if !errors.As(err, &be) || IsCut(err) {
		t.Fatalf("a settle ended by the wall cap: %v, IsCut = %v; want a BusyError that is not a cut", err, IsCut(err))
	}

	got := settleEntries(t, l)
	if len(got) != 2 || !contains(got[0], `"ok":false`) || !contains(got[0], `"cut":true`) ||
		!contains(got[1], `"ok":false`) || !contains(got[1], `"cut":false`) {
		t.Fatalf("settle entries = %v, want a cut one then a failed one", got)
	}
}

// The step an advance was cut inside never finished settling. The next advance call
// settles before it moves the clock, so the next timer cannot fire over that step.
func TestClockAdvance_SettlesBeforeMovingAfterACut(t *testing.T) {
	l := manualLab(t)
	t1, t2 := l.Clock.After(time.Minute), l.Clock.After(2*time.Minute)
	fired := func(ch <-chan time.Time) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	callCtx, cut := context.WithCancel(context.Background())
	defer cut()

	var n int
	var t2FiredAtSettle3 bool
	l.Settle = func(ctx context.Context) error {
		switch n++; n {
		case 1: // before the first call moves the clock
			return nil
		case 2: // after the step that fired t1: the budget ends in here
			cut()
			return ctx.Err()
		case 3: // before the second call moves the clock
			t2FiredAtSettle3 = fired(t2)
		}
		return nil
	}
	args := json.RawMessage(`{"by":"3m"}`)
	first, err := l.clockAdvance(callCtx, args)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if b, _ := json.Marshal(first); contains(string(b), `"done":true`) || !fired(t1) || fired(t2) {
		t.Fatalf("first call: %s, t1 fired %v: want done:false with t1 fired and t2 not", b, fired(t1))
	}
	if _, err := l.clockAdvance(context.Background(), args); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n < 3 || t2FiredAtSettle3 {
		t.Fatalf("settles = %d, t2 fired before the second call's first settle = %v: the clock moved before that settle", n, t2FiredAtSettle3)
	}
	if !fired(t2) {
		t.Error("the second call did not fire t2")
	}
}

// A budget that ends in the settle before moving moves nothing and says done:false; a
// settle that fails on its own cap there is an error, as one after a step is, and nothing
// moved either.
func TestClockAdvance_PreSettleCutOrFailed(t *testing.T) {
	l := manualLab(t)
	t1 := l.Clock.After(time.Minute)
	start := l.Clock.Now()

	ctx, cut := context.WithCancel(context.Background())
	l.Settle = func(c context.Context) error { cut(); return c.Err() }
	res, err := l.clockAdvance(ctx, json.RawMessage(`{"by":"2m"}`))
	if err != nil {
		t.Fatalf("a cut pre-settle: %v", err)
	}
	if b, _ := json.Marshal(res); contains(string(b), `"done":true`) || !l.Clock.Now().Equal(start) {
		t.Fatalf("a cut pre-settle: %s, now %v; want done:false and no movement", b, l.Clock.Now())
	}

	l.Quiesce.Register(quiesce.Func("agent", func() bool { return false }))
	l.Settle = func(c context.Context) error { _, err := l.SettleWait(c, 20*time.Millisecond); return err }
	_, err = l.clockAdvance(context.Background(), json.RawMessage(`{"by":"2m"}`))
	if err == nil || !contains(err.Error(), "agent") {
		t.Fatalf("a pre-settle that never went idle: err = %v, want it to name agent", err)
	}
	select {
	case <-t1:
		t.Fatal("the clock moved past a pre-settle that failed")
	default:
	}
	if got := settleEntries(t, l); len(got) == 0 || !contains(got[len(got)-1], `"cut":false`) {
		t.Fatalf("settle entries = %v, want the failed one journaled with cut:false", got)
	}
}

// What the pre-settle costs on an idle instance: two polls, a fraction of a millisecond.
func TestClockAdvance_PreSettleIsCheapWhenIdle(t *testing.T) {
	l := manualLab(t)
	rep, err := l.SettleWait(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("idle settle: %v wall, %d polls", rep.Wall, rep.Polls)
	if rep.Polls != 2 || rep.Wall > time.Second {
		t.Fatalf("idle settle took %d polls and %v", rep.Polls, rep.Wall)
	}
}
