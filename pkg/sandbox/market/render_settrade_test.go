package market

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Settrade's wire symbol is the bare ticker and its quote is THB; nothing else reads as one.
func TestSettradeSymbols(t *testing.T) {
	p := Pair{"PTT", "THB"}
	if w := Wire("settrade", p, false); w != "PTT" {
		t.Errorf("Wire = %q, want PTT", w)
	}
	if w := Wire("settrade", Pair{"PTT", "USD"}, false); w != "" {
		t.Errorf("Wire of a USD pair = %q, want none", w)
	}
	if got, swap, ok := ParseWire("settrade", "PTT"); !ok || swap || got != p {
		t.Errorf("ParseWire(PTT) = %v %v %v", got, swap, ok)
	}
	for _, bad := range []string{"", "ptt", "BTC-USDT", "BTC/THB", "BTC_THB", "A B"} {
		if _, _, ok := ParseWire("settrade", bad); ok {
			t.Errorf("ParseWire(%q) succeeded", bad)
		}
	}
	// A bare ticker is not guessed at for the wildcard: it would parse for every venue.
	if _, err := ParseSymbol("*", "PTT"); err == nil {
		t.Error("ParseSymbol(*, PTT) succeeded")
	}
	if pr, err := ParseSymbol("settrade", "PTT"); err != nil || pr != p {
		t.Errorf("ParseSymbol(settrade, PTT) = %v, %v", pr, err)
	}
}

// The quote is the book's price: the last, the 24 h range and the change; a symbol the book
// has no model for falls through to what is behind the market stage.
func TestSettradeQuote(t *testing.T) {
	clk := &fakeNow{t: time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)}
	b := NewBook(Options{Now: clk.Now})
	if err := b.Set("settrade", Pair{"PTT", "THB"}, 30); err != nil {
		t.Fatal(err)
	}
	var fell []string
	h := b.Middleware()(sandbox.ResponderFunc(func(v, m, path string, _ *http.Request) (*sandbox.Response, bool) {
		fell = append(fell, v+path)
		return nil, false
	}))
	get := func(path string) (*sandbox.Response, bool) {
		return h.Respond("settrade", "GET", path, httptest.NewRequest("GET", path, nil))
	}

	resp, ok := get("/api/marketdata/v3/SBXBROKER/quote/PTT")
	if !ok || resp.Status != 200 {
		t.Fatalf("quote = %v %+v", ok, resp)
	}
	var q map[string]float64
	_ = json.Unmarshal(resp.Body, &q)
	if q["last"] != 30 || q["high"] != 30 || q["low"] != 30 || q["change"] != 0 {
		t.Errorf("quote = %s, want a flat 30", resp.Body)
	}
	for _, path := range []string{"/api/marketdata/v3/SBXBROKER/quote/NOPE", "/api/marketdata/v3/SBXBROKER/quote/PTT/extra", "/api/marketdata/v3/SBXBROKER/candle/PTT", "/api/marketdata/v3//quote/PTT"} {
		if _, ok := get(path); ok {
			t.Errorf("%s was answered by the book", path)
		}
	}
	if len(fell) != 4 {
		t.Errorf("%d requests fell through, want the 4 the book has no model for: %v", len(fell), fell)
	}
}
