package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// approved proposes and approves a sell, returning the proposal.
func approved(t *testing.T, p *proc) Proposal {
	t.Helper()
	return p.approve(p.propose(sellBTC()))
}

// runUntilDone runs the workflow, advancing the clock between runs, until it is
// terminal or tries run out. It returns the workflow.
func runUntilDone(t *testing.T, p *proc, id string, step time.Duration) journal.Workflow {
	t.Helper()
	for i := 0; i < 20; i++ {
		p.run(id)
		w := p.workflow(id)
		if w.Status.Terminal() {
			return w
		}
		p.clock.Advance(step)
	}
	return p.workflow(id)
}

// TestDecide_ApproveExecutesOnce: an approved order is rechecked, throttled,
// placed, verified, recorded and receipted, and a second run does nothing.
func TestDecide_ApproveExecutesOnce(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p)
	if got := p.get(prop.ID); got.Status != StatusExecuting || got.WorkflowID != WorkflowID(prop.ID) {
		t.Fatalf("after approval: %+v", got)
	}
	tokens := w.backend.tokens
	p.run(prop.ID)
	p.run(prop.ID)

	got := p.get(prop.ID)
	if got.Status != StatusExecuted || !got.Notified || got.FinishedAt == nil {
		t.Fatalf("after execution: %+v", got)
	}
	if w.backend.orderCount() != 1 || w.backend.maxPerKey() != 1 {
		t.Fatalf("%d orders (max %d per key), want exactly one", w.backend.orderCount(), w.backend.maxPerKey())
	}
	if w.backend.placedAmounts[0] != 0.0123 {
		t.Errorf("placed %v, want the stored amount", w.backend.placedAmounts)
	}
	if w.backend.rechecks != 1 || w.backend.records != 1 || w.backend.throttles != 1 || w.backend.tokens != tokens-1 {
		t.Errorf("rechecks %d, records %d, limiter calls %d, tokens left %d (of %d): each must happen once",
			w.backend.rechecks, w.backend.records, w.backend.throttles, w.backend.tokens, tokens)
	}
	receipt := w.notifier.last()
	for _, want := range []string{"executed", "SELL 0.0123 BTC", "okx/main", "Filled 0.0123 @ 60000", "Order ID:"} {
		if !strings.Contains(receipt, want) {
			t.Errorf("receipt lacks %q: %q", want, receipt)
		}
	}
	steps, _ := p.eng.Steps(context.Background(), WorkflowID(prop.ID))
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "recheck,throttle,place,verify,record,receipt" {
		t.Errorf("steps = %v", names)
	}
}

func TestExecute_PaperAndRealExitAreMarked(t *testing.T) {
	for mode, tag := range map[string]string{ModePaper: "PAPER", ModeRealExit: "REAL"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			w.backend.mode = mode
			p := w.newProc(t0)
			prop := p.propose(sellBTC())
			first := strings.SplitN(w.notifier.all()[0].Title, "\n", 2)[0]
			if !strings.HasPrefix(first, tag) {
				t.Errorf("the card's first line does not say %s: %q", tag, first)
			}
			p.approve(prop)
			p.run(prop.ID)
			if !strings.HasPrefix(w.notifier.last(), "\n"+tag) && !strings.Contains(w.notifier.last(), tag) {
				t.Errorf("the receipt does not say %s: %q", tag, w.notifier.last())
			}
		})
	}
}

