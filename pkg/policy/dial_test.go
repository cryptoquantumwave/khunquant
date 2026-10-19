package policy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

var dialSettings = []string{"default", "ask", "notify", "off"}

func uniformDial(setting string) func(context.Context, Tier) (string, error) {
	return func(context.Context, Tier) (string, error) { return setting, nil }
}

func dialRig(t *testing.T, covers bool, dial func(context.Context, Tier) (string, error)) *rig {
	t.Helper()
	r := newRig(t, func(_ *rig, d *Deps) { d.Mandates = fakeMandates{covers: covers} })
	if dial != nil {
		r.e.SetOwnerDial(dial)
	}
	return r
}

func TestOwnerDial_GateC_TableAndReasons(t *testing.T) {
	trusted := func(req ActionRequest) ActionRequest { return req }
	tests := []struct {
		name    string
		req     ActionRequest
		covers  bool
		setting string
		want    Mode
		reason  string
	}{
		// T3 trusted: today ask.
		{"T3 trusted default", auto(ClassTrade, "create_order", proactive.TrustExchange), false, "default", ModeAsk, "C: dial T3, column 1"},
		{"T3 trusted ask", auto(ClassTrade, "create_order", proactive.TrustExchange), false, "ask", ModeAsk, "owner dial ask"},
		{"T3 trusted notify", auto(ClassTrade, "create_order", proactive.TrustExchange), false, "notify", ModeNotifyOnly, "C: dial T3, column 1; owner dial notify"},
		{"T3 trusted off", auto(ClassTrade, "create_order", proactive.TrustExchange), false, "off", ModeDeny, "owner dial off"},
		// T3 tainted: today notify_only. ask cannot loosen it.
		{"T3 tainted ask stays notify", other(ClassTrade, "create_order"), false, "ask", ModeNotifyOnly, "owner dial ask"},
		{"T3 tainted off", other(ClassTrade, "create_order"), false, "off", ModeDeny, "owner dial off"},
		// T2 trusted and covered: today act.
		{"T2 covered default", auto(ClassTrade, "execute_dca_order", proactive.TrustExchange), true, "default", ModeAct, "C: dial T2, column 1"},
		{"T2 covered ask", auto(ClassTrade, "execute_dca_order", proactive.TrustExchange), true, "ask", ModeAsk, "C: dial T2, column 1; owner dial ask"},
		{"T2 covered notify", auto(ClassTrade, "execute_dca_order", proactive.TrustExchange), true, "notify", ModeNotifyOnly, "owner dial notify"},
		{"T2 covered off", auto(ClassTrade, "execute_dca_order", proactive.TrustExchange), true, "off", ModeDeny, "owner dial off"},
		// T1 on an autonomous turn: today ask.
		{"T1 default", trusted(withProtective(auto(ClassTrade, "close_position", proactive.TrustExchange))), false, "default", ModeAsk, "C: dial T1, column 1"},
		{"T1 notify", trusted(withProtective(auto(ClassTrade, "close_position", proactive.TrustExchange))), false, "notify", ModeNotifyOnly, "owner dial notify"},
		{"T1 off", trusted(withProtective(auto(ClassTrade, "close_position", proactive.TrustExchange))), false, "off", ModeDeny, "owner dial off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := dialRig(t, tc.covers, uniformDial(tc.setting)).auth(tc.req)
			if v.Mode != tc.want || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], tc.reason) {
				t.Fatalf("got %s %v, want %s containing %q", v.Mode, v.Reasons, tc.want, tc.reason)
			}
			if tc.setting == "default" && strings.Contains(v.Reasons[0], "owner dial") {
				t.Errorf("the default setting is named in the reasons: %v", v.Reasons)
			}
		})
	}
}

// other is another person's message: the tainted column, with trust that passes gate A.
func other(class Class, tool string) ActionRequest {
	return ActionRequest{Tool: tool, Class: class, TurnKind: proactive.TurnUser, Actor: ActorOther, Trust: proactive.TrustExchange}
}

func withProtective(r ActionRequest) ActionRequest {
	r.ReduceOnly, r.Protective = true, true
	return r
}

// The owner's own typed order is never dialled, and neither are reads and funds.
func TestOwnerDial_OwnerTurnReadsAndFundsAreNotDialled(t *testing.T) {
	for _, setting := range dialSettings {
		r := dialRig(t, false, uniformDial(setting))
		if v := r.auth(owner(ClassTrade, "create_order")); v.Mode != ModeAct || strings.Contains(strings.Join(v.Reasons, ";"), "owner dial") {
			t.Errorf("%s: the owner's own order = %s %v", setting, v.Mode, v.Reasons)
		}
		if v := r.auth(auto(ClassRead, "get_ticker", proactive.TrustUntrusted)); v.Mode != ModeAct {
			t.Errorf("%s: a read = %s", setting, v.Mode)
		}
		if v := r.auth(auto(ClassFunds, "withdraw", proactive.TrustSystem)); v.Mode != ModeDeny || strings.Contains(strings.Join(v.Reasons, ";"), "owner dial") {
			t.Errorf("%s: funds = %s %v", setting, v.Mode, v.Reasons)
		}
	}
}

