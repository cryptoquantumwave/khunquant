package intents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func (r *rig) setZone(name string) {
	r.t.Helper()
	p := intent.DefaultProfile()
	p.Timezone = name
	if _, err := r.store.PutProfile(context.Background(), p, "test", proactive.TrustUserAuthored); err != nil {
		r.t.Fatal(err)
	}
}

func TestReminder_AtIsReadInTheProfileZone(t *testing.T) {
	r := newRig(t)
	r.setZone("Asia/Bangkok")
	it := r.reminder(titleMarker, `{"at":"2026-10-16T09:00"}`, 1, 0)
	r.reconcile()
	s, ok := r.spec(it.ID)
	if !ok || s.Kind != triggers.KindTimeAt || s.Origin != triggers.IntentOrigin(it.ID) || s.Mode != triggers.ModeLive {
		t.Fatalf("spec = %+v %v", s, ok)
	}
	p, err := triggers.ParseTimeAt(s.Params)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC) // 09:00 in Bangkok
	if p.AtMS != want.UnixMilli() || p.Zone != "Asia/Bangkok" || p.Message != titleMarker || p.Budget != 1 {
		t.Fatalf("params = %+v, want %s", p, want)
	}
	tm, ok := r.timers.Get(it.ID)
	if !ok || !tm.At.Equal(want) || tm.Recurring() {
		t.Fatalf("timer = %+v %v", tm, ok)
	}

	// An explicit offset is taken as given, whatever the profile zone.
	other := r.reminder("x", `{"at":"2026-10-16T09:00:00+00:00"}`, 1, 0)
	r.reconcile()
	if tm, _ := r.timers.Get(other.ID); !tm.At.Equal(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("timer = %+v", tm)
	}
}

// An {in} reminder counts from the moment it became active, which is in the ledger:
// a restart (a new compiler, a later clock) does not push it out.
func TestReminder_SurvivesRestart(t *testing.T) {
	r := newRig(t)
	it := r.reminder("later", `{"in":"3d"}`, 1, 0)
	r.reconcile()
	tm, ok := r.timers.Get(it.ID)
	want := t0.Add(72 * time.Hour)
	if !ok || !tm.At.Equal(want) {
		t.Fatalf("timer = %+v %v, want %s", tm, ok, want)
	}
	r.advance(30 * time.Hour)
	r.comp = r.newCompiler() // the gateway restarted
	delete(r.timers.m, it.ID)
	r.reconcile()
	tm, ok = r.timers.Get(it.ID)
	if !ok || !tm.At.Equal(want) {
		t.Fatalf("after the restart the timer is %+v %v, want %s", tm, ok, want)
	}
	s, _ := r.spec(it.ID)
	if p, _ := triggers.ParseTimeAt(s.Params); p.AtMS != want.UnixMilli() {
		t.Fatalf("spec params = %+v", p)
	}
}

func TestReminder_ConfirmedLaterCountsFromTheConfirmation(t *testing.T) {
	r := newRig(t)
	it := r.create(intent.Intent{Type: intent.TypeReminder, Title: "r", Params: json.RawMessage(`{"in":"1d"}`), FireBudget: 1}, false)
	r.advance(5 * time.Hour)
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	tm, ok := r.timers.Get(it.ID)
	if want := t0.Add(5*time.Hour + 24*time.Hour); !ok || !tm.At.Equal(want) {
		t.Fatalf("timer = %+v, want %s", tm, want)
	}
}

