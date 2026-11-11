package market

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/replay"
)

var recT0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

func tickLine(at time.Time, provider, symbol string, last float64) replay.Line {
	return replay.Line{Clock: at, Event: events.Event{Kind: events.KindPerceptionMarketTick, Time: at,
		Payload: map[string]any{"provider": provider, "symbol": symbol, "last": last, "at": at}}}
}

func recordingOf(t *testing.T, o RecordingOptions, lines ...replay.Line) *recordingModel {
	t.Helper()
	m, err := newRecording(lines, "rec.jsonl", "BTC/USDT", o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRecordingFlashCrashPrices(t *testing.T) {
	path := repoFile(t, "testdata/replay/flash_crash/events.jsonl")
	lines, err := replay.LoadEvents(path)
	if err != nil || len(lines) == 0 {
		t.Fatalf("flash_crash events: %d lines, %v", len(lines), err)
	}
	m, err := newRecording(lines, "events.jsonl", "BTC/USDT", RecordingOptions{Align: recT0})
	if err != nil {
		t.Fatal(err)
	}
	if m.opt.Source != "okx:BTC/USDT" {
		t.Errorf("source = %q, want the one provider that recorded the symbol", m.opt.Source)
	}
	for _, c := range []struct {
		at   time.Time
		want float64
	}{{recT0, 70000}, {recT0.Add(11 * time.Minute), 62000}, {recT0.Add(13 * time.Minute), 55750}, {recT0.Add(2 * time.Hour), 70000}, {recT0.Add(-time.Hour), 70000}} {
		if got, ok := m.Price(c.at); !ok || got != c.want {
			t.Errorf("price at %s = %v, %v; want %v", c.at.Format("15:04"), got, ok, c.want)
		}
	}
}

func TestRecordingAlignAndSpeed(t *testing.T) {
	sim := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	lines := []replay.Line{tickLine(recT0, "okx", "BTC/USDT", 100), tickLine(recT0.Add(10*time.Minute), "okx", "BTC/USDT", 200)}
	m := recordingOf(t, RecordingOptions{Align: sim, Speed: 2}, lines...)
	for _, c := range []struct {
		at   time.Duration
		want float64
	}{{0, 100}, {4*time.Minute + 59*time.Second, 100}, {5 * time.Minute, 200}, {time.Hour, 200}} {
		if got, _ := m.Price(sim.Add(c.at)); got != c.want {
			t.Errorf("at +%v at double speed: %v, want %v", c.at, got, c.want)
		}
	}
	lin := recordingOf(t, RecordingOptions{Align: sim, Interpolate: "linear"}, lines...)
	if got, _ := lin.Price(sim.Add(5 * time.Minute)); got != 150 {
		t.Errorf("linear midpoint = %v, want 150", got)
	}
	if _, err := newRecording(lines, "r", "BTC/USDT", RecordingOptions{Interpolate: "cubic"}); err == nil {
		t.Error("an unknown interpolation must be refused")
	}
}

func TestRecordingIgnoresNonMarketKinds(t *testing.T) {
	other := replay.Line{Clock: recT0.Add(time.Minute), Event: events.Event{Kind: events.KindPerceptionFuturesPositions, Time: recT0.Add(time.Minute),
		Payload: map[string]any{"provider": "okx", "symbol": "BTC/USDT", "last": 1}}}
	snap := replay.Line{Clock: recT0.Add(2 * time.Minute), Event: events.Event{Kind: events.KindPerceptionPortfolioSnapshot, Time: recT0, Payload: map[string]any{"last": 5}}}
	m := recordingOf(t, RecordingOptions{Align: recT0}, tickLine(recT0, "okx", "BTC/USDT", 100), other, snap)
	if len(m.ticks) != 1 {
		t.Fatalf("%d ticks, want only the market tick", len(m.ticks))
	}
	if got, _ := m.Price(recT0.Add(5 * time.Minute)); got != 100 {
		t.Errorf("price = %v, want 100: a futures or portfolio line must not move it", got)
	}
	if _, err := newRecording([]replay.Line{other}, "r", "BTC/USDT", RecordingOptions{}); err == nil {
		t.Error("a recording with no market tick must be refused")
	}
	two := []replay.Line{tickLine(recT0, "okx", "BTC/USDT", 1), tickLine(recT0, "binance", "BTC/USDT", 2)}
	if _, err := newRecording(two, "r", "BTC/USDT", RecordingOptions{}); err == nil {
		t.Error("two providers of one symbol need a source")
	}
	if m, err := newRecording(two, "r", "BTC/USDT", RecordingOptions{Source: "binance:BTC/USDT"}); err != nil || m.ticks[0].px != 2 {
		t.Errorf("source binance: %v", err)
	}
}

func candleLine(open time.Time, tf string, o, h, l, c float64) replay.Line {
	return replay.Line{Clock: open.Add(time.Minute), Event: events.Event{Kind: events.KindPerceptionMarketCandleClosed, Time: open.Add(time.Minute),
		Payload: map[string]any{"provider": "okx", "symbol": "BTC/USDT", "timeframe": tf,
			"bar": map[string]any{"open_time": open.UnixMilli(), "open": o, "high": h, "low": l, "close": c}}}}
}

// The ticker, the klines and the mark price are one price source.
func TestRecordingFeedsMarkPrices(t *testing.T) {
	clk := newFakeNow()
	clk.t = recT0
	home := t.TempDir()
	b := NewBook(Options{Now: clk.Now, Home: home})
	src := repoFile(t, "testdata/replay/flash_crash/events.jsonl")
	data, err := os.ReadFile(src)
	if err != nil || len(data) == 0 {
		t.Fatalf("flash_crash events: %d bytes, %v", len(data), err)
	}
	if err := os.WriteFile(filepath.Join(home, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRecording("okx", "BTC/USDT", "events.jsonl", RecordingOptions{}); err != nil {
		t.Fatal(err)
	}
	clk.Add(13 * time.Minute)
	want := 55750.0
	near(t, "price", price(b, "okx", clk.Now()), want)
	if mark, ok := b.MarkPrice("okx", "BTC-USDT-SWAP"); !ok || mark != want {
		t.Errorf("mark price = %v, %v; want %v", mark, ok, want)
	}
	bars := b.Bars("okx", btc, time.Minute, clk.Now().Add(-2*time.Minute), clk.Now())
	if len(bars) == 0 || bars[len(bars)-1].C != want {
		t.Errorf("last kline %+v, want close %v", bars, want)
	}
}

func TestRecordingServesRecordedBars(t *testing.T) {
	lines := []replay.Line{tickLine(recT0, "okx", "BTC/USDT", 100), candleLine(recT0, "1m", 100, 130, 90, 120), candleLine(recT0.Add(time.Minute), "1m", 120, 125, 80, 85)}
	sim := recT0.Add(24 * time.Hour)
	m := recordingOf(t, RecordingOptions{Align: sim}, lines...)
	got, ok := m.Bars(time.Minute, sim, sim.Add(time.Hour))
	if !ok || len(got) != 2 || got[0].H != 130 || got[1].L != 80 || !got[1].Open.Equal(sim.Add(time.Minute)) {
		t.Errorf("recorded bars = %+v, %v; want the two bars shifted to the alignment", got, ok)
	}
	if _, ok := m.Bars(5*time.Minute, sim, sim.Add(time.Hour)); ok {
		t.Error("a timeframe the recording lacks must fall back to the sampled path")
	}
	if _, ok := recordingOf(t, RecordingOptions{Align: sim, Speed: 2}, lines...).Bars(time.Minute, sim, sim.Add(time.Hour)); ok {
		t.Error("recorded bars are served only at the recorded pace")
	}
}

func TestRecordingPersistRoundTrip(t *testing.T) {
	home := t.TempDir()
	clk := newFakeNow()
	clk.t = recT0
	b := NewBook(Options{Now: clk.Now, Home: home})
	if err := os.MkdirAll(filepath.Join(home, "lab", "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(repoFile(t, "testdata/replay/flash_crash/events.jsonl"))
	if err != nil || len(data) == 0 {
		t.Fatalf("flash_crash events: %d bytes, %v", len(data), err)
	}
	rel := filepath.Join("lab", "recordings", "rec1.jsonl")
	if err := os.WriteFile(filepath.Join(home, rel), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.SetRecording("okx", "BTC/USDT", rel, RecordingOptions{Speed: 2}); err != nil {
		t.Fatal(err)
	}
	state, err := b.MarshalState()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), `"file":"lab/recordings/rec1.jsonl"`) || strings.Contains(string(state), home) {
		t.Errorf("the saved state must name the file relative to the home: %s", state)
	}
	// A restore into a new home (a snapshot restore, a crash restart) reads the file there.
	home2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, "lab", "recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home2, rel), data, 0o600); err != nil {
		t.Fatal(err)
	}
	b2 := NewBook(Options{Now: clk.Now, Home: home2})
	if err := b2.RestoreState(state); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, 6 * time.Minute, 7 * time.Minute, time.Hour} {
		if g, w := price(b2, "okx", recT0.Add(d)), price(b, "okx", recT0.Add(d)); g != w {
			t.Errorf("+%v: restored price %v, original %v", d, g, w)
		}
	}
	// The file is gone: the restore fails and changes nothing.
	b3 := NewBook(Options{Now: clk.Now, Home: t.TempDir()})
	if err := b3.Set("", btc, 7); err != nil {
		t.Fatal(err)
	}
	if err := b3.RestoreState(state); err == nil {
		t.Error("restoring a recording whose file is missing must fail")
	}
	near(t, "unchanged after a failed restore", price(b3, "okx", clk.Now()), 7)
}
