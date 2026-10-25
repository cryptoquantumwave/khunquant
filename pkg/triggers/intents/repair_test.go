package intents

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// errTrigger fails on every tick.
type errTrigger struct{ id string }

func (e errTrigger) ID() string                 { return e.id }
func (errTrigger) Kinds() []events.Kind         { return []events.Kind{events.KindPerceptionMarketTick} }
func (errTrigger) Demands() []perception.Demand { return nil }
func (errTrigger) Evaluate(context.Context, worldmodel.View, worldmodel.Diff, *triggers.State) ([]proactive.Candidate, error) {
	return nil, errors.New("boom")
}

func failingPriceCross() map[string]triggers.Factory {
	return map[string]triggers.Factory{triggers.KindPriceCross: func(spec triggers.Spec) (triggers.Trigger, error) {
		return errTrigger{id: spec.Kind + ":" + spec.ID}, nil
	}}
}

// ticks evaluates n ticks and returns how many auto-disabled notices came back.
func (r *rig) ticks(n int) (notices int) {
	r.t.Helper()
	for i := 0; i < n; i++ {
		for _, c := range evalTick(r.t, r.reg, r.clock(), 130) {
			if strings.HasSuffix(c.DedupKey, ":auto_disabled") {
				notices++
			}
		}
	}
	return notices
}

// A spec the registry stopped after repeated errors stays off across reconciles, so
// the owner gets one notice and not one every few minutes; it comes back when the
// owner acts.
func TestAutoDisabled_StaysOffUntilTheOwnerActs(t *testing.T) {
	r := newRigWith(t, failingPriceCross())
	it := r.watch("w", solBelow120, 0, true)
	r.reconcile()

	if n := r.ticks(triggers.MaxConsecutiveErrors); n != 1 {
		t.Fatalf("%d notices from the first ten failures, want 1", n)
	}
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeOff || !r.reg.AutoDisabled(it.ID) {
		t.Fatalf("spec = %+v, auto-disabled = %v", s, r.reg.AutoDisabled(it.ID))
	}

	notices := 0
	for i := 0; i < 5; i++ {
		r.advance(time.Minute)
		rep := r.reconcile()
		if rep.Repaired != 0 {
			t.Fatalf("pass %d switched the spec back on: %+v", i+1, rep)
		}
		if s, _ := r.spec(it.ID); s.Mode != triggers.ModeOff {
			t.Fatalf("pass %d: mode = %q", i+1, s.Mode)
		}
		if got := r.comp.Note(it.ID); got != NoteStopped {
			t.Fatalf("note = %q", got)
		}
		notices += r.ticks(triggers.MaxConsecutiveErrors)
	}
	if notices != 0 {
		t.Fatalf("%d more notices after the first", notices)
	}
	if got := r.get(it.ID); got.Status != intent.StatusActive {
		t.Fatalf("status = %s: the ledger must not change", got.Status)
	}

	// A restart does not forget it either: the count is the registry's, kept with the row.
	r.reg = r.restartRegistry()
	r.specs.Specs = r.reg
	if !r.reg.AutoDisabled(it.ID) {
		t.Fatal("a restart forgot that the spec was stopped")
	}
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeOff {
		t.Fatalf("after the restart: mode = %q", s.Mode)
	}

	// The owner pauses and resumes it: it is on again, and fails (and notifies) afresh.
	r.advance(time.Second)
	r.transition(it.ID, intent.StatusPaused)
	r.reconcile()
	r.advance(time.Second)
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeLive || r.reg.AutoDisabled(it.ID) {
		t.Fatalf("after the resume: %+v, auto-disabled = %v", s, r.reg.AutoDisabled(it.ID))
	}
	if got := r.comp.Note(it.ID); got != "" {
		t.Fatalf("note after the resume = %q", got)
	}
	if n := r.ticks(triggers.MaxConsecutiveErrors); n != 1 {
		t.Fatalf("%d notices after the resume, want 1", n)
	}
}

// The owner pausing and resuming between two passes (so no pass sees the pause) still
// counts as acting.
func TestAutoDisabled_APauseAndResumeBetweenPassesIsSeen(t *testing.T) {
	r := newRigWith(t, failingPriceCross())
	it := r.watch("w", solBelow120, 0, true)
	r.reconcile()
	r.ticks(triggers.MaxConsecutiveErrors)
	r.advance(time.Second)
	r.transition(it.ID, intent.StatusPaused)
	r.advance(time.Second)
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeLive {
		t.Fatalf("mode = %q", s.Mode)
	}
}

// A spec that is off for another reason (a one-shot that fired) is not "stopped".
func TestAutoDisabled_OnlyTheRegistrysOwnStop(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 3, true)
	r.reconcile()
	if r.reg.AutoDisabled(it.ID) || r.reg.AutoDisabled("nobody") {
		t.Fatal("a healthy or missing spec is reported as stopped")
	}
}