// Execution under a crash at every point of every step, on an exchange that can
// tag an order with a client id and one that cannot: never two orders.
func TestExecute_CrashAtEveryStepAndPoint(t *testing.T) {
	steps := []string{"recheck", "throttle", "place", "verify", "record", "receipt"}
	points := []string{journal.CrashBeforeStarted, journal.CrashAfterStarted, journal.CrashAfterFn, journal.CrashAfterOutput}
	for _, capable := range []bool{true, false} {
		for _, step := range steps {
			for _, point := range points {
				name := fmt.Sprintf("capable=%v/%s/%s", capable, step, point)
				t.Run(name, func(t *testing.T) {
					w := newWorld(t)
					w.backend.capable = capable
					a := w.newProc(t0)
					prop := approved(t, a)

					kill := a.eng.SimulateKill(func(pt, st string, _ int) bool { return pt == point && st == step })
					a.run(prop.ID)
					if !kill.Dead() {
						t.Fatal("the crash point was never reached")
					}
					if w.backend.maxPerKey() > 1 {
						t.Fatal("two orders before the restart")
					}

					// The process after the restart, past the lease and its grace.
					b := w.newProc(t0.Add(time.Minute))
					if err := b.svc.Recover(context.Background()); err != nil {
						t.Fatal(err)
					}
					wf := runUntilDone(t, b, prop.ID, time.Minute)
					if !wf.Status.Terminal() {
						t.Fatalf("workflow is %s after the restart", wf.Status)
					}
					got := b.get(prop.ID)

					if w.backend.orderCount() > 1 || w.backend.maxPerKey() > 1 {
						t.Fatalf("%d orders, want at most one", w.backend.orderCount())
					}
					// The one place a crash cannot be healed: an exchange that cannot tag
					// the order, after the place step was started and before an order
					// exists. Nothing is placed again and the owner is told to look.
					unprovable := !capable && step == "place" && point == journal.CrashAfterStarted
					if unprovable {
						if got.Status != StatusFailed || w.backend.orderCount() != 0 {
							t.Fatalf("status %s with %d orders; want failed with none", got.Status, w.backend.orderCount())
						}
						if !strings.Contains(got.Outcome, "MAY OR MAY NOT") || !strings.Contains(got.Outcome, "Check okx") {
							t.Errorf("the owner is not told to check the exchange: %q", got.Outcome)
						}
						return
					}
					if got.Status != StatusExecuted || w.backend.orderCount() != 1 {
						t.Fatalf("status %s with %d orders, want executed with one: %q", got.Status, w.backend.orderCount(), got.Outcome)
					}
					if !got.Notified {
						t.Error("the owner was not told")
					}
				})
			}
		}
	}
}

// A crash between the approval and the workflow start: the restart starts it, once.
func TestExecute_ApprovedWithoutWorkflowIsStartedOnRestart(t *testing.T) {
	w := newWorld(t)
	a := w.newProc(t0)
	a.svc.SetWorkflows(nil) // the process dies before the workflow can be started
	prop := a.propose(sellBTC())
	if _, err := decide(a, prop, prop.Nonce, true, ownerID); err != nil {
		t.Fatal(err)
	}
	if got := a.get(prop.ID); got.Status != StatusApproved || got.WorkflowID != "" {
		t.Fatalf("after the lost start: %+v", got)
	}

	b := w.newProc(t0.Add(time.Minute))
	for i := 0; i < 3; i++ { // every sweep and every start heals; healing twice is harmless
		if err := b.svc.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.get(prop.ID); got.Status != StatusExecuting {
		t.Fatalf("after Recover: %s", got.Status)
	}
	b.run(prop.ID)
	b.run(prop.ID)
	if got := b.get(prop.ID); got.Status != StatusExecuted || w.backend.orderCount() != 1 {
		t.Fatalf("status %s, %d orders; want executed, one", got.Status, w.backend.orderCount())
	}
	if b.eng.Stats().Started != 1 {
		t.Errorf("%d workflows started, want one", b.eng.Stats().Started)
	}
}

// A day's outage between the approval and the execution: the owner agreed to an
// order then, not now. Nothing is placed, and the owner is told why.
func TestExecute_StaleApprovalAfterOutageIsRefused(t *testing.T) {
	w := newWorld(t)
	a := w.newProc(t0)
	prop := approved(t, a)

	b := w.newProc(t0.Add(24 * time.Hour))
	if err := b.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	wf := runUntilDone(t, b, prop.ID, time.Minute)
	if wf.Status != journal.StatusFailed {
		t.Fatalf("workflow is %s, want failed", wf.Status)
	}
	got := b.get(prop.ID)
	if got.Status != StatusFailed || w.backend.orderCount() != 0 || w.backend.rechecks != 0 {
		t.Fatalf("status %s, %d orders, %d rechecks", got.Status, w.backend.orderCount(), w.backend.rechecks)
	}
	for _, want := range []string{"too old", "NOTHING was placed", "propose it again"} {
		if !strings.Contains(got.Outcome, want) {
			t.Errorf("the owner's message lacks %q: %q", want, got.Outcome)
		}
	}
	if strings.Contains(got.Outcome, "Check okx") {
		t.Error("a refusal before anything was sent told the owner to check the exchange")
	}
	if !got.Notified {
		t.Error("the owner was not told")
	}
}

// The bound is the proposal's own window, to the second, and applies to exits too.
func TestExecute_StalenessBoundary(t *testing.T) {
	for name, tc := range map[string]struct {
		wait time.Duration
		want Status
	}{
		"at the window":                  {10 * time.Minute, StatusExecuted},
		"past the window":                {10*time.Minute + time.Second, StatusFailed},
		"a reducing order is not exempt": {time.Hour, StatusFailed},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.backend.reducing = true
			a := w.newProc(t0)
			prop := approved(t, a)
			b := w.newProc(t0.Add(tc.wait))
			b.svc.Recover(context.Background())
			runUntilDone(t, b, prop.ID, time.Minute)
			if got := b.get(prop.ID); got.Status != tc.want {
				t.Fatalf("status = %s (%q), want %s", got.Status, got.Outcome, tc.want)
			}
		})
	}
}

