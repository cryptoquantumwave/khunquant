package market

import (
	"encoding/json"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// countingModel is a flat price that counts how often the book samples it.
type countingModel struct{ n atomic.Int64 }

func (m *countingModel) Price(time.Time) (float64, bool) { m.n.Add(1); return 100, true }

// A request far in the past builds only the bars the reply keeps: Binance the first limit
// bars from startTime (139-1), Bitkub the last 1000 (146-2). Since 1970 that would be about
// 20,000 daily bars of 120 samples each.
func TestKlines_PastStartBuildsOnlyTheKeptBars(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	m := &countingModel{}
	for _, sym := range []string{"BTC/USDT", "BTC/THB"} {
		if err := b.SetModel(sym, m); err != nil {
			t.Fatal(err)
		}
	}
	const perBar = 130 // a 1d bar is sampled every 12 minutes: 120 samples, plus the edges

	code, body, ok := b.binanceKlines(url.Values{"symbol": {"BTCUSDT"}, "interval": {"1d"}, "startTime": {"1"}, "limit": {"5"}})
	var rows [][]any
	if !ok || code != 200 || json.Unmarshal(body, &rows) != nil || len(rows) != 5 {
		t.Fatalf("binance klines = %d %s, want 5 rows", code, body)
	}
	if n := m.n.Load(); n > 7*perBar {
		t.Errorf("binance: %d price samples for 5 kept bars", n)
	}

	m.n.Store(0)
	code, body, ok = b.bitkubHistory(url.Values{"symbol": {"BTC_THB"}, "resolution": {"1D"}, "from": {"1"}})
	var hist struct {
		T []int64 `json:"t"`
	}
	if !ok || code != 200 || json.Unmarshal(body, &hist) != nil || len(hist.T) != maxBitkubBars {
		t.Fatalf("bitkub history = %d, %d bars, want %d", code, len(hist.T), maxBitkubBars)
	}
	if n := m.n.Load(); n > (maxBitkubBars+2)*perBar {
		t.Errorf("bitkub: %d price samples for %d kept bars", n, maxBitkubBars)
	}
}
