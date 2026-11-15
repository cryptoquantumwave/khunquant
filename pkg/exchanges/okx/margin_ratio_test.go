package okx

import (
	"math"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

func f64(v float64) *float64 { return &v }
func str(v string) *string   { return &v }

// The margin ratio of an OKX position comes from OKX's mgnRatio (equity over
// maintenance), not from ccxt's mmr / (imr + upl), which is negative for a cross
// position whose loss has passed its initial margin requirement.
func TestFixPositionMarginRatio(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		mgnRatio any      // the wire's mgnRatio; nil: the key is absent
		ccxt     *float64 // what ccxt derived
		want     *float64 // nil: unknown
	}{
		// OKX's own sample for a cross position (ccxt okx.go): imr 170.66, mmr 1.3653,
		// mgnRatio 140.499. A loss of 180 makes ccxt's ratio 1.3653 / (170.66 - 180) < 0.
		{"cross, loss past imr: ccxt negative, OKX far from liquidation", "cross", "140.49930117599155", f64(-0.1462), f64(1 / 140.49930117599155)},
		{"cross, loss just short of imr: ccxt huge, OKX far from liquidation", "cross", "140.49930117599155", f64(13.65), f64(1 / 140.49930117599155)},
		{"cross near liquidation", "cross", "1.05", f64(0.2), f64(1 / 1.05)},
		{"at liquidation", "cross", "1", f64(0.5), f64(1)},
		{"past liquidation", "cross", "0.5", f64(0.5), f64(2)},
		{"equity gone: capped, never infinite or negative", "cross", "0", f64(-3), f64(marginRatioMax)},
		{"equity negative: capped", "cross", "-0.4", f64(-3), f64(marginRatioMax)},
		{"a sliver of equity: capped", "cross", "0.0001", f64(-3), f64(marginRatioMax)},
		{"isolated: OKX's number too", "isolated", "11.731726509588816", f64(0.08), f64(1 / 11.731726509588816)},
		// No mgnRatio on the wire.
		{"cross with no mgnRatio: ccxt's number is not the account's risk, so unknown", "cross", "", f64(0.3), nil},
		{"cross with the key absent: unknown", "cross", nil, f64(-0.3), nil},
		{"isolated with no mgnRatio: ccxt's number is kept", "isolated", "", f64(0.3), f64(0.3)},
		{"isolated with no mgnRatio and a negative number: unknown", "isolated", "", f64(-0.3), nil},
		{"isolated with no mgnRatio and no number: unknown", "isolated", "", nil, nil},
		{"mgnRatio is not a number: as absent", "cross", "n/a", f64(0.3), nil},
		{"mgnRatio is not a string: as absent", "isolated", 12.5, f64(0.3), f64(0.3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ccxt.Position{MarginMode: str(tc.mode), MarginRatio: tc.ccxt, Info: map[string]any{}}
			if tc.mgnRatio != nil {
				p.Info["mgnRatio"] = tc.mgnRatio
			}
			FixPositionMarginRatio(&p)
			switch {
			case tc.want == nil && p.MarginRatio != nil:
				t.Errorf("margin ratio = %v, want unknown", *p.MarginRatio)
			case tc.want != nil && p.MarginRatio == nil:
				t.Errorf("margin ratio unknown, want %v", *tc.want)
			case tc.want != nil && math.Abs(*p.MarginRatio-*tc.want) > 1e-12:
				t.Errorf("margin ratio = %v, want %v", *p.MarginRatio, *tc.want)
			}
			if r := p.MarginRatio; r != nil && (math.IsNaN(*r) || math.IsInf(*r, 0) || *r < 0) {
				t.Errorf("margin ratio %v is not a number a reader can use", *r)
			}
		})
	}
	FixPositionMarginRatio(nil) // no panic
	var bare ccxt.Position      // a nil Info map
	FixPositionMarginRatio(&bare)
	if bare.MarginRatio != nil {
		t.Errorf("a bare position got a margin ratio: %v", *bare.MarginRatio)
	}
}
