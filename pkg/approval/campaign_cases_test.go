package approval

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

)

// Cases of the proactive-agent test campaign (B9-4, B9-5, B9-6, B3-6) that the Sandbox Lab
// cannot express: a mode flip, a config reload and a tampered row all need the service's
// own seams, so they are proved here against the real Service, store and journal engine
// with only the exchange faked.

// hookBackend is the fake exchange with a hook that runs at the throttle step, which sits
// between recheck and place: the moment a reload or a tamper can land in a real run.
type hookBackend struct {
	*fakeBackend
	atThrottle func()
}

func (b *hookBackend) Throttle(p OrderParams) (bool, time.Duration) {
	if b.atThrottle != nil {
		b.atThrottle()
	}
	return b.fakeBackend.Throttle(p)
}

func withHook(w *world, hook func()) procOpt {
	return func(o *Options) { o.Backend = &hookBackend{fakeBackend: w.backend, atThrottle: hook} }
}

func mustFailNothingPlaced(t *testing.T, w *world, p *proc, id, wantInOutcome string) Proposal {
	t.Helper()
	got := p.get(id)
	if got.Status != StatusFailed {
		t.Fatalf("status = %s (outcome %q), want failed", got.Status, got.Outcome)
	}
	if w.backend.orderCount() != 0 || w.backend.placeCalls != 0 {
		t.Fatalf("%d orders, %d place calls: something was placed", w.backend.orderCount(), w.backend.placeCalls)
	}
	if !strings.Contains(got.Outcome, wantInOutcome) || !strings.Contains(got.Outcome, "NOTHING was placed") {
		t.Errorf("outcome = %q, want %q and \"NOTHING was placed\"", got.Outcome, wantInOutcome)
	}
	return got
}

// B9-4: the card said how the order would be placed (paper or real). The mode flipped between
// the proposal and the execution, in either direction, and the order is refused at recheck.
func TestB94_ModeFlippedBetweenProposalAndExecutionRefusesBothWays(t *testing.T) {
	for _, tc := range []struct{ name, proposedAs, flippedTo string }{
		{"live to paper", ModeLive, ModePaper},
		{"paper to live", ModePaper, ModeLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.backend.mode = tc.proposedAs
			p := w.newProc(t0)
			prop := p.propose(sellBTC())
			if prop.Mode != tc.proposedAs {
				t.Fatalf("the proposal stored mode %q, want %q", prop.Mode, tc.proposedAs)
			}
			prop = p.approve(prop)

			w.backend.mode = tc.flippedTo // the dashboard switch, after the owner pressed Approve
			p.run(prop.ID)

			mustFailNothingPlaced(t, w, p, prop.ID, "trading mode changed")
			if w.backend.throttles != 0 {
				t.Error("the limiter was spent for an order that was never placed")
			}
			// A second run of the finished workflow places nothing either.
			p.run(prop.ID)
			if w.backend.orderCount() != 0 {
				t.Error("a re-run placed the order")
			}
		})
	}
}

// B9-4, other end of the window: the mode is the same at recheck and flips before place. The
// order is placed in the mode recheck vouched for; recheck compared it to the card, so what
// is placed is what was approved. (Documented here so a change to the place step that reads
// the mode again has a test to update, not a hole to find.)
func TestB94_ModeIsCheckedAgainstTheCardAtRecheck(t *testing.T) {
	w := newWorld(t)
	w.backend.mode = ModePaper
	p := w.newProc(t0)
	prop := p.approve(p.propose(sellBTC()))
	p.run(prop.ID)
	got := p.get(prop.ID)
	if got.Status != StatusExecuted || got.Mode != ModePaper {
		t.Fatalf("an unchanged paper mode must execute: status %s, mode %s, outcome %q", got.Status, got.Mode, got.Outcome)
	}
}

