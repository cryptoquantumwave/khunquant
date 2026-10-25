package sweep

import (
	"fmt"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func TestMaterial(t *testing.T) {
	d := worldmodel.Diff{
		Changes: []worldmodel.Change{
			{Subject: "portfolio", Key: "total_usd", Old: 10000.0, New: 10150.0},             // 1.5%: noise
			{Subject: "portfolio", Key: "total_usd", Old: 10000.0, New: 10300.0},             // 3%: material
			{Subject: "portfolio", Key: "class:crypto", Old: 5000.0, New: 5050.0},            // 1%: noise
			{Subject: "portfolio", Key: "unpriced_assets", Old: 0.0, New: 1.0},               // material
			{Subject: "price:binance:BTC/USDT", Key: "close_1h", Old: 100.0, New: 101.0},     // noise
			{Subject: "price:binance:ETH/USDT", Key: "close_1h", Old: 100.0, New: 96.0},      // material
			{Subject: "price:binance:SOL/USDT", Key: "close_1h", Old: nil, New: 100.0},       // appeared: material
			{Subject: "price:binance:XRP/USDT", Key: "close_1h", Old: 0.0, New: 0.5},         // from zero: material
			{Subject: "position:binance:BTC", Key: "qty", Old: nil, New: 0.5},                // holding appeared
			{Subject: "futures:okx/main:BTC/USDT:USDT", Key: "long", Old: 100.0, New: 101.0}, // never noise
			{Subject: "order:okx/main:BTC/USDT", Key: "fill", New: "buy"},                    // fill
			{Subject: "plan:dn:3", Key: "status", Old: "active", New: "degraded"},            // plan
			{Subject: "source:portfolio", Key: "state", Old: "ok", New: "degraded"},          // source
			{Subject: "price:binance:DOGE/USDT", Key: "close_1h", Old: "n/a", New: "n/a"},    // not numbers
		},
		Signals: []worldmodel.Signal{{Series: "price:binance:BTC/USDT@1m", Detector: "ewma_z", Value: 4.2, Fired: true}},
	}
	got, noise := Material(d)
	if noise != 3 {
		t.Errorf("noise = %d, want 3", noise)
	}
	if len(got.Changes) != len(d.Changes)-3 || len(got.Signals) != 1 {
		t.Fatalf("kept %d changes and %d signals", len(got.Changes), len(got.Signals))
	}
	for _, c := range got.Changes {
		if c.Subject == "price:binance:BTC/USDT" || (c.Subject == "portfolio" && c.Key == "class:crypto") {
			t.Errorf("noise kept: %+v", c)
		}
	}
}

// A diff of nothing but noise is empty once filtered; one signal alone is not.
func TestMaterial_EmptyAndSignalOnly(t *testing.T) {
	got, noise := Material(noiseOnly())
	if !got.Empty() || noise != 2 {
		t.Fatalf("got %+v, noise %d", got, noise)
	}
	got, _ = Material(worldmodel.Diff{Signals: []worldmodel.Signal{{Series: "s", Detector: "cusum", Value: 1}}})
	if got.Empty() {
		t.Fatal("a fired signal is always material")
	}
}

// A quiet day with realistic noise: a portfolio snapshot every 15 minutes and an
// hourly close for four symbols wobble all day, and not one sweep of the 48 has
// anything material. (A day with no change at all would prove nothing: the log is
// almost never empty.)
func TestMaterial_QuietDayWithRealisticNoiseIsQuiet(t *testing.T) {
	total := 10000.0
	prices := map[string]float64{"BTC/USDT": 62000, "ETH/USDT": 3400, "SOL/USDT": 140, "AAPL": 190}
	material := 0
	for sweep := range 48 {
		var d worldmodel.Diff
		// Two snapshots per half hour, each a small, deterministic wobble.
		newTotal := total * (1 + 0.004*wobble(sweep))
		d.Changes = append(d.Changes, worldmodel.Change{
			Subject: "portfolio", Key: "total_usd", Old: total, New: newTotal, Trust: proactive.TrustSystem,
		})
		if sweep%2 == 0 { // an hourly close every second sweep
			for sym, p := range prices {
				np := p * (1 + 0.006*wobble(sweep+len(sym)))
				d.Changes = append(d.Changes, worldmodel.Change{
					Subject: "price:binance:" + sym, Key: "close_1h", Old: p, New: np, Trust: proactive.TrustExchange,
				})
			}
		}
		if m, _ := Material(d); !m.Empty() {
			material++
		}
	}
	if material != 0 {
		t.Fatalf("%d of 48 sweeps of a quiet day had something material", material)
	}
}

// wobble is a deterministic value in [-1, 1].
func wobble(i int) float64 {
	return float64((i*7+3)%11-5) / 5
}

func TestMaterial_NonFiniteValuesAreNotNoise(t *testing.T) {
	d := worldmodel.Diff{Changes: []worldmodel.Change{
		{Subject: "portfolio", Key: "total_usd", Old: 100.0, New: nanF()},
		{Subject: "portfolio", Key: "total_usd", Old: infF(), New: 100.0},
	}}
	if got, noise := Material(d); noise != 0 || len(got.Changes) != 2 {
		t.Fatalf("kept %d, noise %d: a value that is not a finite number is shown, not hidden", len(got.Changes), noise)
	}
	_ = fmt.Sprint
}

func nanF() float64 { var z float64; return z / z }
func infF() float64 { var z float64; return 1 / z }
