package lab

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

var ptt = market.Pair{Base: "PTT", Quote: "THB"}

var bangkokZone = time.FixedZone("Asia/Bangkok", 7*3600)

// bkkAt is a time on Wednesday 2026-03-11 in Bangkok (the clock starts Tuesday 16:00 there).
func bkkAt(h, m int) time.Time { return time.Date(2026, 3, 11, h, m, 0, 0, bangkokZone) }

// holdSecond makes the second order create be accepted and never filled (accept_no_fill).
type holdSecond struct {
	mu sync.Mutex
	n  int
}

func (h *holdSecond) OrderEffect(op sandbox.OrderOp, _ sandbox.OrderInfo) (sandbox.OrderEffect, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if op != sandbox.OrderCreate {
		return sandbox.OrderEffect{}, false
	}
	h.n++
	return sandbox.OrderEffect{Kind: "accept_no_fill"}, h.n == 2
}

// The Settrade auctions and the Day end are instants the simulator asks the clock to stop
// at, like a funding settlement: with nothing polling, the clock stops at 14:30 (the ATO
// fills at the price of that instant), at 16:40 (the resting order ends) and at its target,
// and at no other time, because an ATO and a held order are not watched by the minute.
func TestSettrade_AuctionAndDayEndWakeTheClock(t *testing.T) {
	d := newDerivLab(t)
	d.Sim.SetOrderEffects(&holdSecond{})
	if err := d.Market.Set("", ptt, 30); err != nil {
		t.Fatal(err)
	}
	d.advance(t, "20h") // Wednesday 12:00 in Bangkok: between the sessions
	if now := d.Now(); !now.Equal(bkkAt(12, 0)) {
		t.Fatalf("the clock is at %v, want Wednesday 12:00 Bangkok", now)
	}
	if err := d.Sim.Mutate("settrade", func(st *sandbox.VenueState) error {
		st.Balances["THB"] = sandbox.Balance{Free: 1_000_000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if d.Clock.Pending() != 0 {
		t.Fatalf("%d timers before any order", d.Clock.Pending())
	}

	place := func(body string) {
		t.Helper()
		req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
		resp, ok := d.Sim.Respond("settrade", "POST", "/api/seos/v3/B/accounts/A/orders", req)
		if !ok || resp.Status != 200 {
			t.Fatalf("order %s: %v %+v", body, ok, resp)
		}
	}
	place(`{"side":"Buy","symbol":"PTT","volume":200,"priceType":"ATO","validityType":"Day"}`)
	if at, ok := d.Clock.NextAt(); !ok || !at.Equal(bkkAt(14, 30)) || d.Clock.Pending() != 1 {
		t.Fatalf("after the ATO: next %v with %d timers, want the one 14:30 auction", at, d.Clock.Pending())
	}
	place(`{"side":"Buy","symbol":"PTT","volume":100,"price":20,"priceType":"Limit","validityType":"Day"}`)
	if d.Clock.Pending() != 1 {
		t.Fatalf("%d timers after the held order, want the one still", d.Clock.Pending())
	}

	if err := d.Market.Path("", ptt, 33, 4*time.Hour, market.ShapeLinear, nil); err != nil { // 30 -> 33 over 12:00-16:00
		t.Fatal(err)
	}
	sum := d.advance(t, "12h")
	if steps := sum["steps"].(float64); steps != 3 {
		t.Errorf("%v clock steps, want 3: 14:30, 16:40 and the target", steps)
	}
	_, orders := d.journaled()
	var fill, cancel *sandbox.OrderEntry
	for i := range orders {
		switch o := &orders[i]; o.Event {
		case "fill":
			fill = o
		case "cancel":
			cancel = o
		}
	}
	px, _ := d.Market.Price("settrade", ptt, bkkAt(14, 30))
	if fill == nil || !fill.At.Equal(bkkAt(14, 30)) || fill.Symbol != "PTT/THB" || fill.Filled != 200 || fill.Avg != px || px <= 30 {
		t.Errorf("fill = %+v at price %v, want 200 PTT at the 14:30 price", fill, px)
	}
	if cancel == nil || !cancel.At.Equal(bkkAt(16, 40)) || cancel.Amount != 100 {
		t.Errorf("cancel = %+v, want the held limit ended at 16:40", cancel)
	}
	if d.Clock.Pending() != 0 {
		t.Errorf("%d timers armed after the day, want none", d.Clock.Pending())
	}
}