// A firing whose count was lost (the queue died with the process) is repaired once.
func TestRepair_ALostFiringOfAOneShotWatchIsCountedOnce(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", armedWatch, 1, true)
	r.reconcile()
	// Committed, and no observer: the queue is gone before RecordFire.
	evalTick(t, r.reg, r.clock(), 130)
	r.advance(time.Minute)
	if cs := evalTick(t, r.reg, r.clock(), 110); len(cs) != 1 {
		t.Fatalf("%d candidates", len(cs))
	}
	if got := r.get(it.ID); got.Status != intent.StatusActive || got.FiresUsed != 0 {
		t.Fatalf("before the repair: %s, used %d", got.Status, got.FiresUsed)
	}
	if !r.reg.OneShotFired(context.Background(), it.ID) {
		t.Fatal("the committed state does not say it fired")
	}

	r.reconcile()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("after the repair: %s, used %d", got.Status, got.FiresUsed)
	}
	r.reconcile()
	r.reconcile()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("after more passes: %s, used %d", got.Status, got.FiresUsed)
	}
	// The queued firing arrives late, as it would after a slow start: still one.
	r.comp.NoteFire(triggers.Firing{SpecID: it.ID, Kind: triggers.KindPriceCross, IntentID: it.ID, DedupKey: "price_cross:x:y"})
	r.process()
	if got := r.get(it.ID); got.FiresUsed != 1 {
		t.Fatalf("a late firing counted twice: %d", got.FiresUsed)
	}
	if st := r.comp.Stats(); st.FireRepaired != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if _, ok := r.spec(it.ID); ok {
		// the spec goes at the pass after the intent is done
		r.reconcile()
		if _, ok := r.spec(it.ID); ok {
			t.Fatal("the spec of the repaired intent is still there")
		}
	}
}

// What a rehearsal learned is not a firing: nothing is counted in shadow, and the
// move to live starts the trigger afresh, so the watch can still fire for real.
func TestRepair_ARehearsalIsNotAFiring(t *testing.T) {
	r := newRig(t)
	r.setMode(triggers.ModeShadow)
	it := r.watch("w", armedWatch, 1, true)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeShadow {
		t.Fatalf("mode = %q: a shadow registry gets a shadow spec", s.Mode)
	}
	evalTick(t, r.reg, r.clock(), 130)
	r.advance(time.Minute)
	evalTick(t, r.reg, r.clock(), 110) // the rehearsal "fires" and is done
	r.reconcile()
	if got := r.get(it.ID); got.Status != intent.StatusActive || got.FiresUsed != 0 {
		t.Fatalf("a rehearsal was counted: %s, used %d", got.Status, got.FiresUsed)
	}

	r.setMode(triggers.ModeLive)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeLive {
		t.Fatalf("mode = %q", s.Mode)
	}
	if got := r.get(it.ID); got.Status != intent.StatusActive {
		t.Fatalf("status = %s", got.Status)
	}
	r.advance(time.Minute)
	evalTick(t, r.reg, r.clock(), 130)
	r.advance(time.Minute)
	if cs := evalTick(t, r.reg, r.clock(), 110); len(cs) != 1 || cs[0].Shadow {
		t.Fatalf("the watch did not fire for real after the rehearsal: %+v", cs)
	}
}

type timerPayload perception.TimerDue

func (p timerPayload) TimerDue() perception.TimerDue { return perception.TimerDue(p) }

// fireTimer delivers the timer event of a reminder to the registry and commits it, with
// no observer.
func (r *rig) fireTimer(id string, due time.Time) {
	r.t.Helper()
	d := worldmodel.Diff{
		Event:   events.Event{ID: "t", Kind: events.KindPerceptionClockTimer, Trust: events.TrustPtr(proactive.TrustSystem)},
		At:      due,
		Payload: timerPayload{ID: id, Due: due},
	}
	cs, err := r.reg.Evaluate(context.Background(), tickView{now: due, at: due}, d)
	if err != nil || len(cs) != 1 {
		r.t.Fatalf("timer event: %d candidates, %v", len(cs), err)
	}
	ids := []string{cs[0].ID}
	r.reg.Committed(context.Background(), ids)
}

func TestRepair_ALostReminderFiringEndsTheReminder(t *testing.T) {
	r := newRig(t)
	one := r.reminder("one", `{"in":"1d"}`, 5, 0)
	r.reconcile()
	r.advance(25 * time.Hour)
	r.fireTimer(one.ID, t0.Add(24*time.Hour))
	delete(r.timers.m, one.ID) // the clock consumed it

	r.reconcile()
	if got := r.get(one.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("after the repair: %s, used %d", got.Status, got.FiresUsed)
	}
	r.reconcile()
	if got := r.get(one.ID); got.FiresUsed != 1 {
		t.Fatalf("counted twice: %d", got.FiresUsed)
	}
}

// The count was written but the move to done was lost: the reminder is ended, and not
// counted a second time.
func TestRepair_ACountedReminderThatIsStillActiveIsEndedNotRecounted(t *testing.T) {
	r := newRig(t)
	it := r.reminder("one", `{"in":"1d"}`, 5, 0)
	r.reconcile()
	r.advance(25 * time.Hour)
	r.fireTimer(it.ID, t0.Add(24*time.Hour))
	if _, err := r.store.RecordFire(context.Background(), it.ID); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	if got := r.get(it.ID); got.Status != intent.StatusDone || got.FiresUsed != 1 {
		t.Fatalf("%s, used %d", got.Status, got.FiresUsed)
	}
}

func TestRepair_AReminderWhoseMomentIsLongGoneIsEndedAsMissed(t *testing.T) {
	r := newRig(t)
	it := r.reminder("one", `{"in":"1d"}`, 1, 0)
	r.reconcile()
	r.advance(24*time.Hour + MisfireGrace + time.Hour)

	// The clock still holds the timer: it will fire or report it missed itself.
	r.reconcile()
	if got := r.get(it.ID); got.Status != intent.StatusActive {
		t.Fatalf("status = %s while the timer is still there", got.Status)
	}
	// The timer is gone and the moment is beyond the grace.
	_ = r.timers.Cancel(it.ID)
	r.reconcile()
	got := r.get(it.ID)
	if got.Status != intent.StatusDone || got.FiresUsed != 0 {
		t.Fatalf("%s, used %d", got.Status, got.FiresUsed)
	}
	evs, _ := r.store.Events(context.Background(), it.ID)
	if last := evs[len(evs)-1]; !strings.HasPrefix(last.Reason, "missed: was due ") || last.By != "system:compiler" {
		t.Fatalf("last event = %+v", last)
	}
}