func TestReminder_RecurringHasACronTimerAndHonoursBudgetAndExpiry(t *testing.T) {
	r := newRig(t)
	it := r.reminder("weekly", `{"cron":"0 9 * * 5","tz":"Asia/Bangkok"}`, 3, 30*24*time.Hour)
	r.reconcile()
	tm, ok := r.timers.Get(it.ID)
	if !ok || !tm.Recurring() || tm.Cron != "0 9 * * 5" || tm.TZ != "Asia/Bangkok" || tm.Budget != 3 {
		t.Fatalf("timer = %+v %v", tm, ok)
	}
	if want := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC); !tm.At.Equal(want) { // the next Friday 09:00 in Bangkok
		t.Fatalf("next = %s, want %s", tm.At, want)
	}
	if want := t0.Add(30 * 24 * time.Hour); !tm.Until.Equal(want) {
		t.Fatalf("until = %s, want the intent's expiry %s", tm.Until, want)
	}
	s, _ := r.spec(it.ID)
	if p, _ := triggers.ParseTimeAt(s.Params); p.Cron != "0 9 * * 5" || p.Budget != 3 || p.UntilMS != tm.Until.UnixMilli() {
		t.Fatalf("params = %+v", p)
	}
	// A pass that finds the timer in place does not move it.
	moved := tm
	moved.At = tm.At.Add(7 * 24 * time.Hour)
	_ = r.timers.Schedule(moved)
	r.reconcile()
	if got, _ := r.timers.Get(it.ID); !got.At.Equal(moved.At) {
		t.Fatalf("the compiler moved a timer that matched: %+v", got)
	}
}

func TestReminder_PauseCancelsTheTimerResumeSetsItAgainCancelRemovesBoth(t *testing.T) {
	r := newRig(t)
	it := r.reminder("r", `{"in":"3d"}`, 1, 0)
	r.reconcile()
	r.transition(it.ID, intent.StatusPaused)
	r.reconcile()
	if _, ok := r.timers.Get(it.ID); ok {
		t.Fatal("a paused reminder keeps its timer")
	}
	if s, ok := r.spec(it.ID); !ok || s.Mode != triggers.ModeOff {
		t.Fatalf("spec = %+v %v", s, ok)
	}
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	if tm, ok := r.timers.Get(it.ID); !ok || !tm.At.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("timer after the resume = %+v %v", tm, ok)
	}
	r.transition(it.ID, intent.StatusCancelled)
	r.reconcile()
	if _, ok := r.timers.Get(it.ID); ok {
		t.Fatal("a cancelled reminder keeps its timer")
	}
	if _, ok := r.spec(it.ID); ok {
		t.Fatal("a cancelled reminder keeps its spec")
	}
}

func TestReminder_ShadowSchedulesNoTimerAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.setMode(triggers.ModeShadow)
	it := r.reminder("r", `{"in":"3d"}`, 1, 0)
	r.reconcile()
	if _, ok := r.timers.Get(it.ID); ok {
		t.Fatal("a timer was set although triggers are in shadow")
	}
	if n := r.comp.Note(it.ID); n != NoteShadow {
		t.Fatalf("note = %q", n)
	}
	r.setMode(triggers.ModeLive)
	r.reconcile()
	if _, ok := r.timers.Get(it.ID); !ok {
		t.Fatal("no timer once triggers are live")
	}
}

func TestReminder_WithoutAClockSaysSoAndAFailedScheduleIsRetried(t *testing.T) {
	r := newRig(t)
	r.comp.SetTimers(nil)
	it := r.reminder("r", `{"in":"3d"}`, 1, 0)
	r.reconcile()
	if n := r.comp.Note(it.ID); n != NoteNoClock {
		t.Fatalf("note = %q", n)
	}
	r.comp.SetTimers(r.timers)
	r.timers.failOn = it.ID
	r.reconcile()
	if n := r.comp.Note(it.ID); n != NoteFailed {
		t.Fatalf("note = %q", n)
	}
	r.timers.failOn = ""
	r.reconcile()
	if _, ok := r.timers.Get(it.ID); !ok || r.comp.Note(it.ID) != "" {
		t.Fatalf("not retried: note %q", r.comp.Note(it.ID))
	}
}

// A timer that no spec needs is cancelled (an intent that ended while the gateway ran
// without the compiler).
func TestReminder_StrayTimersAreCancelled(t *testing.T) {
	r := newRig(t)
	_ = r.timers.Schedule(perception.Timer{ID: "stray", At: t0.Add(time.Hour)})
	r.reconcile()
	if _, ok := r.timers.Get("stray"); ok {
		t.Fatal("a timer without an intent survived")
	}
}

