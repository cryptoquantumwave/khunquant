package market

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fakeNow is a settable clock shared by a Book and the test.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeNow() *fakeNow {
	return &fakeNow{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}
func (f *fakeNow) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeNow) Add(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

var btc = Pair{"BTC", "USDT"}

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func price(b *Book, venue string, t time.Time) float64 {
	p, _ := b.Price(venue, btc, t)
	return p
}

func TestBook_PathLinearAndStep(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Path("", btc, 100, time.Minute, ShapeLinear, nil); err == nil {
		t.Fatal("a path with no starting price must fail, not invent one")
	}
	if err := b.Set("", btc, 98000); err != nil {
		t.Fatal(err)
	}
	if err := b.Path("", btc, 100500, 3*time.Minute, ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	near(t, "at 0", price(b, "binance", t0), 98000)
	near(t, "mid", price(b, "binance", t0.Add(90*time.Second)), 99250)
	near(t, "end", price(b, "binance", t0.Add(3*time.Minute)), 100500)
	near(t, "holds", price(b, "okx", t0.Add(time.Hour)), 100500)

	clk.Add(10 * time.Minute)
	if err := b.Path("binance", btc, 90000, 100*time.Second, ShapeStep,
		[]Keyframe{{20 * time.Second, 95000}, {60 * time.Second, 92000}}); err != nil {
		t.Fatal(err)
	}
	t1 := clk.Now()
	for _, tc := range []struct {
		at   time.Duration
		want float64
	}{{0, 100500}, {19 * time.Second, 100500}, {20 * time.Second, 95000}, {59 * time.Second, 95000},
		{60 * time.Second, 92000}, {99 * time.Second, 92000}, {100 * time.Second, 90000}} {
		near(t, "step "+tc.at.String(), price(b, "binance", t1.Add(tc.at)), tc.want)
	}
	near(t, "okx keeps the wildcard", price(b, "okx", t1.Add(100*time.Second)), 100500)
	near(t, "history of the venue survives", price(b, "binance", t0.Add(90*time.Second)), 99250)
	if err := b.Path("", btc, 1, time.Minute, "zigzag", nil); err == nil {
		t.Error("an unknown shape was accepted")
	}
	if err := b.Set("kraken", btc, 1); err == nil {
		t.Error("an unknown venue was accepted")
	}
}

func TestBook_WalkDeterministic(t *testing.T) {
	series := func(seed uint64) []float64 {
		clk := newFakeNow()
		b := NewBook(Options{Now: clk.Now})
		_ = b.Set("", btc, 100)
		if err := b.Walk("", btc, WalkParams{Vol: 0.01, Seed: seed, Step: time.Minute}); err != nil {
			t.Fatal(err)
		}
		var out []float64
		for _, k := range []int{30, 5, 60, 0, 61} { // out of order on purpose
			out = append(out, price(b, "binance", clk.Now().Add(time.Duration(k)*time.Minute)))
		}
		return out
	}
	a, again, other := series(7), series(7), series(8)
	for i := range a {
		if a[i] != again[i] {
			t.Fatalf("the same seed diverged at %d: %v vs %v", i, a, again)
		}
	}
	if a[3] != 100 || a[0] == other[0] {
		t.Fatalf("start %v (want 100) or seeds 7/8 equal: %v %v", a[3], a, other)
	}
}

func TestBook_UnknownIsUnknown(t *testing.T) {
	b := NewBook(Options{})
	if _, ok := b.Price("binance", btc, time.Now()); ok {
		t.Error("an unmodelled pair has a price")
	}
	if _, ok := b.MarkPrice("binance", "BTCUSDT"); ok {
		t.Error("an unmodelled wire symbol has a mark price")
	}
}

func TestSymbols_WireRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		venue, wire string
		p           Pair
		swap        bool
	}{
		{"binance", "BTCUSDT", btc, false},
		{"binance", "ETHBTC", Pair{"ETH", "BTC"}, false},
		{"binance", "SOLFDUSD", Pair{"SOL", "FDUSD"}, false},
		{"okx", "BTC-USDT", btc, false},
		{"okx", "BTC-USDT-SWAP", btc, true},
	} {
		p, swap, ok := ParseWire(tc.venue, tc.wire)
		if !ok || p != tc.p || swap != tc.swap {
			t.Errorf("ParseWire(%s,%s) = %v %v %v", tc.venue, tc.wire, p, swap, ok)
		}
		if got := Wire(tc.venue, tc.p, tc.swap); got != tc.wire {
			t.Errorf("Wire(%s,%v,%v) = %s, want %s", tc.venue, tc.p, tc.swap, got, tc.wire)
		}
	}
	for _, bad := range []string{"", "USDT", "btcusdt", "BTCUSDT_PERP"} {
		if _, _, ok := ParseWire("binance", bad); ok {
			t.Errorf("ParseWire(binance, %q) ok", bad)
		}
	}
	if p, err := ParsePair("btc/usdt:usdt"); err != nil || p != btc {
		t.Errorf("ParsePair = %v %v", p, err)
	}
	if _, err := ParsePair("BTC-USDT"); err == nil {
		t.Error("ParsePair accepted a wire symbol")
	}
}
