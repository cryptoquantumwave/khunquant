package approval

import (
	"context"
	"errors"
	"testing"
)

// protectiveClose is the DN-recover close: closeRequest with the key only code sets.
func protectiveClose(n int) ProposeRequest {
	r := closeRequest()
	r.Order.Amount += float64(n) / 1000
	r.ProtectiveKey = "protect:dn_recover:1"
	return r
}

// A model turn's proactive proposals are exits too. MaxPendingExit of them pending must not
// keep the protective close out (the same S2 with another filler).
func TestHYP4_PendingProactiveExitsDoNotBlockProtectiveClose(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, 0)
	for i := 0; i < MaxPendingExit; i++ {
		req := closeRequest()
		req.Order.Amount += float64(i+10) / 1000
		if _, err := p.svc.Propose(context.Background(), req); err != nil {
			t.Fatalf("proactive exit %d: %v", i, err)
		}
	}
	if _, err := p.svc.Propose(context.Background(), closeRequest()); err == nil {
		t.Fatal("a fourth proactive exit without the key must still be refused")
	}
	got, err := p.svc.Propose(context.Background(), protectiveClose(0))
	if err != nil {
		t.Fatalf("the protective close was refused behind %d pending exits: %v", MaxPendingExit, err)
	}
	if !got.Reducing || got.IntentID != "protect:dn_recover:1" {
		t.Errorf("close = %+v", got)
	}
}

// The hourly form: MaxExitPerHour exits created in the hour do not count against it.
func TestHYP4_ProactiveExitsInTheHourDoNotBlockProtectiveClose(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, 0)
	for i := 0; i < MaxExitPerHour; i++ {
		req := closeRequest()
		req.Order.Amount += float64(i+10) / 1000
		prop, err := p.svc.Propose(context.Background(), req)
		if err != nil {
			t.Fatalf("exit %d: %v", i, err)
		}
		if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, false, ownerID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.svc.Propose(context.Background(), protectiveClose(0)); err != nil {
		t.Fatalf("the protective close was refused after %d exits in the hour: %v", MaxExitPerHour, err)
	}
}

// The protective class keeps a bound of its own against runaway repeats.
func TestHYP4_ProtectiveBoundStillHolds(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, 0)
	for i := 0; i < MaxPendingProtective; i++ {
		if _, err := p.svc.Propose(context.Background(), protectiveClose(i)); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	_, err := p.svc.Propose(context.Background(), protectiveClose(99))
	var pe *PendingError
	if !errors.As(err, &pe) {
		t.Fatalf("a fourth pending protective close: %v, want a PendingError", err)
	}
}

// The key is honoured only for a proactive, reducing order: an owner proposal that carries
// it is an ordinary exit.
func TestHYP4_ProtectiveKeyIgnoredForOtherOrigins(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	w.fillOwnerBuys(p, 0)
	req := ownerReq(ownerBuy(1))
	req.ProtectiveKey = "protect:dn_recover:1"
	got, err := p.svc.Propose(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.IntentID != "" {
		t.Errorf("an owner proposal was stored as protective: %q", got.IntentID)
	}
}
