package intents

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func TestCompile_WatchCreatesSpec(t *testing.T) {
	r := newRig(t)
	it := r.watch(titleMarker, solBelow120, 1, true)
	rep := r.reconcile()
	if rep.Created != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v", rep)
	}
	s, ok := r.spec(it.ID)
	if !ok {
		t.Fatal("no spec")
	}
	if s.Kind != triggers.KindPriceCross || s.IntentID != it.ID || s.Mode != triggers.ModeLive || s.Origin != "intent:"+it.ID {
		t.Fatalf("spec = %+v", s)
	}
	p, err := triggers.ParsePriceCross(s.Params)
	if err != nil {
		t.Fatal(err)
	}
	// A budget of one is a one-shot; the title is the spec's message (carried in the
	// Quote only); the symbol and threshold are the intent's.
	if p.Recurring || p.Message != titleMarker || p.Symbol != "SOL/USDT" || p.Threshold != 120 {
		t.Fatalf("params = %+v", p)
	}
	if got := r.comp.Note(it.ID); got != "" {
		t.Fatalf("note = %q", got)
	}
}

func TestCompile_BudgetAndCooldownMapToTheTrigger(t *testing.T) {
	r := newRig(t)
	it := r.create(intent.Intent{
		Type: intent.TypeWatch, Title: "recurring", FireBudget: 3, Cooldown: 90*time.Minute + time.Second,
		Trigger: &intent.TriggerSpec{Kind: triggers.KindPriceCross, Params: json.RawMessage(solBelow120)},
	}, true)
	r.reconcile()
	s, _ := r.spec(it.ID)
	p, _ := triggers.ParsePriceCross(s.Params)
	if !p.Recurring || p.CooldownMin != 91 {
		t.Fatalf("params = %+v, want recurring with a 91 minute cooldown", p)
	}
	un := r.watch("unlimited", solBelow120, 0, true)
	r.reconcile()
	s, _ = r.spec(un.ID)
	if p, _ := triggers.ParsePriceCross(s.Params); !p.Recurring {
		t.Fatalf("an unlimited watch is one-shot: %+v", p)
	}
}

func TestCompile_IndicatorWatch(t *testing.T) {
	r := newRig(t)
	it := r.create(intent.Intent{
		Type: intent.TypeWatch, Title: "RSI", FireBudget: 1,
		Trigger: &intent.TriggerSpec{Kind: triggers.KindIndicatorCross, Params: json.RawMessage(
			`{"provider":"okx","symbol":"BTC/USDT","timeframe":"1h","indicator":"rsi","period":14,"direction":"below","threshold":30}`)},
	}, true)
	r.reconcile()
	if s, ok := r.spec(it.ID); !ok || s.Kind != triggers.KindIndicatorCross || s.Origin != triggers.IntentOrigin(it.ID) {
		t.Fatalf("spec = %+v %v", s, ok)
	}
}

func TestCompile_ProposedCreatesNothing(t *testing.T) {
	r := newRig(t)
	r.watch("suggested", solBelow120, 1, false)
	rep := r.reconcile()
	if rep != (Report{}) {
		t.Fatalf("report = %+v", rep)
	}
	if all, _ := r.reg.List(context.Background(), triggers.ListFilter{}); len(all) != 0 {
		t.Fatalf("specs = %+v", all)
	}
}

func TestCompile_GoalAndPreferenceCompileToNothing(t *testing.T) {
	r := newRig(t)
	r.create(intent.Intent{Type: intent.TypeGoal, Title: "keep crypto under 30%"}, true)
	r.create(intent.Intent{Type: intent.TypePreference, Title: "short answers"}, true)
	r.reconcile()
	if all, _ := r.reg.List(context.Background(), triggers.ListFilter{}); len(all) != 0 {
		t.Fatalf("specs = %+v", all)
	}
	if _, err := Compile(intent.Intent{Type: intent.TypeGoal}, Env{}); err != ErrNotCompiled {
		t.Fatalf("Compile(goal) = %v", err)
	}
}

