package intents

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const armedWatch = `{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":120,"fire_if_already":false}`

// fireWatch drives one crossing of the intent's line through the real registry, with
// the compiler as the fire observer, and returns the candidates.
func (r *rig) fireWatch(commit bool) int {
	r.t.Helper()
	r.reg.SetFireObserver(r.comp.NoteFire)
	r.advance(2 * time.Minute)
	evalTick(r.t, r.reg, r.clock(), 130)
	r.advance(2 * time.Minute)
	if !commit {
		d := tickDiff(r.clock())
		cs, err := r.reg.Evaluate(context.Background(), tickView{now: r.clock(), price: 110, at: r.clock()}, d)
		if err != nil {
			r.t.Fatal(err)
		}
		return len(cs)
	}
	return len(evalTick(r.t, r.reg, r.clock(), 110))
}

func (r *rig) process() { r.comp.processFires(context.Background()) }

func TestFire_IsCountedWhenAttentionRecordedItsDecision(t *testing.T) {
	r := newRig(t)
	it := r.watch(titleMarker, armedWatch, 3, true)
	r.reconcile()

	// Evaluated, the decision not recorded: the budget is untouched.
	if n := r.fireWatch(false); n != 1 {
		t.Fatalf("%d candidates", n)
	}
	r.process()
	if got := r.get(it.ID); got.FiresUsed != 0 {
		t.Fatalf("an undecided evaluation spent a fire: %d", got.FiresUsed)
	}

	// The decision recorded: one fire, counted off the pipeline's path.
	evalTick(t, r.reg, r.clock().Add(time.Second), 110)
	if q := len(r.comp.queue); q != 1 {
		t.Fatalf("%d firings queued", q)
	}
	r.process()
	got := r.get(it.ID)
	if got.FiresUsed != 1 || got.Status != intent.StatusActive {
		t.Fatalf("after the commit: used %d, status %s", got.FiresUsed, got.Status)
	}
	if st := r.comp.Stats(); st.Fires != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestFire_OneShotBecomesDoneAndItsSpecGoesAtTheNextPass(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 1, true)
	r.reconcile()
	if n := r.fireWatch(true); n != 1 {
		t.Fatalf("%d candidates", n)
	}
	r.process()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("status %s, used %d", got.Status, got.FiresUsed)
	}
	// The trigger turned itself off at the commit; the pass removes the spec.
	r.reconcile()
	if _, ok := r.spec(it.ID); ok {
		t.Fatal("the spec of a done intent is still there")
	}
}

func TestFire_ShadowDoesNotCount(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 1, true)
	r.reconcile()
	r.setMode(triggers.ModeShadow)
	if n := r.fireWatch(true); n != 1 {
		t.Fatalf("%d candidates", n)
	}
	r.process()
	if got := r.get(it.ID); got.FiresUsed != 0 || got.Status != intent.StatusActive {
		t.Fatalf("a rehearsal spent a fire: used %d, status %s", got.FiresUsed, got.Status)
	}
}

func TestFire_RepeatsAfterTheIntentEndedAreBenign(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 1, true)
	r.transition(it.ID, intent.StatusCancelled)
	r.comp.NoteFire(triggers.Firing{SpecID: it.ID, Kind: triggers.KindPriceCross, IntentID: it.ID, DedupKey: "price_cross:x:y"})
	r.comp.NoteFire(triggers.Firing{SpecID: "gone", Kind: triggers.KindPriceCross, IntentID: "gone", DedupKey: "price_cross:x:y"})
	r.process()
	if st := r.comp.Stats(); st.FireBenign != 2 || st.Fires != 0 || st.FireRetries != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// A firing that cannot be written now is kept and written later, not dropped.
