package market

import (
	"math"
	"strings"
	"testing"
	"time"
)

const btcSwap = "BTC-USDT-SWAP"

// The schedule is one place: 8 h settles at 00, 08 and 16 UTC, and a shorter interval at
// its multiples, whatever the time of day the question is asked.
func TestFundingSchedule_Next(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		interval time.Duration
		from     time.Time
		want     time.Time
	}{
		{8 * time.Hour, day.Add(-time.Second), day},
		{8 * time.Hour, day, day.Add(8 * time.Hour)}, // strictly after
		{8 * time.Hour, day.Add(15*time.Hour + 59*time.Minute), day.Add(16 * time.Hour)},
		{8 * time.Hour, day.Add(16 * time.Hour), day.Add(24 * time.Hour)},
		{time.Hour, day.Add(90 * time.Minute), day.Add(2 * time.Hour)},
		{4 * time.Hour, day.Add(5 * time.Hour), day.Add(8 * time.Hour)},
	} {
		if got := (FundingSchedule{c.interval}).Next(c.from); !got.Equal(c.want) {
			t.Errorf("Next(%v) every %v = %v, want %v", c.from, c.interval, got, c.want)
		}
	}
}

// What a rate was at an earlier instant is still answerable after it changed, and a venue
// that sets nothing follows the wildcard.
func TestTerms_HistoryAndVenues(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	if r, ok := b.FundingAt("okx", btcSwap, t0); !ok || r != DefaultFunding {
		t.Fatalf("rate before any set = %v %v, want the default", r, ok)
	}
	clk.Add(time.Hour)
	if err := b.SetFunding("", btc, -0.0003); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	if err := b.SetFunding("okx", btc, 0.002); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		venue, wire string
		at          time.Duration
		want        float64
	}{
		{"okx", btcSwap, 30 * time.Minute, DefaultFunding},
		{"okx", btcSwap, time.Hour, -0.0003},
		{"binance", "BTCUSDT", 150 * time.Minute, -0.0003}, // the wildcard's
		{"okx", btcSwap, 2 * time.Hour, 0.002},
		{"okx", btcSwap, 5 * time.Hour, 0.002},
	} {
		if got, ok := b.FundingAt(c.venue, c.wire, t0.Add(c.at)); !ok || got != c.want {
			t.Errorf("FundingAt(%s, +%v) = %v %v, want %v", c.venue, c.at, got, ok, c.want)
		}
	}
	if _, ok := b.FundingAt("okx", "ETH-USDT-SWAP", t0); ok {
		t.Error("a pair the book has no price for must not report a funding rate")
	}
	if _, ok := b.FundingAt("okx", "BTC-USDT", t0); ok {
		t.Error("a spot instrument has no funding")
	}
}

