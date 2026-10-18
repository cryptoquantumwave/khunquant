package reflection

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// propose stores a pending reflection proposal and returns its code.
func (r *rig) propose(req approval.ReflectionRequest) string {
	r.t.Helper()
	p, err := r.approval.ProposeReflection(context.Background(), req)
	if err != nil {
		r.t.Fatal(err)
	}
	return p.Code
}

func floorChange(from, to float64) approval.ReflectionRequest {
	return approval.ReflectionRequest{
		Trigger:   &approval.TriggerParamParams{SpecID: "pf-move_vs_vol", Kind: "move_vs_vol", Param: "floor_th_pct", From: from, To: to},
		Rationale: "You dismissed 4 of 4 SET move alerts.",
	}
}

func TestReflection_ApprovedThresholdApplied(t *testing.T) {
	r := newRig(t)
	code := r.propose(floorChange(3, 5))
	if got := r.value("pf-move_vs_vol", "floor_th_pct"); got != 3 {
		t.Fatalf("floor_th_pct = %g before approval", got)
	}
	before := r.spec("pf-move_vs_vol")

	p := r.decide(code, true)
	if p.Status != approval.StatusExecuted {
		t.Fatalf("status = %s (%s)", p.Status, p.Outcome)
	}
	after := r.spec("pf-move_vs_vol")
	if got := r.value("pf-move_vs_vol", "floor_th_pct"); got != 5 {
		t.Errorf("floor_th_pct = %g after approval, want 5", got)
	}
	// Only that parameter moved; the rest of the kind's parameters are as they were.
	var b, a map[string]any
	_ = json.Unmarshal(before.Params, &b)
	_ = json.Unmarshal(after.Params, &a)
	for k, v := range b {
		if k != "floor_th_pct" && a[k] != v {
			t.Errorf("%s changed from %v to %v", k, v, a[k])
		}
	}
	if !strings.HasSuffix(after.Origin, ProvenanceSuffix) || !strings.HasPrefix(after.Origin, triggers.PortfolioSpecOrigin) {
		t.Errorf("origin = %q: provenance of the change is not kept", after.Origin)
	}
	// Applying it again changes nothing (Recover can call the sink twice).
	again := r.mustGet(p.ID)
	again.Status = approval.StatusExecuting
	if err := (&Applier{Specs: r.reg, Intents: r.intents, Mute: r.muter}).ApplyApprovedReflection(context.Background(), again); err != nil {
		t.Errorf("a second application = %v", err)
	}
}

// A value that moved between the proposal and the approval is not overwritten.
func TestReflection_StaleChangeIsRefusedNotApplied(t *testing.T) {
	r := newRig(t)
	code := r.propose(floorChange(3, 5))
	s := r.spec("pf-move_vs_vol")
	params, err := triggers.WithParam(s.Kind, s.Params, "floor_th_pct", 4)
	if err != nil {
		t.Fatal(err)
	}
	s.Params = params
	if err := r.reg.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if p := r.decide(code, true); p.Status != approval.StatusFailed {
		t.Fatalf("status = %s, want failed", p.Status)
	}
	if got := r.value("pf-move_vs_vol", "floor_th_pct"); got != 4 {
		t.Errorf("floor_th_pct = %g: the owner's own value was overwritten", got)
	}
}

// The apply-time check: a proposal stored against an owner's alert (however it got there)
// is refused when approved, and the alert's level does not move.
func TestApplier_RefusesAnOwnerAlertAtApplyTime(t *testing.T) {
	r := newRig(t)
	for name, o := range ownerOrigins {
		s := r.addSpec("alert-"+strings.ReplaceAll(name, " ", "-"), triggers.KindPriceCross, o.origin, o.intentID, btcAbove100k.JSON())
		code := r.propose(approval.ReflectionRequest{Trigger: &approval.TriggerParamParams{
			SpecID: s.ID, Kind: s.Kind, Param: "threshold", From: 100000, To: 110000}})
		if p := r.decide(code, true); p.Status != approval.StatusFailed {
			t.Errorf("%s: status = %s, want failed", name, p.Status)
		}
		if got := r.spec(s.ID); string(got.Params) != string(s.Params) {
			t.Errorf("%s: the owner's alert changed: %s", name, got.Params)
		}
	}
	// ... and a spec that is not one is changed.
	s := r.addSpec("plain", triggers.KindPriceCross, "builtin:test", "", btcAbove100k.JSON())
	code := r.propose(approval.ReflectionRequest{Trigger: &approval.TriggerParamParams{SpecID: s.ID, Kind: s.Kind, Param: "threshold", From: 100000, To: 110000}})
	if p := r.decide(code, true); p.Status != approval.StatusExecuted {
		t.Fatalf("a plain spec: status = %s (%s)", p.Status, p.Outcome)
	}
	if got := triggers.Bounds(s.Kind, r.spec(s.ID).Params)["threshold"].Value; got != 110000 {
		t.Errorf("threshold = %g", got)
	}
	if got := r.spec(s.ID).Origin; got != "builtin:test"+ProvenanceSuffix {
		t.Errorf("origin = %q", got)
	}
}