// A standing order compiles like a watch but its origin is not the owner-alert one.
func TestCompile_StandingOrderIsANoticeNotAnOwnerAlert(t *testing.T) {
	r := newRig(t)
	it := r.create(intent.Intent{
		Type: intent.TypeStandingOrder, Title: "buy the dip", FireBudget: 1,
		Trigger: &intent.TriggerSpec{Kind: triggers.KindPriceCross, Params: json.RawMessage(solBelow120)},
	}, true)
	r.reconcile()
	s, ok := r.spec(it.ID)
	if !ok || s.Origin != triggers.IntentOrderOrigin(it.ID) || triggers.OwnerAlertSpec(s.Kind, s.Origin) {
		t.Fatalf("spec = %+v %v", s, ok)
	}
}

// Each status has the spec state the design gives it.
func TestStatusToSpecState(t *testing.T) {
	for _, c := range []struct {
		name     string
		to       intent.Status
		wantSpec bool
		wantMode string
	}{
		{"active", intent.StatusActive, true, triggers.ModeLive},
		{"paused", intent.StatusPaused, true, triggers.ModeOff},
		{"cancelled", intent.StatusCancelled, false, ""},
		{"done", intent.StatusDone, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			it := r.watch("w", solBelow120, 0, true)
			r.reconcile()
			if c.to != intent.StatusActive {
				r.transition(it.ID, c.to)
			}
			r.reconcile()
			s, ok := r.spec(it.ID)
			if ok != c.wantSpec || (ok && s.Mode != c.wantMode) {
				t.Fatalf("spec = %+v present=%v, want present=%v mode=%q", s, ok, c.wantSpec, c.wantMode)
			}
		})
	}
	t.Run("expired", func(t *testing.T) {
		r := newRig(t)
		it := r.create(intent.Intent{
			Type: intent.TypeWatch, Title: "w", Lifetime: time.Hour,
			Trigger: &intent.TriggerSpec{Kind: triggers.KindPriceCross, Params: json.RawMessage(solBelow120)},
		}, true)
		r.reconcile()
		if _, ok := r.spec(it.ID); !ok {
			t.Fatal("no spec before the expiry")
		}
		r.advance(2 * time.Hour)
		r.reconcile()
		if _, ok := r.spec(it.ID); ok {
			t.Fatal("the spec of an expired intent is still there")
		}
	})
}

func tickDiff(at time.Time) worldmodel.Diff {
	return worldmodel.Diff{
		Event: events.Event{ID: "e", Kind: events.KindPerceptionMarketTick, Time: at, Trust: events.TrustPtr(proactive.TrustExchange)},
		At:    at,
	}
}

func evalTick(t *testing.T, reg *triggers.Registry, at time.Time, price float64) []proactive.Candidate {
	t.Helper()
	d := tickDiff(at)
	v := tickView{now: at, price: price, at: at}
	cs, err := reg.Evaluate(context.Background(), v, d)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	reg.Committed(context.Background(), ids)
	return cs
}

type tickView struct {
	worldmodel.View
	now   time.Time
	price float64
	at    time.Time
}

func (v tickView) Now() time.Time                           { return v.now }
func (v tickView) Fresh(at time.Time, d time.Duration) bool { return v.now.Sub(at) <= d }
func (v tickView) Price(string, string) (float64, time.Time, bool) {
	return v.price, v.at, true
}
func (v tickView) PriceTrust(string, string) (proactive.Trust, bool) {
	return proactive.TrustExchange, true
}

