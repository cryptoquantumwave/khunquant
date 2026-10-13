package planhealth

import (
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCodes_TableIsStable(t *testing.T) {
	// The order is the order of the bits that travel in a payload: pin the head of it.
	want := []string{
		"data_unavailable", "margin_critical", "liquidation_distance_low", "funding_negative",
		"funding_reversal", "margin_danger", "delta_drift_high", "funding_unavailable",
		"funding_below_min", "exit_spread_target_met", "leg_mismatch", "recovery_required",
		"max_drawdown_breached", "profit_target_met",
	}
	if got := Codes(); !slices.Equal(got, want) {
		t.Fatalf("Codes() = %v", got)
	}
	if len(want) >= 52 {
		t.Fatal("the mask must stay exactly representable")
	}
}

func TestClassOf(t *testing.T) {
	critical := []string{CodeDataUnavailable, CodeMarginCritical, CodeLiquidationDistanceLow}
	for _, c := range Codes() {
		got, ok := ClassOf(c)
		if !ok {
			t.Fatalf("%s unknown", c)
		}
		if (got == ClassCritical) != slices.Contains(critical, c) {
			t.Errorf("%s: class %v", c, got)
		}
	}
	if _, ok := ClassOf("nonsense"); ok || IsCritical("nonsense") {
		t.Error("an unknown code is not a code, and not critical")
	}
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	h := Health{
		Score: 46, BreachCodes: []string{CodeMarginCritical, CodeLegMismatch, CodeProfitTargetMet},
		LiquidationDistancePct: 3.2, MarginRatioPct: 90, DeltaDriftPct: 1.5, LegMismatchPct: 20,
		FundingAPYPct: -12.5, FundingRate: -0.0002, PnLUSDT: -3.5, PnLKnown: true,
		IntervalSeconds: 300, CooldownSeconds: 3600, DataOK: true,
	}
	enc := h.Encode()
	if len(enc) > MaxEntries || len(enc) > 16 {
		t.Fatalf("%d entries; the world model keeps 16", len(enc))
	}
	for k := range enc {
		if len(k) > 48 {
			t.Errorf("key %q is longer than a world-model token", k)
		}
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.BreachCodes, SortedCodes(h.BreachCodes)) || got.Score != 46 || got.PnLUSDT != -3.5 || !got.PnLKnown ||
		got.IntervalSeconds != 300 || got.CooldownSeconds != 3600 || !got.DataOK || got.LegMismatchPct != 20 {
		t.Fatalf("got %+v", got)
	}
	if got.Label() != LabelDanger {
		t.Fatalf("label %q", got.Label())
	}
	// An unknown pnl is absent, not zero.
	h.PnLKnown = false
	if got, _ := Decode(h.Encode()); got.PnLKnown {
		t.Fatal("a plan with no profit and loss decoded with one")
	}
}

func TestEncodeDecode_SilencedCodesRoundTrip(t *testing.T) {
	h := Health{
		Score: 46, BreachCodes: []string{CodeMarginCritical, CodeLegMismatch, CodeFundingNegative},
		SilencedCodes: []string{CodeLegMismatch, CodeMarginCritical}, DataOK: true,
	}
	enc := h.Encode()
	if len(enc) > 16 {
		t.Fatalf("%d entries; the world model keeps 16", len(enc))
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.SilencedCodes, SortedCodes(h.SilencedCodes)) {
		t.Fatalf("silenced = %v, want %v", got.SilencedCodes, SortedCodes(h.SilencedCodes))
	}
	// A report that has no silenced_mask (an older writer) means none silenced.
	delete(enc, keySilenced)
	if got, err := Decode(enc); err != nil || len(got.SilencedCodes) != 0 {
		t.Fatalf("absent mask: %v %v", got.SilencedCodes, err)
	}
	for name, v := range map[string]float64{"fractional": 1.5, "negative": -1, "huge": 1 << 60} {
		bad := Health{Score: 10, DataOK: true}.Encode()
		bad[keySilenced] = v
		if _, err := Decode(bad); err == nil {
			t.Errorf("%s silenced mask decoded", name)
		}
	}
}

func TestEncode_NonFiniteBecomesZero(t *testing.T) {
	enc := Health{Score: 50, LiquidationDistancePct: math.NaN(), FundingAPYPct: math.Inf(1), PnLUSDT: math.Inf(-1), PnLKnown: true}.Encode()
	for k, v := range enc {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("%s = %v", k, v)
		}
	}
}

func TestDecode_RefusesWhatItCannotTrust(t *testing.T) {
	good := Health{Score: 10, DataOK: true}.Encode()
	for name, mutate := range map[string]func(map[string]float64){
		"no mask":       func(m map[string]float64) { delete(m, keyMask) },
		"no score":      func(m map[string]float64) { delete(m, keyScore) },
		"fractional":    func(m map[string]float64) { m[keyMask] = 1.5 },
		"negative mask": func(m map[string]float64) { m[keyMask] = -1 },
		"huge mask":     func(m map[string]float64) { m[keyMask] = 1 << 60 },
		"NaN":           func(m map[string]float64) { m[keyLiq] = math.NaN() },
		"score > 100":   func(m map[string]float64) { m[keyScore] = 101 },
	} {
		m := map[string]float64{}
		for k, v := range good {
			m[k] = v
		}
		mutate(m)
		if _, err := Decode(m); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := Decode(nil); err == nil {
		t.Error("nil decoded")
	}
	// Unknown bits are ignored, not an error: a newer writer, an older reader.
	m := map[string]float64{keyMask: float64(1<<40 | 1), keyScore: 1}
	h, err := Decode(m)
	if err != nil || !slices.Equal(h.BreachCodes, []string{CodeDataUnavailable}) {
		t.Fatalf("%v %v", h, err)
	}
}

func TestSeverity(t *testing.T) {
	cases := map[string][]string{
		"critical": {CodeFundingBelowMin, CodeMarginCritical},
		"danger":   {CodeProfitTargetMet, CodeLegMismatch},
		"warn":     {CodeProfitTargetMet, CodeExitSpreadTargetMet},
	}
	for want, codes := range cases {
		if got := Severity(codes); got != want {
			t.Errorf("%v: %q, want %q", codes, got, want)
		}
	}
	if Severity(nil) != "warn" || Severity([]string{"x"}) != "warn" {
		t.Error("no known code is a warn")
	}
}

// The package must stay a leaf: only the standard library.
func TestImportsStandardLibraryOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "planhealth.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
			t.Errorf("planhealth imports %s", path)
		}
	}
}