// A dial that cannot be read, or says a word it should not, is off.
func TestOwnerDial_UnreadableOrUnknownIsOff(t *testing.T) {
	bad := map[string]func(context.Context, Tier) (string, error){
		"error":   func(context.Context, Tier) (string, error) { return "", errors.New("db down") },
		"act":     uniformDial("act"),
		"garbage": uniformDial("???"),
	}
	for name, f := range bad {
		v := dialRig(t, true, f).auth(auto(ClassTrade, "execute_dca_order", proactive.TrustExchange))
		if v.Mode != ModeDeny {
			t.Errorf("%s: %s %v, want deny", name, v.Mode, v.Reasons)
		}
	}
	// The owner's own turn still works with a broken dial.
	if v := dialRig(t, false, bad["error"]).auth(owner(ClassTrade, "create_order")); v.Mode != ModeAct {
		t.Errorf("owner turn with a broken dial = %s", v.Mode)
	}
}

// The property: for every request in a generated space and every dial setting, the
// result is at least as restrictive as with no dial, and the owner's own turn gets
// exactly the same answer.
func TestOwnerDial_NeverLooserThanNoDial(t *testing.T) {
	classes := []Class{ClassUnknown, ClassRead, ClassNotify, ClassLocalState, ClassAutomation, ClassPropose, ClassTrade, ClassFunds, ClassSystem}
	turns := []struct {
		kind  proactive.TurnKind
		actor Actor
	}{
		{proactive.TurnUser, ActorOwner}, {proactive.TurnUser, ActorOther}, {proactive.TurnProactive, ActorNone},
		{proactive.TurnSubTurn, ActorNone}, {"", ActorNone},
	}
	trusts := []proactive.Trust{proactive.TrustSystem, proactive.TrustExchange, proactive.TrustUserAuthored, proactive.TrustUntrusted}
	bools := []bool{false, true}

	var checked, tightened, ownerChecked int
	for _, covers := range bools {
		base := dialRig(t, covers, nil)
		dialed := map[string]*rig{}
		for _, s := range dialSettings {
			dialed[s] = dialRig(t, covers, uniformDial(s))
		}
		dialed["error"] = dialRig(t, covers, func(context.Context, Tier) (string, error) { return "", errors.New("x") })
		for _, class := range classes {
			for _, turn := range turns {
				for _, trust := range trusts {
					for _, protective := range bools {
						for _, proposal := range bools {
							for _, killed := range bools {
								req := ActionRequest{Tool: "t", Class: class, TurnKind: turn.kind, Actor: turn.actor, Trust: trust,
									ReduceOnly: protective, Protective: protective, IsProposal: proposal}
								base.killed = killed
								want := base.auth(req)
								for name, r := range dialed {
									r.killed = killed
									got := r.auth(req)
									checked++
									if got.Mode.Rank() < want.Mode.Rank() {
										t.Fatalf("dial %s loosened %+v: %s -> %s (%v)", name, req, want.Mode, got.Mode, got.Reasons)
									}
									if got.Mode != want.Mode {
										tightened++
									}
									if columnOf(req) == colUser {
										ownerChecked++
										if got.Mode != want.Mode || got.Tier != want.Tier {
											t.Fatalf("dial %s changed the owner's own turn %+v: %s -> %s", name, req, want.Mode, got.Mode)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if checked < 5000 || tightened == 0 || ownerChecked == 0 {
		t.Fatalf("the space is too small to prove anything: checked %d, tightened %d, owner %d", checked, tightened, ownerChecked)
	}
}

// Enforce (the live hook) applies the dial too, reads it OUTSIDE the engine's lock, and
// an order the dial refuses is not counted by the rate brake.
func TestOwnerDial_Enforce(t *testing.T) {
	setting := "ask"
	var e *Engine
	locked := false
	r := newRig(t, func(rg *rig, d *Deps) {
		d.Mandates = fakeMandates{covers: true}
		rg.cfg.TradingRisk.MaxOrdersPer10Min = 1
	})
	e = r.e
	e.SetOwnerDial(func(context.Context, Tier) (string, error) {
		if !e.mu.TryLock() {
			locked = true
		} else {
			e.mu.Unlock()
		}
		return setting, nil
	})
	req := buy(auto(ClassTrade, "execute_dca_order", proactive.TrustExchange))
	if v := r.enforce(req); v.Mode != ModeAsk {
		t.Fatalf("with ask: %s %v", v.Mode, v.Reasons)
	}
	if locked {
		t.Fatal("the owner's dial was read while the engine's lock was held")
	}
	setting = "default"
	if v := r.enforce(req); v.Mode != ModeAct {
		t.Fatalf("the refused order was counted by the rate brake: %s %v", v.Mode, v.Reasons)
	}
}

func TestDialMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAct, "default": ModeAct, "ask": ModeAsk, "notify": ModeNotifyOnly, "off": ModeDeny} {
		if m, ok := DialMode(in); !ok || m != want {
			t.Errorf("DialMode(%q) = %s, %v", in, m, ok)
		}
	}
	for _, in := range []string{"act", "ASK", "allow", "notify_only"} {
		if _, ok := DialMode(in); ok {
			t.Errorf("DialMode(%q) accepted", in)
		}
	}
}
