package market

import (
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// The funding-rate history on both venues is the schedule and the rates the book pays
// funding with: one row per settlement up to now, the rate in force at each (a change
// takes effect from when it was made), and nothing in the future.
func TestFundingRateHistory_CCXT(t *testing.T) {
	r := newFeedRig(t)
	if err := r.book.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	t0 := r.clk.Now() // 2026-01-02 03:04:05 UTC
	if err := r.book.SetFunding("", btc, 0.0001); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(30 * time.Hour) // 09:04 on the 3rd
	if err := r.book.SetFunding("", btc, -0.0003); err != nil {
		t.Fatal(err)
	}
	r.clk.Add(15 * time.Hour) // 00:04 on the 4th

	day := func(d, h int) time.Time { return time.Date(2026, 1, d, h, 0, 0, 0, time.UTC) }
	type row struct {
		at   time.Time
		rate float64
	}
	want := []row{{day(2, 8), 0.0001}, {day(2, 16), 0.0001}, {day(3, 0), 0.0001}, {day(3, 8), 0.0001}, {day(3, 16), -0.0003}, {day(4, 0), -0.0003}}
	sym, since := "BTC/USDT:USDT", ccxt.WithFetchFundingRateHistorySince(t0.UnixMilli())
	check := func(venue string, got []ccxt.FundingRateHistory, err error) {
		t.Helper()
		if err != nil || len(got) != len(want) {
			t.Fatalf("%s history = %d rows, %v, want %d", venue, len(got), err, len(want))
		}
		for i, w := range want {
			g := got[i]
			if g.Timestamp == nil || g.FundingRate == nil || *g.Timestamp != w.at.UnixMilli() || *g.FundingRate != w.rate {
				t.Errorf("%s row %d = %v %v, want %v %v", venue, i, g.Timestamp, g.FundingRate, w.at.UnixMilli(), w.rate)
			}
		}
	}
	bn := r.binance(t, map[string]any{"defaultType": "future"})
	hist, err := bn.FetchFundingRateHistory(ccxt.WithFetchFundingRateHistorySymbol(sym), since)
	check("binance", hist, err)
	hist, err = r.okx(t, nil).FetchFundingRateHistory(ccxt.WithFetchFundingRateHistorySymbol(sym), since)
	check("okx", hist, err)

	// The 08:00 settlement on the 4th is not yet behind the clock until it is.
	if hist, err = bn.FetchFundingRateHistory(ccxt.WithFetchFundingRateHistorySymbol(sym), since); err != nil || len(hist) != 6 {
		t.Fatalf("before 08:00 on the 4th: %d rows, %v", len(hist), err)
	}
	r.clk.Add(8 * time.Hour)
	want = append(want, row{day(4, 8), -0.0003})
	hist, err = bn.FetchFundingRateHistory(ccxt.WithFetchFundingRateHistorySymbol(sym), since)
	check("binance", hist, err)
	last, err := bn.FetchFundingRateHistory(ccxt.WithFetchFundingRateHistorySymbol(sym), ccxt.WithFetchFundingRateHistoryLimit(1))
	if err != nil || len(last) != 1 || *last[0].Timestamp != day(4, 8).UnixMilli() {
		t.Errorf("limit 1 with no start = %v (%v), want the latest settlement", last, err)
	}
}
