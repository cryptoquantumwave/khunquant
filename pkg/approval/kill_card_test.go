package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// While the kill switch is engaged only an order that reduces risk may be approved;
// a rejection is never blocked and a refused approval leaves the proposal pending.
func TestDecide_KillFlag(t *testing.T) {
	t.Run("a risk-increasing order is refused and stays pending", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = false
		buy := sellBTC()
		buy.Side = "buy"
		prop := p.propose(buy)
		if prop.Reducing || prop.Summary == "" {
			t.Fatal("the test needs a proposal that does not reduce risk")
		}
		w.killer.on = true
		_, err := decide(p, prop, prop.Nonce, true, ownerID)
		mustErrIs(t, err, ErrKilled)
		if got := p.get(prop.ID); got.Status != StatusPending || got.DecidedBy != "" {
			t.Errorf("a refused approval changed the proposal: %+v", got)
		}
		if _, err := p.eng.Get(context.Background(), WorkflowID(prop.ID)); err == nil {
			t.Error("a workflow was started under the kill switch")
		}

		// After /resume the same proposal can still be approved.
		w.killer.on = false
		if got, err := decide(p, prop, prop.Nonce, true, ownerID); err != nil || got.Status == StatusPending {
			t.Errorf("approval after the flag was cleared: %+v, %v", got, err)
		}
	})
	t.Run("an order that reduces risk is allowed", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = true
		prop := p.propose(sellBTC())
		if !prop.Reducing {
			t.Fatal("the test needs a reducing proposal")
		}
		w.killer.on = true
		got, err := decide(p, prop, prop.Nonce, true, ownerID)
		if err != nil {
			t.Fatalf("a reducing order was refused under the kill flag: %v", err)
		}
		if got.Status == StatusPending || got.Status == StatusRejected {
			t.Errorf("status = %s, want approved or beyond", got.Status)
		}
	})
	t.Run("a rejection is never blocked", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		w.backend.reducing = false
		prop := p.propose(sellBTC())
		w.killer.on = true
		got, err := decide(p, prop, prop.Nonce, false, ownerID)
		if err != nil || got.Status != StatusRejected {
			t.Errorf("reject under the kill flag: %+v, %v", got, err)
		}
	})
}

func TestPendingByCard(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	got := p.get(prop.ID)
	if len(got.CardMessageIDs) == 0 {
		t.Fatalf("the card recorded no message ids: %+v", got)
	}
	ctx := context.Background()
	id := got.CardMessageIDs[0]

	found, err := p.svc.PendingByCard(ctx, got.CardChannel, got.CardChatID, id)
	if err != nil || found.ID != prop.ID {
		t.Fatalf("the card's own message: %+v, %v", found, err)
	}
	if _, err := p.svc.PendingByCard(ctx, strings.ToUpper(got.CardChannel), got.CardChatID+"/77", id); err != nil {
		t.Errorf("a topic of the card's chat, channel in another case: %v", err)
	}
	for name, args := range map[string][3]string{
		"another message":  {got.CardChannel, got.CardChatID, "nope"},
		"another chat":     {got.CardChannel, "9999", id},
		"a longer chat id": {got.CardChannel, got.CardChatID + "7", id},
		"another channel":  {"discord", got.CardChatID, id},
		"an empty message": {got.CardChannel, got.CardChatID, ""},
		"an empty chat":    {got.CardChannel, "", id},
		"an empty channel": {"", got.CardChatID, id},
	} {
		if _, err := p.svc.PendingByCard(ctx, args[0], args[1], args[2]); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}

	// Once decided, the card is no longer a pending one.
	p.approve(prop)
	if _, err := p.svc.PendingByCard(ctx, got.CardChannel, got.CardChatID, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("a decided proposal's card: %v", err)
	}
}
