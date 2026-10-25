package policy

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func (r *rig) enforce(req ActionRequest) Verdict { return r.e.Enforce(context.Background(), req) }

// Enforce counts a placing order itself, in the same critical section that checked
// the brakes: with a rate limit of 1, of N concurrent distinct orders exactly one
// passes, and with the duplicate guard exactly one of N identical orders does.
func TestEnforce_BrakesAreAtomicUnderConcurrency(t *testing.T) {
	const n = 32
	run := func(name string, limit int, mk func(i int) ActionRequest, wantPass int) {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			r.cfg.TradingRisk.MaxOrdersPer10Min = limit
			var passed atomic.Int32
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					if v := r.enforce(mk(i)); v.Mode == ModeAct {
						passed.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			if got := int(passed.Load()); got != wantPass {
				t.Fatalf("%d of %d concurrent orders passed, want %d", got, n, wantPass)
			}
		})
	}
	run("rate throttle, distinct orders", 1, func(i int) ActionRequest {
		q := buy(owner(ClassTrade, "create_order"))
		q.Order.Amount = float64(i+1) * 10 // far apart: the duplicate guard stays out of it
		return q
	}, 1)
	run("rate throttle of 5", 5, func(i int) ActionRequest {
		q := buy(owner(ClassTrade, "create_order"))
		q.Order.Amount = float64(i+1) * 10
		return q
	}, 5)
	run("duplicate guard, identical orders", 0, func(int) ActionRequest { return buy(owner(ClassTrade, "create_order")) }, 1)
}

// Only an order that is allowed is counted: one a later gate refuses must not use up
// a rate slot, and a preview or an exit is never counted.
func TestEnforce_CountsOnlyAllowedPlacements(t *testing.T) {
	r := newRig(t, nil)
	r.cfg.TradingRisk.MaxOrdersPer10Min = 1

	stranger := buy(owner(ClassTrade, "create_order"))
	stranger.Actor = ActorOther // gate C: notify_only
	if v := r.enforce(stranger); v.Mode != ModeNotifyOnly {
		t.Fatalf("a stranger's order: %s %v", v.Mode, v.Reasons)
	}
	preview := buy(owner(ClassTrade, "create_order"))
	preview.Order.Placing = false
	if v := r.enforce(preview); v.Mode != ModeAct {
		t.Fatalf("a preview: %s %v", v.Mode, v.Reasons)
	}
	exit := buy(owner(ClassTrade, "create_order"))
	exit.ReduceOnly = true
	exit.Order.Side = "sell"
	if v := r.enforce(exit); v.Mode != ModeAct {
		t.Fatalf("an exit: %s %v", v.Mode, v.Reasons)
	}
	if v := r.enforce(buy(owner(ClassTrade, "create_order"))); v.Mode != ModeAct {
		t.Fatalf("the first real order was refused although nothing before it counted: %s %v", v.Mode, v.Reasons)
	}
	v := r.enforce(buy(owner(ClassTrade, "create_order")))
	if v.Mode != ModeDeny || !strings.HasPrefix(v.Reasons[0], "B:") || !strings.Contains(v.Reasons[0], "rate") {
		t.Fatalf("the second order: %s %v, want a B rate deny", v.Mode, v.Reasons)
	}
	// An exit is exempt however many orders were placed.
	if v := r.enforce(exit); v.Mode != ModeAct {
		t.Fatalf("an exit past the rate limit: %s %v", v.Mode, v.Reasons)
	}
}

// Enforce runs only on calls today's checks allowed, so it skips gate L: a held tool
// whose approval was granted is not asked about again. Authorize (shadow) still asks.
func TestEnforce_SkipsGateL(t *testing.T) {
	r := newRig(t, nil)
	r.row = RowHold
	q := buy(owner(ClassTrade, "create_order"))
	if v := r.auth(q); v.Mode != ModeAsk || !strings.HasPrefix(v.Reasons[0], "L:") {
		t.Fatalf("Authorize on a held tool: %s %v", v.Mode, v.Reasons)
	}
	if v := r.enforce(q); v.Mode != ModeAct {
		t.Fatalf("Enforce on a held tool whose approval was granted: %s %v", v.Mode, v.Reasons)
	}
	// Everything after L still applies.
	r.killed = true
	other := q
	other.Actor = ActorOther
	if v := r.enforce(other); v.Mode != ModeDeny || !strings.HasPrefix(v.Reasons[0], "K:") {
		t.Fatalf("kill switch: %s %v", v.Mode, v.Reasons)
	}
	q.Order.Amount = 50 // not a repeat of the order above
	if v := r.enforce(q); v.Mode != ModeAct {
		t.Fatalf("the owner's own root turn under the kill switch: %s %v", v.Mode, v.Reasons)
	}
}

// The owner's own trade is refused only by gate B's hard limits the owner set, and
// never by the dial, provenance of an owner turn (no holder), or the kill switch.
func TestEnforce_OwnerOnlyRefusedByTheirOwnLimits(t *testing.T) {
	r := newRig(t, nil)
	r.killed = true
	q := buy(owner(ClassTrade, "create_order"))
	if v := r.enforce(q); v.Mode != ModeAct {
		t.Fatalf("owner, no brakes, kill engaged: %s %v", v.Mode, v.Reasons)
	}
	r.cfg.TradingRisk.SymbolAllowlist = []string{"ETH/USDT"}
	if v := r.enforce(q); v.Mode != ModeDeny || !strings.HasPrefix(v.Reasons[0], "B:") {
		t.Fatalf("owner, symbol outside their allowlist: %s %v", v.Mode, v.Reasons)
	}
	exit := q
	exit.ReduceOnly = true
	if v := r.enforce(exit); v.Mode != ModeAct {
		t.Fatalf("the owner's exit is exempt from the brakes: %s %v", v.Mode, v.Reasons)
	}
	// Untrusted text in the owner's own turn never arises (no holder), but if it did
	// the provenance gate would still hold: it is not the owner's hands that move.
	taint := q
	taint.Trust = proactive.TrustUntrusted
	if v := r.enforce(taint); v.Mode == ModeAct {
		t.Fatal("a tainted turn must not act")
	}
}

// Enforcing a USD cap values the order on the call path; an order that cannot be
// valued is refused (fail closed), and one that can is checked against the cap.
func TestEnforce_UnknownValueWithCapRefuses(t *testing.T) {
	r := newRig(t, func(_ *rig, d *Deps) { d.Valuer = fakeValuer{ok: false} })
	r.cfg.TradingRisk.MaxOrderValueUSD = 100
	q := buy(owner(ClassTrade, "create_order"))
	q.Order.USDKnown = false
	if v := r.enforce(q); v.Mode != ModeDeny || !strings.Contains(v.Reasons[0], "cannot be valued") {
		t.Fatalf("unknown value under a cap: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Valuer = fakeValuer{usd: 500, ok: true}
	if v := r.enforce(q); v.Mode != ModeDeny {
		t.Fatalf("500 USD under a 100 USD cap: %s %v", v.Mode, v.Reasons)
	}
	r.e.d.Valuer = fakeValuer{usd: 50, ok: true}
	if v := r.enforce(q); v.Mode != ModeAct {
		t.Fatalf("50 USD under a 100 USD cap: %s %v", v.Mode, v.Reasons)
	}
}
