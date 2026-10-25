package reflection

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const ownerAlertReason = "the trigger is an alert the owner set"

var evidence = []string{"d1", "d2", "d3", "d4"}

func TestReflection_OutOfBoundsChangeRejected(t *testing.T) {
	r := newRig(t)
	for name, change := range map[string]map[string]any{
		"the injected 99":            {"floor_th_pct": 99},
		"just over":                  {"floor_th_pct": 10.01},
		"below the floor":            {"floor_th_pct": 0.5},
		"negative":                   {"floor_th_pct": -3},
		"a string":                   {"floor_th_pct": "5"},
		"unchanged":                  {"floor_th_pct": 3},
		"a parameter without bounds": {"protective_move_pct": 20},
		"another without bounds":     {"k": 1},
		"an unknown parameter":       {"floor_pct": 5},
		"no parameter":               {},
		"a huge number":              {"floor_th_pct": 1e300},
	} {
		v := Validate([]proactive.ReflectionItem{item(TypeThreshold, "pf-move_vs_vol", change, "x", evidence...)}, r.world(evidence))
		if len(v.Valid) != 0 || v.Dropped != 1 {
			t.Errorf("%s: valid %d dropped %d", name, len(v.Valid), v.Dropped)
		}
	}
	// Inside the bounds it is valid, and the bound is a hard line at both ends.
	for _, to := range []float64{1, 5, 10} {
		v := Validate([]proactive.ReflectionItem{item(TypeThreshold, "pf-move_vs_vol", map[string]any{"floor_th_pct": to}, "x", evidence...)}, r.world(evidence))
		if len(v.Valid) != 1 {
			t.Errorf("%g: valid %d (%v)", to, len(v.Valid), v.Reasons)
		}
	}
	// Mutes: 1 to 30 days.
	for days, valid := range map[int]bool{0: false, 1: true, 30: true, 31: false, 365: false, -1: false} {
		v := Validate([]proactive.ReflectionItem{item(TypeMute, "move_vs_vol", map[string]any{"days": days}, "x", evidence...)}, r.world(evidence))
		if (len(v.Valid) == 1) != valid {
			t.Errorf("mute for %d days: valid %v, want %v", days, len(v.Valid) == 1, valid)
		}
	}
	// A price threshold moves at most 20% down or 25% up; a macd threshold is not tunable.
	p := triggers.PriceCrossParams{Provider: "okx", Symbol: "BTC/USDT", Direction: "above", Threshold: 100000}
	rsi := json.RawMessage(`{"provider":"okx","symbol":"BTC/USDT","timeframe":"1h","indicator":"rsi","period":14,"direction":"above","threshold":70}`)
	macd := json.RawMessage(`{"provider":"okx","symbol":"BTC/USDT","timeframe":"1h","indicator":"macd_line","direction":"above","threshold":1}`)
	for _, c := range []struct {
		kind  string
		raw   json.RawMessage
		to    float64
		valid bool
	}{
		{triggers.KindPriceCross, p.JSON(), 110000, true},
		{triggers.KindPriceCross, p.JSON(), 125000, true},
		{triggers.KindPriceCross, p.JSON(), 200000, false},
		{triggers.KindPriceCross, p.JSON(), 79999, false},
		{triggers.KindIndicatorCross, rsi, 80, true},
		{triggers.KindIndicatorCross, rsi, 95, false},
		{triggers.KindIndicatorCross, macd, 1.2, false},
	} {
		got := triggers.Bounds(c.kind, c.raw)["threshold"]
		if _, err := triggers.WithParam(c.kind, c.raw, "threshold", c.to); (err == nil) != c.valid {
			t.Errorf("%s %s -> %g: err %v, bounds %+v", c.kind, c.raw, c.to, err, got)
		}
	}
}

func TestReflection_ItemTypesAreTheFourAndNoMore(t *testing.T) {
	r := newRig(t)
	for _, typ := range []string{"new_watch", "profile_update", "create_order", "propose_order", "mandate", "order", "", "THRESHOLD_CHANGE"} {
		v := Validate([]proactive.ReflectionItem{item(typ, "pf-move_vs_vol", map[string]any{"floor_th_pct": 5}, "x", evidence...)}, r.world(evidence))
		if len(v.Valid) != 0 {
			t.Errorf("type %q was accepted", typ)
		}
	}
}

