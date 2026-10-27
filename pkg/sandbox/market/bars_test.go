package market

import (
	"testing"
	"time"
)

func TestBook_ShockKeepsHistory(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	clk.t = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	_ = b.Set("", btc, 100)
	clk.Add(90 * time.Second) // 10:01:30, inside the 10:01 bar
	if err := b.Shock("", btc, 10); err != nil {
		t.Fatal(err)
	}
	clk.Add(10 * time.Second) // 10:01:40
	bars := b.Bars("binance", btc, time.Minute, time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), clk.Now())
	if len(bars) != 2 {
		t.Fatalf("%d bars, want 2: %+v", len(bars), bars)
	}
	if bars[0].H != 100 || bars[0].L != 100 || bars[0].C != 100 || !bars[0].Closed {
		t.Errorf("pre-shock bar = %+v", bars[0])
	}
	base, pct := 100.0, 10.0
	gap := base * (1 + pct/100)
	if bars[1].O != base || bars[1].L != base || bars[1].H != gap || bars[1].C != gap || bars[1].Closed {
		t.Errorf("shock bar = %+v, want the gap %v -> %v exactly", bars[1], base, gap)
	}
	st, ok := b.Stats24h("binance", btc)
	if !ok || st.Open != 100 || st.Low != 100 || st.High != gap || st.Last != gap {
		t.Errorf("24h stats = %+v %v, want open/low 100 and high/last %v", st, ok, gap)
	}
}

func TestBars_FormingBar(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	_ = b.Set("", btc, 98000)
	_ = b.Path("", btc, 100500, 3*time.Minute, ShapeLinear, nil)
	clk.Add(2*time.Minute + 20*time.Second)
	bars := b.Bars("binance", btc, time.Minute, clk.Now().Add(-4*time.Minute), clk.Now())
	if len(bars) < 3 {
		t.Fatalf("%d bars", len(bars))
	}
	last := bars[len(bars)-1]
	want, _ := b.Price("binance", btc, clk.Now())
	if last.Closed || last.C != want {
		t.Errorf("last bar = %+v, want forming with close %v", last, want)
	}
	for _, bar := range bars[:len(bars)-1] {
		if !bar.Closed || bar.Open.UnixMilli()%60000 != 0 {
			t.Errorf("bar %+v is not a closed, minute-aligned bar", bar)
		}
	}
	// the bar before the first op still has a price: the first model extends backwards.
	if got := b.Bars("binance", btc, time.Hour, clk.Now().Add(-30*time.Hour), clk.Now()); len(got) < 30 || got[0].C != 98000 {
		t.Errorf("history before the first op: %d bars, first %+v", len(got), got[0])
	}
	if _, ok := b.Stats24h("okx", Pair{"ETH", "USDT"}); ok {
		t.Error("an unmodelled pair has 24h stats")
	}
}
