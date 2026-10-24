package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func decide(p *proc, prop Proposal, nonce string, approve bool, sender string) (Proposal, error) {
	return p.svc.Decide(context.Background(), prop.Code, nonce, approve, sender)
}

func TestDecide_NonOwnerRejected(t *testing.T) {
	for name, sender := range map[string]string{
		"another user":        "telegram:999",
		"another platform":    "discord:1001",
		"a raw id":            "1001",
		"empty":               "",
		"a prefix of the id":  "telegram:100",
		"the id with a tail":  "telegram:10011",
		"owner id in a group": "telegram:-1001",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			p := w.newProc(t0)
			prop := p.propose(sellBTC())
			_, err := decide(p, prop, prop.Nonce, true, sender)
			mustErrIs(t, err, ErrNotOwner)
			if got := p.get(prop.ID); got.Status != StatusPending || got.DecidedBy != "" {
				t.Errorf("a non-owner changed the proposal: %+v", got)
			}
			if _, err := p.eng.Get(context.Background(), WorkflowID(prop.ID)); err == nil {
				t.Error("a workflow was started")
			}
		})
	}

	t.Run("no owner configured: nobody is the owner", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		prop := p.propose(sellBTC())
		w.cfg.Proactive.Owner.SenderID = ""
		_, err := decide(p, prop, prop.Nonce, true, ownerID)
		mustErrIs(t, err, ErrNotOwner)
		_, err = decide(p, prop, prop.Nonce, true, "")
		mustErrIs(t, err, ErrNotOwner)
	})

	t.Run("the owner is matched the way checkOwnerTurn matches it", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		prop := p.propose(sellBTC())
		got, err := decide(p, prop, prop.Nonce, true, "TELEGRAM:1001")
		if err != nil {
			t.Fatalf("the owner in another case was refused: %v", err)
		}
		if got.DecidedBy != ownerID {
			t.Errorf("DecidedBy = %q, want the canonical owner id", got.DecidedBy)
		}
	})
}

func TestDecide_WrongNonce(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	for _, nonce := range []string{"0000000000000000", prop.Nonce[:15], prop.Nonce + "0", strings.ToUpper(prop.Nonce), "x"} {
		_, err := decide(p, prop, nonce, true, ownerID)
		mustErrIs(t, err, ErrNotFound)
		if err.Error() != "not found or no longer pending" {
			t.Errorf("the refusal gives detail: %q", err)
		}
	}
	if got := p.get(prop.ID); got.Status != StatusPending {
		t.Fatalf("status = %s after wrong nonces", got.Status)
	}
	if _, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil {
		t.Fatalf("the right nonce was refused: %v", err)
	}
}

func TestDecide_ExpiredFailsClosed(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	p.clock.Advance(10 * time.Minute) // exactly ExpiresAt
	_, err := decide(p, prop, prop.Nonce, true, ownerID)
	mustErrIs(t, err, ErrNotFound)
	if got := p.get(prop.ID); got.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	if w.backend.orderCount() != 0 {
		t.Error("an expired proposal placed an order")
	}
	// A minute before expiry is still good.
	prop2 := p.propose(sellBTC())
	p.clock.Advance(10*time.Minute - time.Millisecond)
	if _, err := decide(p, prop2, prop2.Nonce, false, ownerID); err != nil {
		t.Fatalf("1ms before expiry: %v", err)
	}
}

func TestDecide_UnknownAndFinishedCodes(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	_, err := p.svc.Decide(context.Background(), "ZZZZ", "", true, ownerID)
	mustErrIs(t, err, ErrNotFound)
	_, err = p.svc.Decide(context.Background(), "", "", true, ownerID)
	mustErrIs(t, err, ErrNotFound)

	prop := p.propose(sellBTC())
	if _, err := decide(p, prop, prop.Nonce, false, ownerID); err != nil {
		t.Fatal(err)
	}
	_, err = decide(p, prop, prop.Nonce, true, ownerID) // already rejected
	mustErrIs(t, err, ErrNotFound)
	if got := p.get(prop.ID); got.Status != StatusRejected {
		t.Errorf("a rejected proposal became %s", got.Status)
	}
}

