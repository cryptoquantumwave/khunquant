package triggers

import (
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
)

// healthFromCheck is what the perception source does with one check: the evaluation's
// snapshot becomes the plan.health numbers.
func healthFromCheck(t *testing.T, f deltaneutral.FuturesState) planhealth.Health {
	t.Helper()
	plan := deltaneutral.Plan{
		ID: 7, Status: deltaneutral.PlanStatusActive, SpotProvider: "spot", FuturesProvider: "fut",
		RiskPolicy: deltaneutral.DefaultRiskPolicy(),
	}
	f.Available = true
	f.MarkPrice = 0.033
	ev := deltaneutral.Evaluate(deltaneutral.EvaluationInput{
		Plan: plan, Now: time.Now(),
		SpotState:    deltaneutral.SpotState{Available: true, Price: 0.033, Quantity: 1350, ValueUSDT: 44.55},
		FuturesState: f,
		FundingInfo:  deltaneutral.FundingInfo{Available: true, CurrentRate: 0.0001},
	})
	snap := ev.Snapshot
	h := planhealth.Health{
		Score: snap.HealthScore, BreachCodes: []string{planhealth.CodeMarginCritical},
		LiquidationDistancePct: snap.LiquidationDistancePct, MarginRatioPct: snap.MarginRatioPct,
		MarginRatioKnown: snap.MarginIsKnown(), DataOK: true, IntervalSeconds: 300, CooldownSeconds: 3600,
	}
	// Through the world model's numbers, as the trigger reads them.
	back, err := planhealth.Decode(h.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if snap.MarginState == "safe" && !snap.MarginIsKnown() && f.LiquidationPrice == 0 {
		t.Fatalf("margin state safe with neither a ratio nor a liquidation price")
	}
	return back
}

// An unknown margin ratio reaches the digest as "unknown", never as 0.0%.
func TestDNHealthTrigger_UnknownMarginRatioIsSaidUnknown(t *testing.T) {
	de := newDNEnv(t, DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT"})
	h := healthFromCheck(t, deltaneutral.FuturesState{Contracts: 135, ContractSize: 10}) // nothing known about the margin
	if h.MarginIsKnown() {
		t.Fatal("margin should be unknown after the round trip")
	}
	c := de.report(h, noon)[0]
	for _, text := range []string{c.Title, c.Detail} {
		if strings.Contains(text, "margin ratio 0.0%") {
			t.Errorf("unknown margin shown as 0.0%%:\n%s", text)
		}
	}
	if !strings.Contains(c.Title, "margin ratio unknown") || !strings.Contains(c.Detail, "margin ratio unknown") {
		t.Errorf("title %q / detail %q lack \"margin ratio unknown\"", c.Title, c.Detail)
	}

	de.lang = "th"
	c = de.report(h, noon.Add(time.Second))[0]
	if strings.Contains(c.Title+c.Detail, "อัตรามาร์จิน 0.0%") || !strings.Contains(c.Detail, "อัตรามาร์จิน ไม่ทราบ") {
		t.Errorf("thai: %q / %q", c.Title, c.Detail)
	}
}

// A known ratio reads exactly as before.
func TestDNHealthTrigger_KnownMarginRatioIsUnchanged(t *testing.T) {
	de := newDNEnv(t, DNHealthParams{PlanID: 7, Symbol: "CHZ/USDT:USDT"})
	h := healthFromCheck(t, deltaneutral.FuturesState{Contracts: 135, ContractSize: 10, MarginRatioPct: 91.2, MarginRatioKnown: true, LiquidationPrice: 0.04})
	c := de.report(h, noon)[0]
	if !strings.Contains(c.Detail, "margin ratio 91.2%") || !strings.Contains(c.Title, "margin ratio 91.2%") {
		t.Errorf("title %q / detail %q", c.Title, c.Detail)
	}
}

func TestPlanHealth_UnknownMarginRatioIsOmittedAndLegacyZeroReadsUnknown(t *testing.T) {
	enc := planhealth.Health{Score: 50, DataOK: true}.Encode()
	if _, has := enc["margin_ratio_pct"]; has {
		t.Fatal("an unknown margin ratio must not be encoded as a number")
	}
	back, err := planhealth.Decode(enc)
	if err != nil || back.MarginIsKnown() {
		t.Fatalf("decode: %v known %v", err, back.MarginIsKnown())
	}
	// A report from before the flag: 0 for unknown. It reads as unknown; a non-zero as known.
	enc["margin_ratio_pct"] = 0
	if back, _ = planhealth.Decode(enc); back.MarginIsKnown() {
		t.Fatal("a legacy 0 must read as unknown")
	}
	enc["margin_ratio_pct"] = 42
	if back, _ = planhealth.Decode(enc); !back.MarginIsKnown() || back.MarginRatioPct != 42 {
		t.Fatalf("a legacy 42: known %v pct %v", back.MarginIsKnown(), back.MarginRatioPct)
	}
	enc = planhealth.Health{Score: 50, MarginRatioPct: 0, MarginRatioKnown: true}.Encode()
	if v, has := enc["margin_ratio_pct"]; !has || v != 0 {
		t.Fatalf("a known 0 is encoded as 0: %v %v", v, has)
	}
}