func TestPauseTurnsSpecOff(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", `{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":120,"fire_if_already":false}`, 0, true)
	r.reconcile()
	evalTick(t, r.reg, t0, 130) // the trigger learns it is on the far side of the line
	state := r.stateOf(it.ID)
	if state == "" {
		t.Fatal("the trigger kept no state")
	}

	r.transition(it.ID, intent.StatusPaused)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeOff {
		t.Fatalf("paused spec mode = %q", s.Mode)
	}
	if got := r.stateOf(it.ID); got != state {
		t.Fatalf("pausing changed the state: %q -> %q", state, got)
	}
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	if s, _ := r.spec(it.ID); s.Mode != triggers.ModeLive {
		t.Fatalf("resumed spec mode = %q", s.Mode)
	}
	if got := r.stateOf(it.ID); got != state {
		t.Fatalf("resuming dropped the state: %q -> %q", state, got)
	}
}

func TestConfirmOfASuggestionCompilesIt(t *testing.T) {
	r := newRig(t)
	it := r.watch("suggested", solBelow120, 1, false)
	r.reconcile()
	if _, ok := r.spec(it.ID); ok {
		t.Fatal("a suggestion has a spec")
	}
	r.transition(it.ID, intent.StatusActive)
	r.reconcile()
	if s, ok := r.spec(it.ID); !ok || s.Mode != triggers.ModeLive {
		t.Fatalf("spec = %+v %v", s, ok)
	}
}

func TestReconcile_RemovesOrphans(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, origin := range []string{triggers.IntentOrigin("ghost"), triggers.IntentOrderOrigin("ghost2")} {
		id := strings.ReplaceAll(origin, ":", "-")
		if _, err := r.reg.Add(ctx, triggers.Spec{ID: id, Kind: triggers.KindPriceCross, Mode: triggers.ModeLive, Origin: origin,
			IntentID: "ghost", Params: json.RawMessage(solBelow120)}); err != nil {
			t.Fatal(err)
		}
	}
	// Another origin is never touched, even with an intent id on it.
	if _, err := r.reg.Add(ctx, triggers.Spec{ID: "alert1", Kind: triggers.KindPriceCross, Mode: triggers.ModeLive,
		Origin: triggers.OriginSetPriceAlert, Params: json.RawMessage(solBelow120)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reg.Add(ctx, triggers.Spec{ID: "other1", Kind: triggers.KindPriceCross, Mode: triggers.ModeLive,
		Origin: "builtin", IntentID: "ghost", Params: json.RawMessage(solBelow120)}); err != nil {
		t.Fatal(err)
	}
	rep := r.reconcile()
	if rep.Removed != 2 {
		t.Fatalf("report = %+v", rep)
	}
	all, _ := r.reg.List(ctx, triggers.ListFilter{})
	if len(all) != 2 {
		t.Fatalf("specs left = %+v", all)
	}
	for _, s := range all {
		if s.ID != "alert1" && s.ID != "other1" {
			t.Fatalf("an orphan survived: %+v", s)
		}
	}
}

func TestReconcile_RepairsATamperedSpec(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", solBelow120, 1, true)
	r.reconcile()
	s, _ := r.spec(it.ID)

	// The mode is wrong.
	s.Mode = triggers.ModeShadow
	if err := r.reg.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if rep := r.reconcile(); rep.Repaired != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if got, _ := r.spec(it.ID); got.Mode != triggers.ModeLive {
		t.Fatalf("mode = %q", got.Mode)
	}

	// The parameters are wrong: the threshold was edited behind the ledger's back.
	s, _ = r.spec(it.ID)
	s.Params = json.RawMessage(`{"provider":"okx","symbol":"SOL/USDT","direction":"below","threshold":50,"message":"x"}`)
	if err := r.reg.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if rep := r.reconcile(); rep.Repaired != 1 {
		t.Fatalf("report = %+v", rep)
	}
	got, _ := r.spec(it.ID)
	if p, _ := triggers.ParsePriceCross(got.Params); p.Threshold != 120 || p.Message != "w" {
		t.Fatalf("params = %s", got.Params)
	}
}

func TestReconcile_IsIdempotent(t *testing.T) {
	r := newRig(t)
	r.watch("a", solBelow120, 1, true)
	r.reminder("b", `{"in":"3d"}`, 1, 0)
	r.transition(r.watch("c", solBelow120, 1, true).ID, intent.StatusPaused)
	first := r.reconcile()
	if first.Created != 2 || first.Failed != 0 {
		t.Fatalf("first = %+v", first)
	}
	specs := r.specsOf("")
	for i := 0; i < 3; i++ {
		rep := r.reconcile()
		if rep.Created != 0 || rep.Repaired != 0 || rep.Removed != 0 || rep.Failed != 0 {
			t.Fatalf("pass %d changed things: %+v", i+2, rep)
		}
	}
	again := r.specsOf("")
	if len(again) != len(specs) {
		t.Fatalf("%d specs, then %d", len(specs), len(again))
	}
	for i := range specs {
		if !specs[i].UpdatedAt.Equal(again[i].UpdatedAt) {
			t.Fatalf("spec %s was rewritten", specs[i].ID)
		}
	}
}

func TestReconcile_ASingleBadIntentDoesNotStopTheOthers(t *testing.T) {
	r := newRig(t)
	bad := r.create(intent.Intent{
		Type: intent.TypeWatch, Title: "bad", FireBudget: 1,
		Trigger: &intent.TriggerSpec{Kind: triggers.KindMoveVsVol, Params: json.RawMessage(`{}`)},
	}, true)
	good := r.watch("good", solBelow120, 1, true)
	rep := r.reconcile()
	if rep.Failed != 1 || rep.Created != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := r.spec(good.ID); !ok {
		t.Fatal("the good intent was not compiled")
	}
	if _, ok := r.spec(bad.ID); ok {
		t.Fatal("the bad intent got a spec")
	}
	if n := r.comp.Note(bad.ID); n != NoteRefused {
		t.Fatalf("note = %q", n)
	}
	if st := r.comp.Stats(); st.Failures != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if got := r.get(bad.ID); got.Status != intent.StatusActive {
		t.Fatalf("the ledger was changed: %s", got.Status)
	}
}

// A cap that refuses a spec leaves the intent active, says so in a code-built note and
// is retried at the next pass.
func TestCaps_RefusedSpecIsNotedAndRetried(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", solBelow120, 1, true)
	r.specs.setRefuse(true)
	rep := r.reconcile()
	if rep.Failed != 1 || rep.Created != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if n := r.comp.Note(it.ID); n != NoteCapReached {
		t.Fatalf("note = %q", n)
	}
	if got := r.get(it.ID); got.Status != intent.StatusActive {
		t.Fatalf("status = %s", got.Status)
	}
	r.specs.setRefuse(false)
	if rep := r.reconcile(); rep.Created != 1 {
		t.Fatalf("retry = %+v", rep)
	}
	if n := r.comp.Note(it.ID); n != "" {
		t.Fatalf("note after the retry = %q", n)
	}
}

func TestReconcile_AnIdTakenByAnotherOriginIsNotHijacked(t *testing.T) {
	r := newRig(t)
	it := r.watch("w", solBelow120, 1, true)
	if _, err := r.reg.Add(context.Background(), triggers.Spec{ID: it.ID, Kind: triggers.KindPriceCross, Mode: triggers.ModeLive,
		Origin: triggers.OriginSetPriceAlert, Params: json.RawMessage(solBelow120)}); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	s, _ := r.spec(it.ID)
	if s.Origin != triggers.OriginSetPriceAlert {
		t.Fatalf("spec = %+v", s)
	}
	if n := r.comp.Note(it.ID); n != NoteSpecBlocked {
		t.Fatalf("note = %q", n)
	}
}

func TestHousekeepOnlySignalsOncePerMinute(t *testing.T) {
	r := newRig(t)
	r.comp.Housekeep(context.Background(), t0)
	select {
	case <-r.comp.wake:
	default:
		t.Fatal("no request after the first call")
	}
	r.comp.Housekeep(context.Background(), t0.Add(10*time.Second))
	select {
	case <-r.comp.wake:
		t.Fatal("a second request inside the minute")
	default:
	}
	r.comp.Housekeep(context.Background(), t0.Add(61*time.Second))
	select {
	case <-r.comp.wake:
	default:
		t.Fatal("no request a minute later")
	}
}

func TestRun_StartupPassAndRequestedPass(t *testing.T) {
	r := newRig(t)
	a := r.watch("a", solBelow120, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.comp.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the startup pass", func() bool { _, ok := r.spec(a.ID); return ok })
	b := r.watch("b", solBelow120, 1, true)
	r.comp.Request()
	r.comp.Request()
	waitFor(t, "the requested pass", func() bool { _, ok := r.spec(b.ID); return ok })
	if !r.comp.Running() {
		t.Fatal("not running")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