// B9-5: approvals are switched off by a reload while a card is pending, then on again. The
// reload builds a fresh Service over the same database (a second proc is that generation).
// The card is still pending, nothing is placed by the new generation's sweeper or recovery,
// and only a fresh owner decision starts an order, exactly one.
func TestB95_PendingCardSurvivesAReloadAndPlacesNothingWithoutAFreshDecision(t *testing.T) {
	w := newWorld(t)
	gen1 := w.newProc(t0)
	prop := gen1.propose(sellBTC())

	// Approvals off: no service exists. The control layer answers "not available" for a decision
	// (pkg/control: "approvals not wired"; cmd/.../gateway: TestB95_ReloadWithApprovalsOffAnswersNotAvailable).
	// Re-enable: a new generation.
	gen2 := w.newProc(t0.Add(time.Minute))
	if err := gen2.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	gen2.svc.sweep(context.Background())
	gen2.run(prop.ID) // the workflow of a proposal nobody approved does not exist: nothing to run

	got := gen2.get(prop.ID)
	if got.Status != StatusPending || got.WorkflowID != "" {
		t.Fatalf("after re-enable: status %s, workflow %q, want pending with none", got.Status, got.WorkflowID)
	}
	if w.backend.orderCount() != 0 || w.backend.placeCalls != 0 || w.backend.rechecks != 0 {
		t.Fatalf("re-enabling placed or rechecked an order: %d orders, %d place, %d recheck",
			w.backend.orderCount(), w.backend.placeCalls, w.backend.rechecks)
	}

	// A fresh decision on the new generation places exactly one order.
	gen2.approve(got)
	gen2.run(prop.ID)
	gen2.run(prop.ID)
	if w.backend.orderCount() != 1 || w.backend.maxPerKey() != 1 {
		t.Fatalf("%d orders (max %d per key) after one fresh decision, want exactly one", w.backend.orderCount(), w.backend.maxPerKey())
	}
	if end := gen2.get(prop.ID); end.Status != StatusExecuted {
		t.Errorf("status = %s, want executed", end.Status)
	}
}

// B9-5, the card expires while approvals are off: re-enabled, it cannot be approved.
func TestB95_CardThatExpiredWhileApprovalsWereOffCannotBeApproved(t *testing.T) {
	w := newWorld(t)
	gen1 := w.newProc(t0)
	prop := gen1.propose(sellBTC())

	gen2 := w.newProc(t0.Add(time.Hour)) // well past the 10 minute window
	_, err := gen2.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID)
	mustErrIs(t, err, ErrNotFound)
	gen2.svc.sweep(context.Background())
	if got := gen2.get(prop.ID); got.Status != StatusExpired || w.backend.orderCount() != 0 {
		t.Fatalf("status %s, %d orders", got.Status, w.backend.orderCount())
	}
}

// B9-6: the owner is changed by a reload between the decision and the order. The decider is
// re-proved against the CONFIGURED owner at recheck and again at place, so the order is
// refused and the message says why. Three shapes: another owner at the next generation,
// no owner at all, and the owner changing between recheck and place.
func TestB96_OwnerChangedByAReloadRefusesNotDecidedByTheConfiguredOwner(t *testing.T) {
	const want = "not decided by the configured owner"

	t.Run("a different owner at the next generation", func(t *testing.T) {
		w := newWorld(t)
		gen1 := w.newProc(t0)
		prop := approved(t, gen1)
		w.cfg.Proactive.Owner.SenderID = "telegram:2002" // the reload
		gen2 := w.newProc(t0.Add(time.Second))
		gen2.run(prop.ID)
		mustFailNothingPlaced(t, w, gen2, prop.ID, want)
		if w.backend.rechecks != 0 {
			t.Error("recheck ran for a proposal the configured owner did not decide")
		}
	})

	t.Run("the owner is removed", func(t *testing.T) {
		w := newWorld(t)
		gen1 := w.newProc(t0)
		prop := approved(t, gen1)
		w.cfg.Proactive.Owner.SenderID, w.cfg.Proactive.Owner.ChatID, w.cfg.Proactive.Owner.Channel = "", "", ""
		gen2 := w.newProc(t0.Add(time.Second))
		gen2.run(prop.ID)
		mustFailNothingPlaced(t, w, gen2, prop.ID, want)
	})

	t.Run("changed between recheck and place", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0, withHook(w, func() { w.cfg.Proactive.Owner.SenderID = "telegram:2002" }))
		prop := approved(t, p)
		p.run(prop.ID)
		mustFailNothingPlaced(t, w, p, prop.ID, want)
		if w.backend.rechecks != 1 {
			t.Errorf("rechecks = %d, want 1 (the change landed after it)", w.backend.rechecks)
		}
	})
}

