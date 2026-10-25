package approval

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func cardFixture(side string, amount float64, symbol, market, mode string) (Proposal, OrderParams) {
	op := OrderParams{
		Provider: "okx", Account: "main", Market: market, Symbol: symbol, Side: side, Type: "market", Amount: amount,
		RefPrice: 60312, NotionalUSD: 741.8, PriceCollarPct: 1,
	}
	p := Proposal{
		Code: "7F3K", Nonce: "00112233aabbccdd", Rationale: "protect the gain", Mode: mode,
		CreatedAt: time.Date(2026, 10, 14, 14, 22, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 10, 14, 14, 32, 0, 0, time.UTC),
	}
	return p, op
}

// The numbers and the side word of the card are checked, in both languages, against
// the params - not read by eye.
func TestCard_ThaiAndEnglishTemplatesAreChecked(t *testing.T) {
	type tc struct {
		side, market, symbol string
		amount               float64
	}
	for _, c := range []tc{
		{"sell", MarketSpot, "BTC/USDT", 0.0123},
		{"buy", MarketSpot, "ETH/USDT", 1.5},
		{"sell", MarketFutures, "CHZ/USDT:USDT", 135},
		{"buy", MarketSpot, "BTC/THB", 0.00000001},
	} {
		p, op := cardFixture(c.side, c.amount, c.symbol, c.market, ModeLive)
		var cards = map[string]Card{}
		for _, lang := range []string{"en", "th"} {
			card, err := RenderOrderCard(p, op, Locale{Lang: lang, Loc: time.UTC}, "")
			if err != nil {
				t.Fatal(err)
			}
			cards[lang] = card
			head := card.Title
			wantSide := map[string]map[string]string{
				"en": {"buy": "BUY", "sell": "SELL"}, "th": {"buy": "ซื้อ", "sell": "ขาย"},
			}[lang]
			otherSide := wantSide["sell"]
			if c.side == "sell" {
				otherSide = wantSide["buy"]
			}
			if !strings.Contains(head, wantSide[c.side]) {
				t.Errorf("%s %s: headline lacks its side word %q: %q", lang, c.side, wantSide[c.side], head)
			}
			if strings.Contains(card.Text(), otherSide) {
				t.Errorf("%s %s: the card contains the opposite side word %q: %q", lang, c.side, otherSide, card.Text())
			}
			for _, want := range []string{
				strconvF(c.amount), "okx/main", "7F3K", "60312", "741.80", "14:32", "10", " 1%",
			} {
				if !strings.Contains(card.Text(), want) {
					t.Errorf("%s: card lacks %q: %q", lang, want, card.Text())
				}
			}
			if strings.Contains(card.Text(), "protect the gain") && !strings.Contains(card.Body, "protect the gain") {
				t.Errorf("%s: the rationale is not in the body", lang)
			}
			if c.market == MarketFutures && !strings.Contains(head, c.symbol) {
				t.Errorf("%s: a futures card does not name the contract symbol: %q", lang, head)
			}
		}
		// Every digit sequence of the English card is in the Thai one: same facts.
		numRE := regexp.MustCompile(`[0-9][0-9.:]*`)
		th := cards["th"].Text()
		for _, n := range numRE.FindAllString(cards["en"].Text(), -1) {
			if !strings.Contains(th, strings.TrimRight(n, ".")) {
				t.Errorf("%s %s: number %q is in the English card but not the Thai one", c.side, c.symbol, n)
			}
		}
	}
}

func strconvF(v float64) string { return num(v) }

func TestCard_LimitPriceModeAndReduceOnly(t *testing.T) {
	p, op := cardFixture("sell", 2, "BTC/USDT:USDT", MarketFutures, ModeRealExit)
	op.Type, op.ReduceOnly = "limit", true
	px := 61000.5
	op.Price = &px
	for lang, wantRO := range map[string]string{"en": "reduce-only", "th": "ลดสถานะเท่านั้น"} {
		card, err := RenderOrderCard(p, op, Locale{Lang: lang}, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(card.Title, wantRO) || !strings.Contains(card.Title, "@ 61000.5") {
			t.Errorf("%s: %q", lang, card.Title)
		}
		if !strings.HasPrefix(card.Title, "REAL") && !strings.Contains(card.Title[:30], "REAL") && !strings.Contains(card.Title[:40], "REAL") {
			t.Errorf("%s: a real exit under paper mode does not say REAL in its first line: %q", lang, card.Title)
		}
		if strings.Contains(card.Title, "PAPER") {
			t.Errorf("%s: a real exit says PAPER: %q", lang, card.Title)
		}
	}
	p.Mode = ModePaper
	card, _ := RenderOrderCard(p, op, Locale{Lang: "en"}, "")
	if !strings.HasPrefix(card.Title, "PAPER") {
		t.Errorf("a simulated order does not start with PAPER: %q", card.Title)
	}
}

func TestCard_OutcomeAndLocaleFallbacks(t *testing.T) {
	p, op := cardFixture("sell", 1, "BTC/USDT", MarketSpot, ModeLive)
	card, err := RenderOrderCard(p, op, Locale{Lang: "fr"}, OutcomeText("fr", StatusRejected))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card.Text(), "REJECTED - nothing was placed.") || !strings.Contains(card.Title, "Proposal 7F3K") {
		t.Errorf("an unknown language must fall back to English: %q", card.Text())
	}
	if OutcomeText("en", StatusPending) != "" {
		t.Error("a pending proposal has no outcome line")
	}
	if got := card.Actions[0].Payload; got != "7F3K.00112233aabbccdd" {
		t.Errorf("payload = %q", got)
	}
	// The expiry is shown in the owner's zone.
	bkk, _ := time.LoadLocation("Asia/Bangkok")
	card, _ = RenderOrderCard(p, op, Locale{Lang: "en", Loc: bkk}, "")
	if !strings.Contains(card.Text(), "21:32") {
		t.Errorf("expiry not in the owner's zone: %q", card.Text())
	}
}
