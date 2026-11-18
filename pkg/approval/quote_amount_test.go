package approval

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// A proposal sized by a quote amount shows the owner both numbers, and what is
// hashed (and so approved) holds the converted base amount and the quote amount.
func TestCard_QuoteSizedOrderShowsBothAndHashesTheBaseAmount(t *testing.T) {
	p, op := cardFixture("buy", 0.000204, "BTC/USDT", MarketSpot, ModeLive)
	op.QuoteAmount = 20
	op.RefPrice, op.NotionalUSD = 98000, 19.99

	for lang, want := range map[string][]string{
		"en": {"BUY 0.000204 BTC", "sized from 20 USDT"},
		"th": {"ซื้อ 0.000204 BTC", "จากมูลค่า 20 USDT"},
	} {
		card, err := RenderOrderCard(p, op, Locale{Lang: lang, Loc: time.UTC}, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if !strings.Contains(card.Title, w) {
				t.Errorf("%s: headline lacks %q: %q", lang, w, card.Title)
			}
		}
	}

	raw, err := canonicalOrder(op)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["amount"] != 0.000204 || got["quote_amount"] != 20.0 {
		t.Errorf("hashed params = %s, want amount 0.000204 and quote_amount 20", raw)
	}
	// The stored form decodes back to the same params (the hash check at execution).
	prop := Proposal{Kind: KindOrder, Params: raw, Hash: hashOf(KindOrder, raw)}
	back, err := decodeVerified(prop)
	if err != nil || back.Amount != 0.000204 || back.QuoteAmount != 20 {
		t.Errorf("decodeVerified = %+v, %v", back, err)
	}

	// An order not sized by a quote amount renders and hashes exactly as before.
	_, plain := cardFixture("buy", 0.000204, "BTC/USDT", MarketSpot, ModeLive)
	card, _ := RenderOrderCard(p, plain, Locale{Lang: "en", Loc: time.UTC}, "")
	if strings.Contains(card.Text(), "sized from") {
		t.Errorf("a plain order mentions a quote size: %s", card.Text())
	}
	rawPlain, _ := canonicalOrder(plain)
	if strings.Contains(string(rawPlain), "quote_amount") {
		t.Errorf("a plain order's params carry quote_amount: %s", rawPlain)
	}
}

func TestValidateOrderParams_QuoteAmount(t *testing.T) {
	for name, mutate := range map[string]func(*OrderParams){
		"negative":     func(p *OrderParams) { p.QuoteAmount = -1 },
		"NaN":          func(p *OrderParams) { p.QuoteAmount = math.NaN() },
		"Inf":          func(p *OrderParams) { p.QuoteAmount = math.Inf(1) },
		"on a futures": func(p *OrderParams) { p.Market, p.Symbol, p.QuoteAmount = MarketFutures, "BTC/USDT:USDT", 20 },
	} {
		_, op := cardFixture("buy", 0.000204, "BTC/USDT", MarketSpot, ModeLive)
		mutate(&op)
		if _, err := canonicalOrder(op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