func TestExecute_RecheckRefusalPlacesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(w *world)
		want string
	}{
		"a gate fails": {func(w *world) {
			w.backend.recheck = errors.New("gate price collar refused the order: moved 2.00% against it")
		}, "price collar"},
		"the mode changed": {func(w *world) { w.backend.mode = ModePaper }, "trading mode changed"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			p := w.newProc(t0)
			prop := approved(t, p)
			tc.set(w)
			p.run(prop.ID)
			got := p.get(prop.ID)
			if got.Status != StatusFailed || w.backend.orderCount() != 0 || w.backend.placeCalls != 0 {
				t.Fatalf("status %s, %d orders, %d place calls", got.Status, w.backend.orderCount(), w.backend.placeCalls)
			}
			if !strings.Contains(got.Outcome, tc.want) || !strings.Contains(got.Outcome, "NOTHING was placed") ||
				strings.Contains(got.Outcome, "Check okx") {
				t.Errorf("message = %q", got.Outcome)
			}
			if w.backend.throttles != 0 {
				t.Error("the limiter was spent for an order that was never placed")
			}
		})
	}
}

// The kill flag at recheck: a buy is refused, an order that reduces risk goes through.
func TestExecute_KillFlagAtRecheck(t *testing.T) {
	for _, reducing := range []bool{false, true} {
		t.Run(fmt.Sprintf("reducing=%v", reducing), func(t *testing.T) {
			w := newWorld(t)
			w.backend.reducing = reducing
			p := w.newProc(t0)
			prop := approved(t, p)
			w.killer.on = true
			p.run(prop.ID)
			got := p.get(prop.ID)
			if reducing && (got.Status != StatusExecuted || w.backend.orderCount() != 1) {
				t.Fatalf("an exit was stopped by the kill flag: %s %q", got.Status, got.Outcome)
			}
			if !reducing && (got.Status != StatusFailed || w.backend.orderCount() != 0 || !strings.Contains(got.Outcome, "kill switch")) {
				t.Fatalf("a buy passed the kill flag: %s %q (%d orders)", got.Status, got.Outcome, w.backend.orderCount())
			}
		})
	}
}

// A proposal from a background turn is held to risk-reducing at execution too.
func TestExecute_ProactiveOrderThatNoLongerReducesIsRefused(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop, err := p.svc.Propose(context.Background(), ProposeRequest{
		Order: sellBTC(), Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: true, CandidateID: "c1",
	})
	if err != nil {
		t.Fatal(err)
	}
	p.approve(prop)
	w.backend.reducing = false // the holding was sold meanwhile; the order would now be a short
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusFailed || w.backend.orderCount() != 0 {
		t.Fatalf("status %s, %d orders", got.Status, w.backend.orderCount())
	}
}

// The limiter refuses: the throttle step waits with a persisted backoff and nothing
// is placed until a token exists, and the wait is bounded by the approval's age.
func TestExecute_LimiterRefusalWaitsThenProceeds(t *testing.T) {
	w := newWorld(t)
	w.backend.tokens = 0
	p := w.newProc(t0)
	prop := approved(t, p)

	p.run(prop.ID)
	wf := p.workflow(prop.ID)
	if wf.Status != journal.StatusWaiting || wf.WakeAt == nil {
		t.Fatalf("workflow is %s (wake %v), want waiting on a persisted backoff", wf.Status, wf.WakeAt)
	}
	if w.backend.placeCalls != 0 {
		t.Fatal("placed while the limiter refused")
	}
	// A restart in the middle of the wait neither resets nor shortens it.
	q := w.newProc(t0.Add(time.Minute))
	_ = q
	w.backend.tokens = 1
	p.clock.Advance(20 * time.Second)
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusExecuted || w.backend.orderCount() != 1 {
		t.Fatalf("status %s, %d orders: %q", got.Status, w.backend.orderCount(), got.Outcome)
	}
	if w.backend.tokens != 0 {
		t.Errorf("tokens left %d: exactly one must be spent", w.backend.tokens)
	}
}