// tamperCases are rewrites of a stored order that a database writer could make. Each leaves
// the row looking like a valid order, so only the hash can tell.
var tamperCases = map[string]string{
	"side":    `UPDATE proposals SET params = replace(params, '"side":"sell"', '"side":"buy"') WHERE id = ?`,
	"amount":  `UPDATE proposals SET params = replace(params, '0.0123', '12.3') WHERE id = ?`,
	"symbol":  `UPDATE proposals SET params = replace(params, 'BTC/USDT', 'ETH/USDT') WHERE id = ?`,
	"account": `UPDATE proposals SET params = replace(params, '"account":"main"', '"account":"other"') WHERE id = ?`,
	"hash":    `UPDATE proposals SET hash = zeroblob(length(hash)) WHERE id = ?`,
}

func tamper(t *testing.T, db *sql.DB, query, id string) {
	t.Helper()
	res, err := db.Exec(query, id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("the tamper changed %d rows", n)
	}
}

// B3-6 at decide time: a stored order that no longer matches its hash is cancelled with
// "integrity check failed" when the owner presses Approve, answers as unknown, and places nothing.
func TestB36_TamperedRowIsCancelledAtDecideWithTheIntegrityReason(t *testing.T) {
	for name, query := range tamperCases {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			p := w.newProc(t0)
			prop := p.propose(sellBTC())
			tamper(t, p.db, query, prop.ID)

			_, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID)
			mustErrIs(t, err, ErrNotFound) // the caller learns nothing about why
			got := p.get(prop.ID)
			if got.Status != StatusCancelled || got.Outcome != "integrity check failed" {
				t.Fatalf("status %s, outcome %q, want cancelled with \"integrity check failed\"", got.Status, got.Outcome)
			}
			if got.WorkflowID != "" || w.backend.orderCount() != 0 || w.backend.rechecks != 0 || w.backend.placeCalls != 0 {
				t.Fatalf("workflow %q, %d orders, %d rechecks: something ran", got.WorkflowID, w.backend.orderCount(), w.backend.rechecks)
			}
		})
	}
}

// B3-6 at the approval.order recheck, and after it: tampered after approval, before recheck
// (the existing TestExecute_TamperedRowIsNotExecuted covers one rewrite); here every rewrite,
// and one that lands between recheck and place, where a proposal that verified a moment ago
// no longer does.
func TestB36_TamperedAfterApprovalIsRefusedAtRecheckAndAtPlace(t *testing.T) {
	for name, query := range tamperCases {
		t.Run("before recheck: "+name, func(t *testing.T) {
			w := newWorld(t)
			p := w.newProc(t0)
			prop := approved(t, p)
			tamper(t, p.db, query, prop.ID)
			p.run(prop.ID)
			got := p.get(prop.ID)
			if got.Status != StatusFailed || w.backend.orderCount() != 0 || w.backend.rechecks != 0 || w.backend.placeCalls != 0 {
				t.Fatalf("status %s, %d orders, %d rechecks, %d place calls", got.Status, w.backend.orderCount(), w.backend.rechecks, w.backend.placeCalls)
			}
			if !strings.Contains(got.Outcome, "hash") || !strings.Contains(got.Outcome, "NOTHING was placed") {
				t.Errorf("outcome = %q", got.Outcome)
			}
		})
		t.Run("between recheck and place: "+name, func(t *testing.T) {
			w := newWorld(t)
			var p *proc
			var id string
			p = w.newProc(t0, withHook(w, func() { tamper(t, p.db, query, id) }))
			prop := approved(t, p)
			id = prop.ID
			p.run(prop.ID)
			mustFailNothingPlaced(t, w, p, prop.ID, "hash")
			if w.backend.rechecks != 1 {
				t.Errorf("rechecks = %d, want 1 (the tamper landed after it)", w.backend.rechecks)
			}
		})
	}
}
