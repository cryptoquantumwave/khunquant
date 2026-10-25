package approval

import (
	"encoding/json"
	"strings"
	"testing"
)

func storedProposal(t *testing.T) (Proposal, OrderParams) {
	t.Helper()
	_, op := cardFixture("sell", 0.0123, "BTC/USDT", MarketSpot, ModeLive)
	raw, err := canonicalOrder(op)
	if err != nil {
		t.Fatal(err)
	}
	return Proposal{Kind: KindOrder, Params: raw, Hash: hashOf(KindOrder, raw)}, op
}

func TestDecodeVerified_AcceptsTheCanonicalRow(t *testing.T) {
	p, op := storedProposal(t)
	got, err := decodeVerified(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != op.Amount || got.Side != op.Side || got.Symbol != op.Symbol {
		t.Errorf("decoded %+v", got)
	}
}

// Whatever an attacker (or a bug) does to the stored bytes, the executor refuses
// unless hash, strict decode and re-encoding all agree.
func TestDecodeVerified_RefusesAnythingThatIsNotWhatWasHashed(t *testing.T) {
	good, _ := storedProposal(t)
	rehash := func(raw string) Proposal {
		p := good
		p.Params = json.RawMessage(raw)
		p.Hash = hashOf(KindOrder, p.Params) // an attacker who also fixes the hash
		return p
	}
	canonical := string(good.Params)

	tamper := map[string]Proposal{
		"changed amount, old hash": func() Proposal {
			p := good
			p.Params = json.RawMessage(strings.Replace(canonical, `"amount":0.0123`, `"amount":123`, 1))
			return p
		}(),
		"changed kind, old hash":        func() Proposal { p := good; p.Kind = "mandate"; return p }(),
		"an extra field, hash fixed":    rehash(strings.TrimSuffix(canonical, "}") + `,"leverage":125}`),
		"a duplicate key, hash fixed":   rehash(strings.TrimSuffix(canonical, "}") + `,"side":"buy"}`),
		"reordered keys, hash fixed":    rehash(`{"symbol":"BTC/USDT",` + strings.TrimPrefix(strings.Replace(canonical, `"symbol":"BTC/USDT",`, "", 1), "{")),
		"whitespace, hash fixed":        rehash(strings.Replace(canonical, ",", ", ", 1)),
		"trailing garbage, hash fixed":  rehash(canonical + " {}"),
		"a negative amount, hash fixed": rehash(strings.Replace(canonical, `"amount":0.0123`, `"amount":-1`, 1)),
		"not JSON, hash fixed":          rehash(`not json`),
		"a zeroed hash":                 func() Proposal { p := good; p.Hash = [32]byte{}; return p }(),
	}
	for name, p := range tamper {
		if _, err := decodeVerified(p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidateOrderParams(t *testing.T) {
	_, ok := cardFixture("sell", 1, "BTC/USDT", MarketSpot, ModeLive)
	px := 100.0
	mutations := map[string]func(*OrderParams){
		"empty provider":            func(o *OrderParams) { o.Provider = " " },
		"market unknown":            func(o *OrderParams) { o.Market = "margin" },
		"side unknown":              func(o *OrderParams) { o.Side = "short" },
		"type unknown":              func(o *OrderParams) { o.Type = "stop_loss" },
		"amount zero":               func(o *OrderParams) { o.Amount = 0 },
		"collar zero":               func(o *OrderParams) { o.PriceCollarPct = 0 },
		"collar huge":               func(o *OrderParams) { o.PriceCollarPct = 90 },
		"limit without price":       func(o *OrderParams) { o.Type = "limit" },
		"market with a price":       func(o *OrderParams) { o.Price = &px },
		"spot reduce-only":          func(o *OrderParams) { o.ReduceOnly = true },
		"spot with a position side": func(o *OrderParams) { o.PositionSide = "long" },
		"futures spot-style symbol": func(o *OrderParams) { o.Market = MarketFutures },
		"spot contract symbol":      func(o *OrderParams) { o.Symbol = "CHZ/USDT:USDT" },
	}
	if err := validateOrderParams(ok); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}
	for name, m := range mutations {
		op := ok
		m(&op)
		if err := validateOrderParams(op); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCodes(t *testing.T) {
	for i := 0; i < 2000; i++ {
		c := newCode()
		if !ValidCode(c) || strings.ContainsAny(c, "01IO") {
			t.Fatalf("code %q", c)
		}
	}
	if ValidCode("0ABC") || ValidCode("ABCDE") || ValidCode("abcd") || ValidCode("") {
		t.Error("ValidCode accepts a non-code")
	}
	if code, nonce, ok := ParseButtonPayload("7F3K.00112233aabbccdd"); !ok || code != "7F3K" || nonce != "00112233aabbccdd" {
		t.Errorf("payload parse = %q %q %v", code, nonce, ok)
	}
	for _, bad := range []string{"", "7F3K", "7F3K.", ".abc", "0000.abc", "7F3K.a.b"[:0]} {
		if _, _, ok := ParseButtonPayload(bad); ok {
			t.Errorf("payload %q parsed", bad)
		}
	}
	if !nonceEqual("abc", "abc") || nonceEqual("abc", "abd") || nonceEqual("abc", "ab") || nonceEqual("", "x") {
		t.Error("nonceEqual")
	}
	if n1, n2 := newNonce(), newNonce(); len(n1) != 16 || n1 == n2 {
		t.Errorf("nonces %q %q", n1, n2)
	}
}

// A spot proposal on a contract symbol is refused at canonicalisation with a
// message the model can act on, and the same symbol is fine as a futures order.
func TestCanonicalOrder_SpotRefusesContractSymbol(t *testing.T) {
	_, spot := cardFixture("buy", 10, "CHZ/USDT:USDT", MarketSpot, ModeLive)
	_, err := canonicalOrder(spot)
	if err == nil || !strings.Contains(err.Error(), "use market=futures") {
		t.Fatalf("err = %v, want a refusal that names market=futures", err)
	}
	_, fut := cardFixture("buy", 10, "CHZ/USDT:USDT", MarketFutures, ModeLive)
	if _, err := canonicalOrder(fut); err != nil {
		t.Errorf("the same symbol as a futures order was refused: %v", err)
	}
}