func TestApplier_MutesAWholeKindOnly(t *testing.T) {
	r := newRig(t)
	code := r.propose(approval.ReflectionRequest{Mute: &approval.MuteParams{Prefix: "move_vs_vol:", Days: 14}})
	if p := r.decide(code, true); p.Status != approval.StatusExecuted {
		t.Fatalf("status = %s (%s)", p.Status, p.Outcome)
	}
	if r.muter.count() != 1 || r.muter.calls[0] != "move_vs_vol:" {
		t.Fatalf("mutes = %v", r.muter.calls)
	}
	if d := r.muter.until[0].Sub(r.clock.Now()); d != 14*24*time.Hour {
		t.Errorf("the mute lasts %s, want 14 days", d)
	}
	// A prefix that reaches one alert is refused when approved.
	code = r.propose(approval.ReflectionRequest{Mute: &approval.MuteParams{Prefix: "price_cross:my-alert", Days: 7}})
	if p := r.decide(code, true); p.Status != approval.StatusFailed || r.muter.count() != 1 {
		t.Errorf("status = %s, %d mutes", p.Status, r.muter.count())
	}
}

func TestApplier_IntentCleanupOnlyPausesOrCancels(t *testing.T) {
	r := newRig(t)
	r.intents.SetClock(r.clock.Now)
	mk := func(title string) intent.Intent {
		it, err := r.intents.CreateIntent(context.Background(), intent.Intent{
			Type: intent.TypeWatch, Title: title, Status: intent.StatusActive, Source: intent.SourceExplicit,
			Provenance: intent.Provenance{SenderID: ownerID, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser},
			Trigger:    &intent.TriggerSpec{Kind: "price_cross", Params: json.RawMessage(`{"symbol":"SOL/USDT","below":120}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	pause, cancel := mk("Watch SOL below 120"), mk("Watch ETH below 2000")
	for _, c := range []struct {
		it     intent.Intent
		action string
		want   intent.Status
	}{{pause, approval.CleanupPause, intent.StatusPaused}, {cancel, approval.CleanupCancel, intent.StatusCancelled}} {
		code := r.propose(approval.ReflectionRequest{Intent: &approval.IntentCleanupParams{IntentID: c.it.ID, Action: c.action, Title: c.it.Title}})
		if got, _ := r.intents.GetIntent(context.Background(), c.it.ID); got.Status != intent.StatusActive {
			t.Fatalf("%s: the intent is %s before approval", c.action, got.Status)
		}
		if p := r.decide(code, true); p.Status != approval.StatusExecuted {
			t.Fatalf("%s: status = %s (%s)", c.action, p.Status, p.Outcome)
		}
		if got, _ := r.intents.GetIntent(context.Background(), c.it.ID); got.Status != c.want {
			t.Errorf("%s: the intent is %s, want %s", c.action, got.Status, c.want)
		}
	}
	// A cancelled intent cannot be paused by a late approval: nothing is revived.
	code := r.propose(approval.ReflectionRequest{Intent: &approval.IntentCleanupParams{IntentID: cancel.ID, Action: approval.CleanupPause, Title: cancel.Title}})
	if p := r.decide(code, true); p.Status != approval.StatusFailed {
		t.Errorf("pausing a cancelled intent: status = %s", p.Status)
	}
	if got, _ := r.intents.GetIntent(context.Background(), cancel.ID); got.Status != intent.StatusCancelled {
		t.Errorf("the intent is %s", got.Status)
	}
}