func TestExecute_LimiterWaitIsBoundedByTheApprovalsAge(t *testing.T) {
	w := newWorld(t)
	w.backend.tokens = 0
	p := w.newProc(t0)
	prop := approved(t, p)
	wf := runUntilDone(t, p, prop.ID, 3*time.Minute)
	if wf.Status != journal.StatusFailed {
		t.Fatalf("workflow is %s: a limiter that never refills must not wait for ever", wf.Status)
	}
	got := p.get(prop.ID)
	if got.Status != StatusFailed || w.backend.orderCount() != 0 || !strings.Contains(got.Outcome, "NOTHING was placed") {
		t.Fatalf("status %s, %d orders: %q", got.Status, w.backend.orderCount(), got.Outcome)
	}
}

// How a failure is worded: without "check the exchange" when nothing was sent,
// with it when the order may exist.
func TestExecute_FailureNotifications(t *testing.T) {
	t.Run("a definite rejection says nothing was placed", func(t *testing.T) {
		w := newWorld(t)
		w.backend.placeErr = func(int) error { return errors.New("[ccxtError]::[InsufficientFunds]::[okx balance too low]") }
		p := w.newProc(t0)
		prop := approved(t, p)
		p.run(prop.ID)
		got := p.get(prop.ID)
		if got.Status != StatusFailed || !strings.Contains(got.Outcome, "NOTHING was placed") || strings.Contains(got.Outcome, "MAY OR MAY NOT") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
		if w.backend.placeCalls != 1 {
			t.Errorf("a definite rejection was retried: %d calls", w.backend.placeCalls)
		}
	})

	t.Run("an unknown outcome that stays unknown says the order may exist", func(t *testing.T) {
		w := newWorld(t)
		w.backend.placeErr = func(int) error { return fmt.Errorf("timeout: %w", broker.ErrOutcomeUnknown) }
		p := w.newProc(t0)
		prop := approved(t, p)
		runUntilDone(t, p, prop.ID, time.Minute)
		got := p.get(prop.ID)
		if got.Status != StatusFailed || !strings.Contains(got.Outcome, "MAY OR MAY NOT HAVE BEEN PLACED on okx") ||
			!strings.Contains(got.Outcome, "Check okx") || !strings.Contains(got.Outcome, "do not approve it again") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
		if w.backend.placeCalls != placeRetries+1 {
			t.Errorf("%d place attempts, want %d (the declared retries)", w.backend.placeCalls, placeRetries+1)
		}
	})

	t.Run("an exchange that cannot be reconciled is never placed on again", func(t *testing.T) {
		w := newWorld(t)
		w.backend.placeErr = func(attempt int) error {
			if attempt == 1 {
				return fmt.Errorf("timeout: %w", broker.ErrOutcomeUnknown)
			}
			return fmt.Errorf("%w", broker.ErrReconcileUnproven)
		}
		p := w.newProc(t0)
		prop := approved(t, p)
		runUntilDone(t, p, prop.ID, time.Minute)
		got := p.get(prop.ID)
		if got.Status != StatusFailed || !strings.Contains(got.Outcome, "Check okx") || w.backend.orderCount() != 0 {
			t.Fatalf("%s: %q (%d orders)", got.Status, got.Outcome, w.backend.orderCount())
		}
		if w.backend.placeCalls != 2 {
			t.Errorf("%d place calls: an Unproven result must not be retried", w.backend.placeCalls)
		}
	})

	t.Run("a panic while placing is told to the owner by the sweeper", func(t *testing.T) {
		w := newWorld(t)
		w.backend.placeErr = func(int) error { panic("boom") }
		p := w.newProc(t0)
		prop := approved(t, p)
		p.run(prop.ID)
		if got := p.get(prop.ID); got.Status != StatusExecuting {
			t.Fatalf("status %s: the engine refuses further steps after a panic", got.Status)
		}
		if err := p.svc.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := p.get(prop.ID)
		if got.Status != StatusFailed || !strings.Contains(got.Outcome, "MAY OR MAY NOT") || !strings.Contains(got.Outcome, "Check okx") || !got.Notified {
			t.Fatalf("%s: %q notified=%v", got.Status, got.Outcome, got.Notified)
		}
		if n := w.notifier.containing("MAY OR MAY NOT"); n != 1 {
			t.Errorf("the owner was told %d times", n)
		}
	})

	t.Run("a failure message is sent once even if two paths settle it", func(t *testing.T) {
		w := newWorld(t)
		w.backend.recheck = errors.New("gate permission refused the order: nope")
		p := w.newProc(t0)
		prop := approved(t, p)
		p.run(prop.ID)
		_ = p.svc.Recover(context.Background())
		_ = p.svc.Recover(context.Background())
		if n := w.notifier.containing("NOTHING was placed"); n != 1 {
			t.Errorf("the owner was told %d times", n)
		}
	})
}