func TestFire_ATransientFailureIsRetried(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 3, true)
	r.comp.NoteFire(triggers.Firing{SpecID: it.ID, Kind: triggers.KindPriceCross, IntentID: it.ID, DedupKey: "price_cross:x:y"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.comp.processFires(ctx)
	if got := r.get(it.ID); got.FiresUsed != 0 {
		t.Fatalf("used %d under a cancelled context", got.FiresUsed)
	}
	if len(r.comp.retry) != 1 {
		t.Fatalf("%d firings kept for a retry", len(r.comp.retry))
	}
	r.process()
	if got := r.get(it.ID); got.FiresUsed != 1 || len(r.comp.retry) != 0 {
		t.Fatalf("after the retry: used %d, %d still waiting", got.FiresUsed, len(r.comp.retry))
	}
}

// A firing committed while the gateway shuts down is counted: Run writes what is
// queued as it ends, with a context of its own.
func TestFire_QueuedFiringsAreWrittenWhenRunEnds(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 3, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.comp.Run(ctx) }()
	waitFor(t, "the compiler to run", r.comp.Running)
	cancel()
	<-done
	// Committed after Run was told to stop, before it returned: simulate by queuing and
	// running the exit path once more.
	r.comp.NoteFire(triggers.Firing{SpecID: it.ID, Kind: triggers.KindPriceCross, IntentID: it.ID, DedupKey: "price_cross:x:y"})
	fctx, stop := context.WithCancel(context.Background())
	stop()
	r.comp.processFires(fctx) // under a cancelled context nothing is lost
	if got := r.get(it.ID); got.FiresUsed != 0 || len(r.comp.retry) != 1 {
		t.Fatalf("used %d, %d kept", got.FiresUsed, len(r.comp.retry))
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); r.comp.Run(ctx2) }()
	cancel2()
	<-done2
	if got := r.get(it.ID); got.FiresUsed != 1 {
		t.Fatalf("a firing queued when Run ended was not counted: used %d", got.FiresUsed)
	}
}

func TestFire_TheQueueIsBoundedAndNeverBlocks(t *testing.T) {
	r := newRig(t)
	for i := 0; i < maxQueued+50; i++ {
		r.comp.NoteFire(triggers.Firing{IntentID: "x", Kind: triggers.KindPriceCross})
	}
	if st := r.comp.Stats(); st.FireDropped != 50 || len(r.comp.queue) != maxQueued {
		t.Fatalf("dropped %d, queued %d", st.FireDropped, len(r.comp.queue))
	}
	r.comp.NoteFire(triggers.Firing{}) // no intent: ignored
}

func timeAtFiring(it intent.Intent, key string) triggers.Firing {
	return triggers.Firing{SpecID: it.ID, Kind: triggers.KindTimeAt, IntentID: it.ID, DedupKey: key}
}

// A one-shot reminder is done once it has been delivered, whatever budget was asked.
func TestFire_OneShotReminderIsDoneAfterItsFiring(t *testing.T) {
	r := newRig(t)
	it := r.reminder("r", `{"in":"1d"}`, 5, 0)
	r.comp.NoteFire(timeAtFiring(it, triggers.TimeAtKey(it.ID, t0.UnixMilli())))
	r.process()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("status %s, used %d", got.Status, got.FiresUsed)
	}
}

func TestFire_RecurringReminderGoesOnUntilItsBudgetIsSpent(t *testing.T) {
	r := newRig(t)
	it := r.reminder("r", `{"cron":"0 9 * * 5","tz":"Asia/Bangkok"}`, 2, 0)
	f := timeAtFiring(it, triggers.TimeAtKey(it.ID, t0.UnixMilli()))
	r.comp.NoteFire(f)
	r.process()
	if got := r.get(it.ID); got.Status != intent.StatusActive || got.FiresUsed != 1 {
		t.Fatalf("status %s, used %d", got.Status, got.FiresUsed)
	}
	r.comp.NoteFire(f)
	r.process()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 2 {
		t.Fatalf("status %s, used %d", got.Status, got.FiresUsed)
	}
}

// A one-shot reminder whose moment passed while nothing ran is done, with a reason
// the owner can read; a recurring one is not ended, and neither counts as a fire.
func TestFire_MissedReminder(t *testing.T) {
	r := newRig(t)
	one := r.reminder("one", `{"in":"1d"}`, 1, 0)
	rec := r.reminder("rec", `{"cron":"0 9 * * 5","tz":"Asia/Bangkok"}`, 0, 0)
	due := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC)
	r.comp.NoteFire(timeAtFiring(one, triggers.TimeAtMissedKey(one.ID, due.UnixMilli())))
	r.comp.NoteFire(timeAtFiring(rec, triggers.TimeAtMissedKey(rec.ID, due.UnixMilli())))
	r.process()

	got := r.get(one.ID)
	if got.Status != intent.StatusDone || got.FiresUsed != 0 {
		t.Fatalf("one-shot: status %s, used %d", got.Status, got.FiresUsed)
	}
	evs, err := r.store.Events(context.Background(), one.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.ToStatus != intent.StatusDone || last.By != "system:compiler" ||
		last.Reason != "missed: was due 2026-10-16T02:00:00Z while the assistant was not running" {
		t.Fatalf("last event = %+v", last)
	}
	if got := r.get(rec.ID); got.Status != intent.StatusActive || got.FiresUsed != 0 {
		t.Fatalf("recurring: status %s, used %d", got.Status, got.FiresUsed)
	}
	if st := r.comp.Stats(); st.Missed != 2 {
		t.Fatalf("stats = %+v", st)
	}
}