// Origins of the alerts an owner sets: the alert tools, a migrated cron alert, an active
// watch or reminder intent. Each is a level chosen on purpose.
var ownerOrigins = map[string]struct{ origin, intentID string }{
	"set_price_alert": {triggers.OriginSetPriceAlert, ""},
	"migrated cron":   {triggers.MigratedOriginPrefix + "job-7", ""},
	"an intent watch": {triggers.IntentOrigin("01ABCINTENT"), "01ABCINTENT"},
}

var btcAbove100k = triggers.PriceCrossParams{Provider: "okx", Symbol: "BTC/USDT", Direction: "above", Threshold: 100000}

func TestReflection_OwnerAlertsAreNotTunable(t *testing.T) {
	r := newRig(t)
	for name, o := range ownerOrigins {
		s := r.addSpec("alert-"+strings.ReplaceAll(name, " ", "-"), triggers.KindPriceCross, o.origin, o.intentID, btcAbove100k.JSON())
		if !triggers.OwnerAlertSpec(s.Kind, s.Origin) {
			t.Fatalf("%s: the fixture is not an owner alert", name)
		}
		if b := triggers.SpecBounds(s); b != nil {
			t.Errorf("%s: SpecBounds = %+v, want none", name, b)
		}
		w := r.world([]string{"p1"}, triggers.KindPriceCross)
		v := Validate([]proactive.ReflectionItem{item(TypeThreshold, s.ID, map[string]any{"threshold": 110000}, "x", "p1")}, w)
		if len(v.Valid) != 0 || v.Reasons[ownerAlertReason] != 1 {
			t.Errorf("%s: valid %d, reasons %v: a change to the owner's alert must be refused as one", name, len(v.Valid), v.Reasons)
		}
		// Also by its trigger id, "<kind>:<spec id>".
		v = Validate([]proactive.ReflectionItem{item(TypeThreshold, "price_cross:"+s.ID, map[string]any{"threshold": 110000}, "x", "p1")}, w)
		if len(v.Valid) != 0 {
			t.Errorf("%s: a change to the owner's alert was accepted by trigger id", name)
		}
	}
	// A price_cross that is not an owner's alert is tunable as before, within its bounds.
	s := r.addSpec("not-an-alert", triggers.KindPriceCross, "builtin:test", "", btcAbove100k.JSON())
	w := r.world([]string{"p1"}, triggers.KindPriceCross)
	if v := Validate([]proactive.ReflectionItem{item(TypeThreshold, s.ID, map[string]any{"threshold": 110000}, "x", "p1")}, w); len(v.Valid) != 1 {
		t.Errorf("a spec that is not an owner's alert was refused: %v", v.Reasons)
	}
}

func TestReflection_MuteMayNotTargetAnOwnerAlertButMayMuteItsKind(t *testing.T) {
	r := newRig(t)
	s := r.addSpec("my-alert", triggers.KindPriceCross, triggers.OriginSetPriceAlert, "", btcAbove100k.JSON())
	w := r.world([]string{"p1"}, triggers.KindPriceCross)
	for _, target := range []string{"price_cross:" + s.ID, "price_cross:my-alert:above", s.ID} {
		v := Validate([]proactive.ReflectionItem{item(TypeMute, target, map[string]any{"days": 7}, "x", "p1")}, w)
		if len(v.Valid) != 0 {
			t.Errorf("a mute targeting %q was accepted", target)
		}
		if target == "price_cross:"+s.ID && v.Reasons[ownerAlertReason] != 1 {
			t.Errorf("reasons = %v", v.Reasons)
		}
	}
	// A kind-wide mute is fine: it silences a noisy kind, and a protective alert is exempt in attention.
	if v := Validate([]proactive.ReflectionItem{item(TypeMute, "price_cross", map[string]any{"days": 7}, "x", "p1")}, w); len(v.Valid) != 1 {
		t.Errorf("a kind-wide mute was refused: %v", v.Reasons)
	}
}