// A message that could not be delivered is sent again by the sweeper.
func TestExecute_UndeliveredOutcomeIsResent(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p)
	w.notifier.fail = true
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusExecuted || got.Notified {
		t.Fatalf("status %s notified %v", got.Status, got.Notified)
	}
	w.notifier.fail = false
	if err := p.svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.get(prop.ID); !got.Notified || w.notifier.containing("executed") != 1 {
		t.Fatalf("notified=%v, %d receipts", got.Notified, w.notifier.containing("executed"))
	}
}

// The verify step reports what it can and never turns a placed order into a failure.
func TestExecute_VerifyNeverFailsAPlacedOrder(t *testing.T) {
	t.Run("a fetch that fails falls back to the answer to the placement", func(t *testing.T) {
		w := newWorld(t)
		w.backend.fetchErr = errors.New("503")
		p := w.newProc(t0)
		prop := approved(t, p)
		p.run(prop.ID)
		if got := p.get(prop.ID); got.Status != StatusExecuted || !strings.Contains(got.Outcome, "Filled 0.0123") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
	})
	t.Run("a market order still open after the polls is reported as it is", func(t *testing.T) {
		w := newWorld(t)
		w.backend.fetchFill = &Fill{Known: true, Status: "open", Filled: 0.01}
		p := w.newProc(t0, func(o *Options) { o.VerifyPolls = 3; o.VerifyPollInterval = -1 })
		prop := approved(t, p)
		p.run(prop.ID)
		got := p.get(prop.ID)
		if got.Status != StatusExecuted || !strings.Contains(got.Outcome, "Status: open") || !strings.Contains(got.Outcome, "Filled 0.01") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
	})
	t.Run("nothing known says to check the exchange", func(t *testing.T) {
		w := newWorld(t)
		w.backend.fetchErr = errors.New("503")
		w.backend.noFill = true
		p := w.newProc(t0)
		prop := approved(t, p)
		p.run(prop.ID)
		got := p.get(prop.ID)
		if got.Status != StatusExecuted || !strings.Contains(got.Outcome, "fill could not be confirmed: check the exchange") {
			t.Fatalf("%s: %q", got.Status, got.Outcome)
		}
	})
}

// A proposal row changed after it was approved is not executed.
func TestExecute_TamperedRowIsNotExecuted(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p)
	if _, err := p.db.Exec(`UPDATE proposals SET params = replace(params, '0.0123', '12.3') WHERE id = ?`, prop.ID); err != nil {
		t.Fatal(err)
	}
	p.run(prop.ID)
	got := p.get(prop.ID)
	if got.Status != StatusFailed || w.backend.orderCount() != 0 || w.backend.rechecks != 0 {
		t.Fatalf("status %s, %d orders, %d rechecks", got.Status, w.backend.orderCount(), w.backend.rechecks)
	}
	if !strings.Contains(got.Outcome, "hash") {
		t.Errorf("message = %q", got.Outcome)
	}
}

// A proposal decided by somebody who is no longer the owner is not executed.
func TestExecute_OwnerChangedBetweenApprovalAndExecution(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p)
	w.cfg.Proactive.Owner.SenderID = "telegram:2002"
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusFailed || w.backend.orderCount() != 0 {
		t.Fatalf("status %s, %d orders", got.Status, w.backend.orderCount())
	}
}

// The sweeper runs on the injected clock, heals at its first pass and stops with its context.
func TestRun_SweepsOnTheClockAndStops(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.svc.Run(ctx); close(done) }()

	p.clock.Advance(11 * time.Minute) // expired, and the sweeper's timer fires
	deadline := time.Now().Add(5 * time.Second)
	for p.get(prop.ID).Status != StatusExpired {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper did not expire the proposal")
		}
		p.clock.Advance(SweepEvery)
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper did not stop with its context")
	}
}
