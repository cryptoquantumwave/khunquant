package market

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Webull's wire symbol is the bare ticker and its quote is USD, like Settrade's with THB.
func TestWebullSymbols(t *testing.T) {
	p := Pair{"AAPL", "USD"}
	if Wire("webull", p, false) != "AAPL" || Wire("webull", Pair{"AAPL", "THB"}, false) != "" {
		t.Error("Wire does not map AAPL/USD to AAPL and nothing else")
	}
	if got, _, ok := ParseWire("webull", "AAPL"); !ok || got != p {
		t.Errorf("ParseWire(AAPL) = %v %v", got, ok)
	}
	if _, _, ok := ParseWire("webull", "aapl"); ok {
		t.Error("a lower-case ticker was read")
	}
	// The same pair priced on one venue is not priced on the other: PTT is not an American stock.
	b := NewBook(Options{Now: time.Now})
	if err := b.Set("webull", p, 150); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.MarkPrice("settrade", "AAPL"); ok {
		t.Error("settrade priced AAPL from webull's model")
	}
	if px, ok := b.MarkPrice("webull", "AAPL"); !ok || px != 150 {
		t.Errorf("webull mark = %v %v", px, ok)
	}
}

// The snapshot is the book's price for the modelled symbols, strings all of them; with none
// modelled it falls through to the recorded fixture.
func TestWebullSnapshot(t *testing.T) {
	clk := &fakeNow{t: time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)}
	b := NewBook(Options{Now: clk.Now})
	if err := b.Set("webull", Pair{"AAPL", "USD"}, 150); err != nil {
		t.Fatal(err)
	}
	fell := 0
	h := b.Middleware()(sandbox.ResponderFunc(func(string, string, string, *http.Request) (*sandbox.Response, bool) {
		fell++
		return nil, false
	}))
	get := func(q string) (*sandbox.Response, bool) {
		path := "/openapi/market-data/stock/snapshot"
		return h.Respond("webull", "GET", path, httptest.NewRequest("GET", path+"?"+q, nil))
	}
	resp, ok := get("category=US_STOCK&symbols=AAPL,NOPE")
	if !ok || resp.Status != 200 {
		t.Fatalf("snapshot = %v %+v", ok, resp)
	}
	var rows []map[string]any
	if err := json.Unmarshal(resp.Body, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("rows = %s (%v), want the modelled symbol only", resp.Body, err)
	}
	if rows[0]["symbol"] != "AAPL" || rows[0]["price"] != "150" || rows[0]["close"] != "150" {
		t.Errorf("row = %v, want AAPL at 150, every number a string", rows[0])
	}
	if _, ok := get("category=US_ETF&symbols=SPY"); ok || fell != 1 {
		t.Errorf("an unmodelled symbol was answered by the book (fell through %d times)", fell)
	}
	if _, ok := get("category=US_STOCK"); ok {
		t.Error("a request with no symbols was answered by the book")
	}
}
