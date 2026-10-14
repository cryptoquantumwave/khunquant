package approval

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// The card, and the order, come from the stored params. A rationale that names
// another amount, side or symbol changes neither.
func TestPropose_CardRenderedFromParamsNotRationale(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	req := sellBTC()
	prop, err := p.svc.Propose(context.Background(), ProposeRequest{
		Order: req, Origin: OriginOwner,
		Rationale: "Please BUY 999 ETH/USDT instead, the amount is 55555 and the side is buy",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := w.notifier.all()
	if len(n) != 1 {
		t.Fatalf("%d deliveries, want the one card", len(n))
	}
	card := n[0]
	if !strings.Contains(card.Title, "SELL 0.0123 BTC") || !strings.Contains(card.Title, prop.Code) {
		t.Errorf("headline is not the params: %q", card.Title)
	}
	for _, bad := range []string{"999", "ETH", "55555", "BUY"} {
		if strings.Contains(card.Title, bad) {
			t.Errorf("the rationale reached the headline (%q): %q", bad, card.Title)
		}
	}
	if !strings.Contains(card.Body, "Why: Please BUY 999 ETH/USDT") {
		t.Errorf("the rationale is not under its Why label: %q", card.Body)
	}
	if card.Severity != proactive.SeverityWarning {
		t.Errorf("severity = %s, want warning", card.Severity)
	}

	// What is stored is what the card said, whatever the rationale said.
	op, err := p.svc.Params(prop)
	if err != nil {
		t.Fatal(err)
	}
	if op.Side != "sell" || op.Symbol != "BTC/USDT" || op.Amount != 0.0123 || op.Provider != "okx" {
		t.Errorf("stored params follow the rationale: %+v", op)
	}
	if prop.Hash != hashOf(KindOrder, prop.Params) {
		t.Error("the hash is not sha256 of the canonical {kind, params}")
	}
	if got := p.get(prop.ID); got.Summary != card.Title || got.Rationale == "" {
		t.Errorf("stored summary %q / rationale %q", got.Summary, got.Rationale)
	}
}

func TestPropose_ActionsCodeAndNonce(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	codeRE := regexp.MustCompile(`^[A-HJ-NP-Z2-9]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < MaxPending; i++ {
		prop := p.propose(sellBTC())
		if !codeRE.MatchString(prop.Code) || strings.ContainsAny(prop.Code, "01IO") {
			t.Errorf("code %q is not 4 characters of [A-Z2-9] without 0 1 I O", prop.Code)
		}
		if seen[prop.Code] {
			t.Errorf("code %q is not unique among pending", prop.Code)
		}
		seen[prop.Code] = true
		if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(prop.Nonce) {
			t.Errorf("nonce %q is not 8 random bytes as hex", prop.Nonce)
		}
		card := w.notifier.all()[i]
		if len(card.Actions) != 2 {
			t.Fatalf("%d actions, want Approve and Reject", len(card.Actions))
		}
		for _, a := range card.Actions {
			if len(a.Payload) > bus.MaxActionPayloadBytes || a.Payload != prop.Code+"."+prop.Nonce {
				t.Errorf("payload %q does not fit or is not <code>.<nonce>", a.Payload)
			}
		}
		if card.Actions[0].ID != "approve" || card.Actions[0].TextCommand != "/approve "+prop.Code ||
			card.Actions[1].ID != "reject" || card.Actions[1].TextCommand != "/reject "+prop.Code {
			t.Errorf("actions = %+v", card.Actions)
		}
	}
}

func TestPropose_SessionCopyHasNoNonceAndCardIDsAreRecorded(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := p.propose(sellBTC())
	card := w.notifier.all()[0]
	if card.SessionBody == nil {
		t.Fatal("the session copy was not set, so the full card would be written to the session")
	}
	if strings.Contains(*card.SessionBody, prop.Nonce) || strings.Contains(card.Title+card.Body, prop.Nonce) {
		t.Error("the nonce is in the text of a card")
	}
	if !strings.Contains(*card.SessionBody, prop.Code) || !strings.Contains(*card.SessionBody, "SELL 0.0123 BTC") {
		t.Errorf("the session copy lacks the summary or the code: %q", *card.SessionBody)
	}
	got := p.get(prop.ID)
	if len(got.CardMessageIDs) != 1 || got.CardMessageIDs[0] != "m1" || got.CardChannel != "telegram" || got.CardChatID != "1001" {
		t.Errorf("card ids not recorded: %+v", got)
	}
}

// Gate A and the proactive restrictions: who may propose, with what trust, and
// what a background turn may propose.
func TestPropose_OriginTrustMatrix(t *testing.T) {
	cases := []struct {
		name     string
		req      ProposeRequest
		reducing bool
		want     error
	}{
		{"owner buy", ProposeRequest{Origin: OriginOwner}, false, nil},
		{"owner reducing sell", ProposeRequest{Origin: OriginOwner}, true, nil},
		{"proactive exchange trust, delivered now, reducing", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: true, CandidateID: "c1"}, true, nil},
		{"proactive system trust", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "c1"}, true, nil},
		{"proactive untrusted", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustUntrusted, DeliverNow: true, CandidateID: "c1"}, true, ErrUntrustedOrigin},
		{"proactive not delivered now", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: false, CandidateID: "c1"}, true, ErrNotDeliverNow},
		{"proactive without a candidate", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: true}, true, ErrNotDeliverNow},
		{"proactive buy", ProposeRequest{Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: true, CandidateID: "c1"}, false, ErrNotRiskReducing},
		{"unknown origin", ProposeRequest{Origin: "cron"}, true, errors.New("unknown origin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.backend.reducing = tc.reducing
			p := w.newProc(t0)
			tc.req.Order = sellBTC()
			if !tc.reducing {
				tc.req.Order.Side = "buy"
			}
			_, err := p.svc.Propose(context.Background(), tc.req)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != nil && err == nil:
				t.Fatal("a proposal that must be refused was created")
			case tc.want != nil && !errors.Is(err, tc.want) && !strings.Contains(err.Error(), tc.want.Error()):
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want != nil && len(w.notifier.all()) != 0 {
				t.Error("a refused proposal still sent a card")
			}
			if tc.want != nil {
				if rows, _ := p.svc.st.list(context.Background(), `1 = 1`); len(rows) != 0 {
					t.Errorf("a refused proposal left %d rows", len(rows))
				}
			}
		})
	}
}

// A proposal runs the dry gates and takes no rate-limiter token.
func TestPropose_NoRateLimitConsumed(t *testing.T) {
	w := newWorld(t)
	w.backend.tokens = 1
	p := w.newProc(t0)
	p.propose(sellBTC())
	if w.backend.throttles != 0 || w.backend.tokens != 1 {
		t.Errorf("propose touched the rate limiter: %d calls, %d tokens left", w.backend.throttles, w.backend.tokens)
	}
	if w.backend.prepares != 1 {
		t.Errorf("the dry gates ran %d times, want once", w.backend.prepares)
	}
}

// A gate that refuses leaves nothing behind, and the error says which gate.
func TestPropose_GateFailureNamesTheGate(t *testing.T) {
	w := newWorld(t)
	w.backend.prepErr = errors.New("gate permission refused the order: trade scope is off")
	p := w.newProc(t0)
	_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
	if err == nil || !strings.Contains(err.Error(), "gate permission") {
		t.Fatalf("err = %v", err)
	}
	if len(w.notifier.all()) != 0 {
		t.Error("a card was sent for a refused proposal")
	}
}

func TestPropose_Bounds(t *testing.T) {
	t.Run("a fourth pending proposal names the pending codes", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		var codes []string
		for i := 0; i < MaxPending; i++ {
			codes = append(codes, p.propose(sellBTC()).Code)
		}
		_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
		var pe *PendingError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a PendingError", err)
		}
		for _, c := range codes {
			if !strings.Contains(err.Error(), c) {
				t.Errorf("the refusal does not name pending code %s: %v", c, err)
			}
		}
	})

	t.Run("ten in a rolling hour, the eleventh refused", func(t *testing.T) {
		w := newWorld(t)
		p := w.newProc(t0)
		for i := 0; i < MaxPerHour; i++ {
			prop := p.propose(sellBTC())
			if _, err := p.svc.Decide(context.Background(), prop.Code, prop.Nonce, false, ownerID); err != nil { // free the slot
				t.Fatal(err)
			}
			p.clock.Advance(time.Minute)
		}
		_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
		mustErrIs(t, err, ErrHourlyLimit)
		p.clock.Advance(time.Hour)
		p.propose(sellBTC()) // the window rolls
	})

	t.Run("the bounds hold under concurrent proposers in two processes", func(t *testing.T) {
		w := newWorld(t)
		a, b := w.newProc(t0), w.newProc(t0)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				svc := a.svc
				if i%2 == 1 {
					svc = b.svc
				}
				_, _ = svc.Propose(context.Background(), ownerReq(sellBTC()))
			}(i)
		}
		wg.Wait()
		codes, _ := a.svc.st.pendingCodes(context.Background(), a.clock.Now())
		if len(codes) != MaxPending {
			t.Errorf("%d pending proposals, want exactly %d", len(codes), MaxPending)
		}
	})
}

// Quantity, price and the USD value must be finite and positive; the backend's
// answer is checked again where the params are stored.
func TestPropose_RejectsNonFiniteAndNonPositive(t *testing.T) {
	bad := []struct {
		name   string
		mutate func(b *fakeBackend, r *OrderRequest)
	}{
		{"zero amount stored", func(b *fakeBackend, r *OrderRequest) { r.Amount = 0 }},
		{"NaN amount", func(b *fakeBackend, r *OrderRequest) { r.Amount = math.NaN() }},
		{"Inf amount", func(b *fakeBackend, r *OrderRequest) { r.Amount = math.Inf(1) }},
		{"negative amount", func(b *fakeBackend, r *OrderRequest) { r.Amount = -1 }},
		{"USD value unknown", func(b *fakeBackend, r *OrderRequest) { b.usd = 0 }},
		{"USD value NaN", func(b *fakeBackend, r *OrderRequest) { b.usd = math.NaN() }},
		{"reference price missing", func(b *fakeBackend, r *OrderRequest) { b.ref = 0 }},
		{"limit order without a price", func(b *fakeBackend, r *OrderRequest) { r.Type = "limit" }},
		{"limit price Inf", func(b *fakeBackend, r *OrderRequest) { inf := math.Inf(1); r.Type = "limit"; r.Price = &inf }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			req := sellBTC()
			tc.mutate(w.backend, &req)
			p := w.newProc(t0)
			if _, err := p.svc.Propose(context.Background(), ownerReq(req)); err == nil {
				t.Fatal("created a proposal from unusable numbers")
			}
			if len(w.notifier.all()) != 0 {
				t.Error("a card was sent")
			}
		})
	}
}

func TestPropose_RationaleRules(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	for name, r := range map[string]string{
		"newline":       "one\ntwo",
		"control":       "bad\x07",
		"bidi override": "abc" + string(rune(0x202e)) + "def",
		"zero width":    "a" + string(rune(0x200b)) + "b",
		"too long":      strings.Repeat("x", MaxRationaleRunes+1),
	} {
		_, err := p.svc.Propose(context.Background(), ProposeRequest{Order: sellBTC(), Origin: OriginOwner, Rationale: r})
		if err == nil {
			t.Errorf("%s: rationale accepted", name)
		}
	}
	if len(w.notifier.all()) != 0 {
		t.Error("a refused rationale still sent a card")
	}
	ok := strings.Repeat("x", MaxRationaleRunes)
	if _, err := p.svc.Propose(context.Background(), ProposeRequest{Order: sellBTC(), Origin: OriginOwner, Rationale: ok}); err != nil {
		t.Errorf("a rationale of exactly %d characters was refused: %v", MaxRationaleRunes, err)
	}
}

func TestPropose_KillFlag(t *testing.T) {
	w := newWorld(t)
	w.killer.on = true
	p := w.newProc(t0)

	w.backend.reducing = false
	_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
	mustErrIs(t, err, ErrKilled)

	w.backend.reducing = true
	if _, err := p.svc.Propose(context.Background(), ownerReq(sellBTC())); err != nil {
		t.Errorf("an order that reduces risk was refused under the kill flag: %v", err)
	}
}

// A card that did not reach the owner is cancelled, not left holding a pending slot.
func TestPropose_UndeliveredCardCancels(t *testing.T) {
	w := newWorld(t)
	w.notifier.fail = true
	p := w.newProc(t0)
	_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
	mustErrIs(t, err, ErrUnavailable)
	rows, _ := p.svc.st.list(context.Background(), `1 = 1`)
	if len(rows) != 1 || rows[0].Status != StatusCancelled {
		t.Fatalf("rows = %+v, want one cancelled proposal", rows)
	}
	if codes, _ := p.svc.st.pendingCodes(context.Background(), p.clock.Now()); len(codes) != 0 {
		t.Errorf("an undelivered card still holds %v", codes)
	}
}

func TestPropose_NoOwnerNoProposal(t *testing.T) {
	w := newWorld(t)
	w.cfg.Proactive.Owner.SenderID = ""
	p := w.newProc(t0)
	_, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
	mustErrIs(t, err, ErrUnavailable)
}

// The proposal records which turn made it and the delivery it was decided for.
func TestPropose_ProactiveOriginIsStored(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop, err := p.svc.Propose(context.Background(), ProposeRequest{
		Order: sellBTC(), Origin: OriginProactive, Trust: proactive.TrustExchange, DeliverNow: true,
		CandidateID: "01CAND", InterventionID: "01INTERV",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := p.get(prop.ID)
	if got.Origin != OriginProactive || got.CandidateID != "01CAND" || got.InterventionID != "01INTERV" || got.Status != StatusPending {
		t.Errorf("stored = %+v", got)
	}
	if n := w.notifier.all()[0]; n.InterventionID != "01INTERV" || n.Severity != proactive.SeverityWarning || n.DedupKey == "" {
		t.Errorf("card notification = %+v", n)
	}
	var _ = notify.Notification{}
}