func TestDecide_RejectPlacesNothing(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	got, err := decide(p, prop, prop.Nonce, false, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRejected || got.DecidedAt == nil || got.DecidedBy != ownerID || got.FinishedAt == nil {
		t.Errorf("rejected = %+v", got)
	}
	if _, err := p.eng.Get(context.Background(), WorkflowID(prop.ID)); err == nil {
		t.Error("a rejection started a workflow")
	}
	if w.backend.orderCount() != 0 || w.backend.rechecks != 0 {
		t.Error("a rejection touched the exchange")
	}
}

// The text command carries the code only: no nonce, any case, stray spaces.
func TestTextFallbackCommands(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	a := p.propose(sellBTC())
	b := p.propose(sellBTC())
	if _, err := p.svc.Decide(context.Background(), " "+strings.ToLower(a.Code)+" ", "", true, ownerID); err != nil {
		t.Fatalf("/approve %s: %v", strings.ToLower(a.Code), err)
	}
	if _, err := p.svc.Decide(context.Background(), b.Code, "", false, ownerID); err != nil {
		t.Fatalf("/reject %s: %v", b.Code, err)
	}
	if p.get(a.ID).Status != StatusExecuting || p.get(b.ID).Status != StatusRejected {
		t.Errorf("statuses = %s, %s", p.get(a.ID).Status, p.get(b.ID).Status)
	}
	// A non-owner's text command is refused like any other.
	c := p.propose(sellBTC())
	_, err := p.svc.Decide(context.Background(), c.Code, "", true, "telegram:6")
	mustErrIs(t, err, ErrNotOwner)
}

// A double tap, two channels: at most one approval, the loser told what won.
func TestDecide_DoubleApproveFromTwoGoroutines(t *testing.T) {
	w := newWorld(t)
	a, b := w.newProc(t0), w.newProc(t0) // two processes on the one database
	prop := a.propose(sellBTC())

	var wins, losers atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			svc := a.svc
			if i%2 == 1 {
				svc = b.svc
			}
			_, err := svc.Decide(context.Background(), prop.Code, prop.Nonce, true, ownerID)
			var se *StatusError
			switch {
			case err == nil:
				wins.Add(1)
			case errors.As(err, &se), errors.Is(err, ErrNotFound):
				losers.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || losers.Load() != 7 {
		t.Fatalf("%d approvals and %d refusals, want 1 and 7", wins.Load(), losers.Load())
	}
	a.run(prop.ID)
	if w.backend.orderCount() != 1 {
		t.Fatalf("%d orders after a double approval, want 1", w.backend.orderCount())
	}
}

func TestDecide_LoserOfTheRaceIsToldTheStatus(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	// Another process decided between this one's read and its update.
	if _, err := p.svc.st.decide(context.Background(), prop.ID, StatusApproved, ownerID, p.clock.Now()); err != nil {
		t.Fatal(err)
	}
	changed, err := p.svc.st.decide(context.Background(), prop.ID, StatusRejected, ownerID, p.clock.Now())
	if err != nil || changed {
		t.Fatalf("a second decision changed the row: %v %v", changed, err)
	}
}

// Approving and the sweeper racing at the edge of the expiry: the sweeper never
// touches what was approved, and a decision after the edge is refused.
func TestDecide_ApproveRacesTheSweeper(t *testing.T) {
	for i := 0; i < 25; i++ {
		w := newWorld(t)
		p := w.newProc(t0)
		prop := p.propose(sellBTC())
		p.clock.Advance(10*time.Minute - time.Millisecond)

		var wg sync.WaitGroup
		wg.Add(2)
		var decideErr error
		go func() { defer wg.Done(); _, decideErr = decide(p, prop, prop.Nonce, true, ownerID) }()
		go func() { defer wg.Done(); _, _ = p.svc.ExpireDue(context.Background()) }()
		wg.Wait()
		if decideErr != nil {
			t.Fatalf("iteration %d: a decision 1ms before expiry failed: %v", i, decideErr)
		}
		p.clock.Advance(time.Hour)
		if n, _ := p.svc.ExpireDue(context.Background()); n != 0 {
			t.Fatalf("iteration %d: the sweeper expired %d approved proposals", i, n)
		}
		if got := p.get(prop.ID); got.Status == StatusExpired {
			t.Fatalf("iteration %d: an approved proposal expired", i)
		}
	}
}

func TestExpireDue_ExpiresAndEditsTheCard(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	a, b := p.propose(sellBTC()), p.propose(sellBTC())
	if _, err := decide(p, b, b.Nonce, false, ownerID); err != nil {
		t.Fatal(err)
	}
	p.clock.Advance(10 * time.Minute)
	n, err := p.svc.ExpireDue(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1 (the rejected one is not touched)", n, err)
	}
	if p.get(a.ID).Status != StatusExpired || p.get(b.ID).Status != StatusRejected {
		t.Errorf("statuses = %s, %s", p.get(a.ID).Status, p.get(b.ID).Status)
	}
	var expiredEdit bool
	for _, e := range w.editor.all() {
		if strings.Contains(e, "EXPIRED") && strings.HasPrefix(e, "telegram/1001/m") {
			expiredEdit = true
		}
	}
	if !expiredEdit {
		t.Errorf("the expired card was not edited: %v", w.editor.all())
	}
	if n, _ := p.svc.ExpireDue(context.Background()); n != 0 {
		t.Errorf("a second sweep expired %d", n)
	}
}

func TestCardEditedOnDecision(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	a, b := p.propose(sellBTC()), p.propose(sellBTC())
	decide(p, a, a.Nonce, true, ownerID)
	decide(p, b, b.Nonce, false, ownerID)
	edits := strings.Join(w.editor.all(), "\n")
	if !strings.Contains(edits, "telegram/1001/m1: ") || !strings.Contains(edits, "APPROVED") {
		t.Errorf("approved card not edited: %q", edits)
	}
	if !strings.Contains(edits, "telegram/1001/m2: ") || !strings.Contains(edits, "REJECTED") {
		t.Errorf("rejected card not edited: %q", edits)
	}
	for _, e := range w.editor.all() {
		if !strings.Contains(e, "SELL 0.0123 BTC") {
			t.Errorf("an edited card lost its order line: %q", e)
		}
		if strings.Contains(e, a.Nonce) || strings.Contains(e, b.Nonce) {
			t.Errorf("an edited card carries a nonce: %q", e)
		}
	}
}

// A channel that cannot edit (no editor) changes nothing about the decision.
func TestDecide_WorksWithoutAnEditor(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0, func(o *Options) { o.Editor = nil })
	prop := p.propose(sellBTC())
	if _, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil {
		t.Fatal(err)
	}
	p.run(prop.ID)
	if got := p.get(prop.ID); got.Status != StatusExecuted {
		t.Fatalf("status = %s", got.Status)
	}
}

// A row whose params were changed after the fact is not decided and not executed.
func TestDecide_RefusesATamperedRow(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	if _, err := p.db.Exec(`UPDATE proposals SET params = replace(params, '"sell"', '"buy"') WHERE id = ?`, prop.ID); err != nil {
		t.Fatal(err)
	}
	_, err := decide(p, prop, prop.Nonce, true, ownerID)
	mustErrIs(t, err, ErrNotFound)
	if got := p.get(prop.ID); got.Status != StatusCancelled {
		t.Errorf("status = %s, want cancelled", got.Status)
	}
	if w.backend.orderCount() != 0 {
		t.Error("a tampered proposal placed an order")
	}
}
