package sweep

import (
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// NoiseFloor is the relative size below which a price or a portfolio value that
// moved is noise to the sweep: 2% (0.02) between the value at the mark and the
// value now.
//
// The change log is almost never empty: a portfolio snapshot every quarter of an
// hour moves the total by something, and every demanded symbol logs a new hourly
// close. A sweep that called the model for each of those would call it at every
// tick, which is the heartbeat it replaces. The floor is a deliberate, coarse
// judgement in code of what is worth a model's look; the triggers (move against
// volatility, drawdown, drift) are what judge size properly and deliver. Noise is not
// lost: the mark does not move past it, so a slow drift accumulates in the next
// diff until it crosses the floor.
const NoiseFloor = 0.02

// Material returns the diff with the changes the sweep does not consider noise, and
// how many it left out.
//
// Always material: every fired signal, a holding or a futures position that appeared
// or went, a fill, a plan or a source changing state, the number of unpriced assets
// changing, and any change whose values are not both numbers. Material only above
// NoiseFloor: an hourly or one-minute price close, the portfolio total and a class
// total.
func Material(d worldmodel.Diff) (worldmodel.Diff, int) {
	out := d
	out.Changes = nil
	noise := 0
	for _, c := range d.Changes {
		if isNoise(c) {
			noise++
			continue
		}
		out.Changes = append(out.Changes, c)
	}
	return out, noise
}

func isNoise(c worldmodel.Change) bool {
	switch {
	case c.Subject == "portfolio" && (c.Key == "total_usd" || strings.HasPrefix(c.Key, "class:")):
	case strings.HasPrefix(c.Subject, "price:") && (c.Key == "close_1h" || c.Key == "close_1m"):
	default:
		return false
	}
	oldV, okOld := number(c.Old)
	newV, okNew := number(c.New)
	if !okOld || !okNew || oldV == 0 {
		return false // appeared, vanished or not a number: not a small move
	}
	return math.Abs(newV-oldV)/math.Abs(oldV) < NoiseFloor
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case float32:
		f := float64(x)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}