func TestSetTerms_Validated(t *testing.T) {
	b := NewBook(Options{})
	for _, bad := range []time.Duration{0, -time.Hour, time.Minute, 25 * time.Hour} {
		if err := b.SetFundingInterval("", btc, bad); err == nil {
			t.Errorf("funding interval %v accepted", bad)
		}
	}
	for _, bad := range []float64{0, -0.1, 1, 2, math.NaN(), math.Inf(1)} {
		if err := b.SetMaintRate("", btc, bad); err == nil {
			t.Errorf("maintenance rate %v accepted", bad)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(-1), 1.5} {
		if err := b.SetFunding("", btc, bad); err == nil {
			t.Errorf("funding rate %v accepted", bad)
		}
	}
	if err := b.Set("", btc, math.NaN()); err == nil {
		t.Error("a NaN price was accepted")
	}
	if err := b.Set("", btc, math.Inf(1)); err == nil {
		t.Error("an infinite price was accepted")
	}
	if err := b.SetMaintRate("", btc, 0.005); err != nil {
		t.Fatal(err)
	}
	if err := b.SetFundingInterval("okx", btc, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	if s := b.schedule("okx", btc); s.Interval != 4*time.Hour {
		t.Errorf("schedule = %v, want 4h", s)
	}
	if r, _ := b.MaintRateAt("okx", btcSwap, b.Now()); r != 0.005 { // the venue's first entry started from the wildcard's
		t.Errorf("okx maintenance rate = %v, want the wildcard's 0.005", r)
	}
}

func TestFirstCross(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	if err := b.Path("", btc, 80000, 2*time.Hour, ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	cross := func(from, to time.Duration, level float64, below bool) (time.Time, bool) {
		return b.FirstCross("okx", btcSwap, t0.Add(from), t0.Add(to), level, below)
	}

	// A linear path is solved, not sampled: 75000 is reached at exactly half of 2 h.
	if at, ok := cross(0, 3*time.Hour, 75000, false); !ok || at.Sub(t0.Add(time.Hour)) > time.Millisecond || at.Before(t0.Add(time.Hour)) {
		t.Errorf("crossing 75000 = %v %v, want +1h (to the ms)", at.Sub(t0), ok)
	}
	if _, ok := cross(0, 3*time.Hour, 81000, false); ok {
		t.Error("a level the path never reaches was crossed")
	}
	if at, ok := cross(0, time.Hour, 70000, false); !ok || !at.Equal(t0) {
		t.Errorf("a level already reached at the start = %v %v, want the start", at, ok)
	}
	if _, ok := cross(0, 59*time.Minute, 75000, false); ok {
		t.Error("the window ends before the crossing")
	}
	if at, ok := cross(0, 3*time.Hour, 69999, true); ok {
		t.Errorf("below 69999 crossed at %v, but the path only rises", at)
	}
	if at, ok := cross(90*time.Minute, 3*time.Hour, 78000, false); !ok || at.Before(t0.Add(96*time.Minute)) || at.After(t0.Add(96*time.Minute+time.Millisecond)) {
		t.Errorf("crossing 78000 from +90m = %v, want +96m", at.Sub(t0))
	}
}

// A crash that recovers between two polls is found, at its own time, which a sample
// taken every minute (or every hour) would not do.
func TestFirstCross_CrashBetweenSamples(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	// Down to 60000 at +10m and back to 70000 at +10m20s: the dip lasts 20 s.
	if err := b.Path("", btc, 70000, 20*time.Second+10*time.Minute, ShapeLinear,
		[]Keyframe{{10 * time.Minute, 60000}}); err != nil {
		t.Fatal(err)
	}
	at, ok := b.FirstCross("okx", btcSwap, t0, t0.Add(6*time.Hour), 61000, true)
	if !ok {
		t.Fatal("the dip to 60000 was missed")
	}
	// 61000 is 90% of the way down the first leg (70000 -> 60000 in 10 m): +9 m.
	if d := at.Sub(t0.Add(9 * time.Minute)); d < 0 || d > time.Millisecond {
		t.Errorf("crossing at %v, want +9m", at.Sub(t0))
	}
	// The price is back at 70000 by the end: only the crossing finds the dip.
	if p, _ := b.Price("okx", btc, t0.Add(6*time.Hour)); p != 70000 {
		t.Fatalf("the path ended at %v", p)
	}
}

// A shock is a jump between segments: the old model holds up to it and the new one
// from it, and the crossing is dated to the instant of the jump.
func TestFirstCross_ShockAndSegments(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	clk.Add(2 * time.Hour)
	if err := b.Shock("", btc, -20); err != nil { // 56000 from +2h
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	if err := b.Set("", btc, 70000); err != nil { // and back from +3h
		t.Fatal(err)
	}
	at, ok := b.FirstCross("okx", btcSwap, t0, t0.Add(5*time.Hour), 60000, true)
	if !ok || !at.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("crossing below 60000 = %v %v, want the shock at +2h", at.Sub(t0), ok)
	}
	if _, ok := b.FirstCross("okx", btcSwap, t0.Add(3*time.Hour), t0.Add(5*time.Hour), 60000, true); ok {
		t.Error("a window after the shock saw it")
	}
	if _, ok := b.FirstCross("okx", "ETH-USDT-SWAP", t0, t0.Add(time.Hour), 1, true); ok {
		t.Error("a pair with no model crossed")
	}
}

// A walk holds one price per step, so the crossing is the first step at or beyond the
// level; a custom model with no instants of its own is sampled once a minute.
func TestFirstCross_WalkAndSampled(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	if err := b.Set("", btc, 70000); err != nil {
		t.Fatal(err)
	}
	if err := b.Walk("", btc, WalkParams{Vol: 0.01, Seed: 7, Step: 5 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	at, ok := b.FirstCross("okx", btcSwap, t0, t0.Add(24*time.Hour), 70000*0.97, true)
	if !ok {
		t.Fatal("the seeded walk never fell 3%; pick another seed")
	}
	if got := at.Sub(t0) % (5 * time.Minute); got != 0 {
		t.Errorf("a walk was crossed at +%v, off its 5m grid", at.Sub(t0))
	}
	if p, _ := b.Price("okx", btc, at); p > 70000*0.97 {
		t.Errorf("price at the crossing = %v, above the level", p)
	}
	if p, _ := b.Price("okx", btc, at.Add(-5*time.Minute)); p <= 70000*0.97 {
		t.Errorf("price a step earlier = %v: the crossing was not the first", p)
	}

	if err := b.SetModel(btc.String(), customModel{}); err != nil {
		t.Fatal(err)
	}
	if at, ok := b.FirstCross("*", btcSwap, t0, t0.Add(2*time.Hour), 100, false); ok {
		t.Errorf("unknown venue crossed at %v", at)
	}
	got, ok := b.FirstCross("okx", btcSwap, t0, t0.Add(2*time.Hour), 100, false)
	if !ok || got.Sub(t0)%time.Minute != 0 || got.Sub(t0) > 61*time.Minute {
		t.Errorf("custom model crossing = %v %v, want a whole minute within the hour it rises", got.Sub(t0), ok)
	}
}

// customModel names no instants: 50 until +30m, then it is 200.
type customModel struct{}

func (customModel) Price(t time.Time) (float64, bool) {
	if t.Before(time.Date(2026, 1, 2, 3, 34, 5, 0, time.UTC)) {
		return 50, true
	}
	return 200, true
}

func TestFirstCross_Replay(t *testing.T) {
	clk := newFakeNow()
	b := NewBook(Options{Now: clk.Now})
	t0 := clk.Now()
	csv := "ts,open,high,low,close\n" +
		"2026-03-10T00:00:00Z,100,101,99,100\n" +
		"2026-03-10T00:01:00Z,100,100,80,80\n" + // falls to 80 inside the bar
		"2026-03-10T00:02:00Z,80,100,80,100\n"
	if err := b.Replay("", btc, strings.NewReader(csv), time.Minute, time.Time{}); err != nil {
		t.Fatal(err)
	}
	at, ok := b.FirstCross("okx", btcSwap, t0, t0.Add(10*time.Minute), 90, true)
	if want := t0.Add(85 * time.Second); !ok || at.Before(want) || at.After(want.Add(time.Millisecond)) {
		t.Errorf("replay crossing below 90 = %v %v, want +85s (halfway down the second bar, which opens at the replay's minute boundary, 5 s before t0)", at.Sub(t0), ok)
	}
}
