package approval

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The card's price line says what will really happen: an order that opens or
// enlarges a position is abandoned past the collar; an exit is not.
func TestCard_PriceLineFollowsWhetherTheOrderReducesRisk(t *testing.T) {
	px := 59000.0
	for _, tc := range []struct {
		name     string
		reducing bool
		limit    bool
		want     map[string][]string // language -> fragments that must be there
		absent   map[string][]string
	}{
		{"risk-increasing: aborts past the collar", false, false,
			map[string][]string{"en": {"aborts if the price moves against the order by more than 1%"}, "th": {"ยกเลิกถ้าราคาเคลื่อนที่สวนทางกับคำสั่งเกิน 1%"}},
			map[string][]string{"en": {"MARKET price"}, "th": {"ที่ราคาตลาดแม้"}}},
		{"reducing market: executes at the market price", true, false,
			map[string][]string{
				"en": {"reduces risk", "executes at the MARKET price even if the price has moved since this card was sent"},
				"th": {"ลดความเสี่ยง", "ส่งที่ราคาตลาดแม้ราคาจะเปลี่ยนไปหลังจากส่งการ์ดนี้"}},
			map[string][]string{"en": {"aborts"}, "th": {"ยกเลิกถ้า"}}},
		{"reducing limit: its limit price or better", true, true,
			map[string][]string{"en": {"fills at its limit price or better"}, "th": {"จับคู่ที่ราคาลิมิตหรือดีกว่า"}},
			map[string][]string{"en": {"aborts"}, "th": {"ยกเลิกถ้า"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, op := cardFixture("sell", 0.0123, "BTC/USDT", MarketSpot, ModeLive)
			p.Reducing = tc.reducing
			if tc.limit {
				op.Type, op.Price = "limit", &px
			}
			for _, lang := range []string{"en", "th"} {
				card, err := RenderOrderCard(p, op, Locale{Lang: lang, Loc: time.UTC}, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range tc.want[lang] {
					if !strings.Contains(card.Body, w) {
						t.Errorf("%s: the card lacks %q: %q", lang, w, card.Body)
					}
				}
				for _, w := range tc.absent[lang] {
					if strings.Contains(card.Body, w) {
						t.Errorf("%s: the card says %q although that is not what happens: %q", lang, w, card.Body)
					}
				}
				if !strings.Contains(card.Body, "60312") {
					t.Errorf("%s: the reference price is not shown: %q", lang, card.Body)
				}
			}
		})
	}
}

// What Propose stores is what the card was rendered with, so an edited card keeps its line.
func TestPropose_StoresWhetherTheOrderReduces(t *testing.T) {
	for _, reducing := range []bool{true, false} {
		w := newWorld(t)
		w.backend.reducing = reducing
		p := w.newProc(t0)
		prop, err := p.svc.Propose(context.Background(), ownerReq(sellBTC()))
		if err != nil {
			t.Fatal(err)
		}
		if got := p.get(prop.ID); got.Reducing != reducing {
			t.Errorf("stored Reducing = %v, want %v", got.Reducing, reducing)
		}
		title := w.notifier.last()
		hasExit := strings.Contains(title, "executes at the MARKET price")
		if hasExit != reducing {
			t.Errorf("reducing=%v but the card's exit line is present=%v: %q", reducing, hasExit, title)
		}
	}
}

// The receipt states the price the order got against the reference.
func TestReceipt_StatesThePriceItGot(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p)
	w.backend.fetchFill = &Fill{Known: true, Status: "closed", Filled: 0.0123, AvgPrice: 57000, Done: true}
	p.run(prop.ID)
	got := p.get(prop.ID)
	for _, want := range []string{"Got an average price of 57000 against the reference 60000 (-5.00%)"} {
		if !strings.Contains(got.Outcome, want) {
			t.Errorf("receipt lacks %q: %q", want, got.Outcome)
		}
	}
}

// An approved exit whose order no longer only reduces risk is not the exit the
// card promised, and gets the collar rather than the exemption.
func TestExecute_ExitThatStoppedReducingIsRefused(t *testing.T) {
	w := newWorld(t)
	p := w.newProc(t0)
	prop := approved(t, p) // proposed as reducing
	w.backend.reducing = false
	p.run(prop.ID)
	got := p.get(prop.ID)
	if got.Status != StatusFailed || w.backend.orderCount() != 0 || !strings.Contains(got.Outcome, "no longer only reduces risk") {
		t.Fatalf("%s, %d orders: %q", got.Status, w.backend.orderCount(), got.Outcome)
	}
}