func TestCompileReminder_Refusals(t *testing.T) {
	env := Env{Now: t0, Loc: DefaultLocation(), ActivatedAt: t0}
	for name, params := range map[string]string{
		"nothing":        `{}`,
		"two ways":       `{"at":"2026-10-16T09:00","in":"3d"}`,
		"bad at":         `{"at":"tomorrow"}`,
		"bad in":         `{"in":"soon"}`,
		"in too long":    `{"in":"9999d"}`,
		"tz alone":       `{"in":"3d","tz":"Asia/Bangkok"}`,
		"bad tz":         `{"cron":"0 9 * * *","tz":"Mars/Base"}`,
		"bad cron":       `{"cron":"every friday","tz":"Asia/Bangkok"}`,
		"never":          `{"cron":"0 0 30 2 *","tz":"Asia/Bangkok"}`,
		"text not text":  `{"at":5}`,
		"six field tick": `{"cron":"*/10 * * * * *","tz":"Asia/Bangkok"}`,
	} {
		_, err := Compile(intent.Intent{Type: intent.TypeReminder, Title: "r", Params: json.RawMessage(params), FireBudget: 1}, env)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Once a minute is the floor, and is allowed.
	if _, err := Compile(intent.Intent{Type: intent.TypeReminder, Title: "r", Params: json.RawMessage(`{"cron":"* * * * *","tz":"UTC"}`), FireBudget: 1}, env); err != nil {
		t.Errorf("every minute refused: %v", err)
	}
}

func TestNextOccurrence_AcrossADSTChange(t *testing.T) {
	// New York's clocks go forward on 2026-03-08: 09:00 local is 14:00Z before and 13:00Z after.
	tm := perception.Timer{Cron: "0 9 * * *", TZ: "America/New_York"}
	first, ok := NextOccurrence(tm, time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC))
	if !ok || !first.Equal(time.Date(2026, 3, 7, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("first = %s %v", first, ok)
	}
	second, ok := NextOccurrence(tm, first)
	if !ok || !second.Equal(time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("second = %s %v, want 13:00Z (09:00 EDT)", second, ok)
	}
}

// ---- validation --------------------------------------------------------------

func TestValidate(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	watch := func(kind, params string) intent.Intent {
		return intent.Intent{Type: intent.TypeWatch, Title: "w", FireBudget: 1, Trigger: &intent.TriggerSpec{Kind: kind, Params: json.RawMessage(params)}}
	}
	rem := func(params string) intent.Intent {
		return intent.Intent{Type: intent.TypeReminder, Title: "r", FireBudget: 1, Params: json.RawMessage(params)}
	}
	for name, c := range map[string]struct {
		it   intent.Intent
		want string // "" = valid; else a fragment of the message
	}{
		"good watch":             {watch(triggers.KindPriceCross, solBelow120), ""},
		"unsupported kind":       {watch(triggers.KindMoveVsVol, `{}`), "price_cross or an indicator_cross"},
		"no trigger":             {intent.Intent{Type: intent.TypeWatch, Title: "w"}, "needs a trigger"},
		"negative threshold":     {watch(triggers.KindPriceCross, `{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":-1}`), "threshold"},
		"bad direction":          {watch(triggers.KindPriceCross, `{"provider":"okx","symbol":"SOL/USDT","direction":"under","threshold":120}`), "direction"},
		"unknown param":          {watch(triggers.KindPriceCross, `{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":120,"zzz":1}`), "not valid"},
		"empty params":           {watch(triggers.KindPriceCross, `{}`), "provider"},
		"cooldown too long":      {intent.Intent{Type: intent.TypeWatch, Title: "w", Cooldown: 10 * 24 * time.Hour, Trigger: &intent.TriggerSpec{Kind: triggers.KindPriceCross, Params: json.RawMessage(solBelow120)}}, "cooldown"},
		"good reminder":          {rem(`{"in":"3d"}`), ""},
		"reminder in the past":   {rem(`{"at":"2020-01-01T09:00"}`), "already passed"},
		"reminder nothing":       {rem(`{}`), "exactly one"},
		"goal needs nothing":     {intent.Intent{Type: intent.TypeGoal, Title: "g"}, ""},
		"preference needs none":  {intent.Intent{Type: intent.TypePreference, Title: "p"}, ""},
		"order wants a good one": {intent.Intent{Type: intent.TypeStandingOrder, Title: "o", Trigger: &intent.TriggerSpec{Kind: "rsi", Params: json.RawMessage(`{}`)}}, "price_cross"},
	} {
		err := Validate(ctx, r.store, c.it, t0)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}
