package market

import (
	"strings"
	"testing"
	"time"
)

const replayCSV = `ts,open,high,low,close,volume
1700000000,100,110,95,105,5
1700000060,105,120,100,101,7
1700000120,101,102,90,92,3
1700000180,92,95,91,94,2
`

func TestReplayCSV_PricesAndBars(t *testing.T) {
	clk := newFakeNow()
	clk.t = time.Date(2026, 1, 1, 10, 0, 20, 0, time.UTC)
	b := NewBook(Options{Now: clk.Now})
	if err := b.Set("", btc, 50); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Minute) // 10:01:20: the replay starts here, on the 10:01 boundary
	if err := b.Replay("", btc, strings.NewReader(replayCSV), time.Minute, time.Time{}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)
	near(t, "first bar at the start", price(b, "binance", clk.Now()), 100+5.0*20/60)
	near(t, "first bar mid", price(b, "binance", t0.Add(30*time.Second)), 102.5)
	near(t, "second bar open", price(b, "binance", t0.Add(time.Minute)), 105)
	near(t, "last close held", price(b, "okx", t0.Add(time.Hour)), 94)
	near(t, "history before the replay", price(b, "okx", t0.Add(-2*time.Minute)), 50)

	clk.Add(2 * time.Minute) // 10:03:20: bars 1 and 2 are done, bar 3 is forming
	bars := b.Bars("binance", btc, time.Minute, t0.Add(-time.Minute), clk.Now())
	if len(bars) != 4 {
		t.Fatalf("%d bars, want 4 (one before the replay, three played): %+v", len(bars), bars)
	}
	want := []Bar{{O: 100, H: 110, L: 95, C: 105}, {O: 105, H: 120, L: 100, C: 101}}
	for i, w := range want {
		g := bars[i+1]
		if g.O != w.O || g.H != w.H || g.L != w.L || g.C != w.C || !g.Closed || !g.Open.Equal(t0.Add(time.Duration(i)*time.Minute)) {
			t.Errorf("bar %d = %+v, want the CSV row %+v verbatim", i+1, g, w)
		}
	}
	f := bars[3]
	px := price(b, "binance", clk.Now())
	if f.Closed || f.C != px || f.O != 101 || f.L != 90 || f.H != 102 {
		t.Errorf("forming bar = %+v, want open 101, close %v (the price now) and the CSV range", f, px)
	}
	if got := b.Bars("binance", btc, 5*time.Minute, t0, clk.Now()); len(got) == 0 {
		t.Error("another timeframe must fall back to the sampled path, not vanish")
	}
}

func TestReplayCSV_StartAndErrors(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	start := time.Unix(1700000120, 0)
	if err := b.Replay("okx", btc, strings.NewReader(replayCSV), time.Minute, start); err != nil {
		t.Fatal(err)
	}
	p, _ := b.Price("okx", btc, clk.Now())
	near(t, "starts in the bar holding start", p, 101+(92-101)*float64(clk.Now().Sub(alignDown(clk.Now(), time.Minute)))/float64(time.Minute))
	if _, ok := b.Price("binance", btc, clk.Now()); ok {
		t.Error("a venue-scoped replay priced another venue")
	}
	for _, bad := range []string{"", "ts,open,high,low,close\n", "1,2,3\n", "1,10,5,4,6\n", "2,1,1,1,1\n1,1,1,1,1\n", "1,0,1,1,1\n"} {
		if err := b.Replay("okx", btc, strings.NewReader(bad), time.Minute, time.Time{}); err == nil {
			t.Errorf("csv %q was accepted", bad)
		}
	}
	if err := b.Replay("okx", btc, strings.NewReader(replayCSV), 0, time.Time{}); err == nil {
		t.Error("a zero timeframe was accepted")
	}
	if tm, err := ParseCSVTime("2026-01-01T10:00:00Z"); err != nil || tm.Unix() != 1767261600 {
		t.Errorf("RFC 3339 ts = %v (%v)", tm, err)
	}
	if tm, _ := ParseCSVTime("1700000000000"); tm.Unix() != 1700000000 {
		t.Errorf("ms ts = %v", tm)
	}
}
