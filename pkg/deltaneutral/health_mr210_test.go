package deltaneutral

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
)

// goodInput is a perfectly balanced, healthy plan on one exchange with 1350 CHZ on
// each leg (135 contracts of 10 CHZ).
func goodInput() EvaluationInput {
	plan := Plan{
		ID: 1, Status: PlanStatusActive, SpotProvider: "okx", FuturesProvider: "okx",
		RiskPolicy: DefaultRiskPolicy(),
	}
	return EvaluationInput{
		Plan:      plan,
		SpotState: SpotState{Available: true, Price: 0.033, Quantity: 1350, ValueUSDT: 44.55},
		FuturesState: FuturesState{
			Available: true, MarkPrice: 0.033, Contracts: 135, ContractSize: 10, NotionalUSDT: 44.55,
			LiquidationPrice: 0.1, MarginRatioPct: 80,
		},
		FundingInfo: FundingInfo{Available: true, CurrentRate: 0.0001},
		Now:         time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestEvaluate_NoFreePoints(t *testing.T) {
	in := goodInput()

	// 20 funding + 25 margin + 20 delta + 10 exchange. Liquidity and profit progress
	// used to add 25 more without any data.
	r := Evaluate(in)
	if r.Snapshot.HealthScore != 75 || r.Snapshot.HealthLabel != HealthLabelHealthy {
		t.Fatalf("same-exchange perfect plan: score %d label %q, want 75/healthy", r.Snapshot.HealthScore, r.Snapshot.HealthLabel)
	}
	in.Plan.FuturesProvider = "binance"
	if r := Evaluate(in); r.Snapshot.HealthScore != 71 || r.Snapshot.HealthLabel != HealthLabelWatch {
		t.Fatalf("cross-exchange perfect plan: score %d label %q, want 71/watch", r.Snapshot.HealthScore, r.Snapshot.HealthLabel)
	}

	// The components that need data score when they have it.
	in = goodInput()
	in.Liquidity = &LiquidityInfo{SlippageBps: 10}
	if r := Evaluate(in); r.Snapshot.HealthScore != 85 {
		t.Fatalf("with liquidity measured: %d, want 85", r.Snapshot.HealthScore)
	}
	in.Liquidity = &LiquidityInfo{SlippageBps: 30} // within twice the 20 bps limit
	if r := Evaluate(in); r.Snapshot.HealthScore != 80 {
		t.Fatalf("with 30 bps of slippage: %d, want 80", r.Snapshot.HealthScore)
	}
	in = goodInput()
	in.Plan.RiskPolicy.ProfitTargetUSDT = 10
	in.PnLKnown, in.PnLUSDT = true, 5
	if r := Evaluate(in); r.Snapshot.HealthScore != 83 { // 75 + round(15 * 0.5)
		t.Fatalf("half way to the target: %d, want 83", r.Snapshot.HealthScore)
	}
	in.PnLUSDT = 25 // beyond the target counts as the whole 15
	r = Evaluate(in)
	if r.Snapshot.HealthScore != 90 {
		t.Fatalf("beyond the target: %d, want 90", r.Snapshot.HealthScore)
	}
	// A profit and loss with no target to measure it against scores nothing.
	in.Plan.RiskPolicy.ProfitTargetUSDT = 0
	if r := Evaluate(in); r.Snapshot.HealthScore != 75 {
		t.Fatalf("no target: %d, want 75", r.Snapshot.HealthScore)
	}
}

// Representative plans, before and after: the score no longer includes 25 unearned
// points. The thresholds of the labels are unchanged, so labels move down.
func TestEvaluate_HealthScoreBeforeAfter(t *testing.T) {
	type c struct {
		name          string
		mutate        func(*EvaluationInput)
		before, after int
		labelAfter    string
	}
	cases := []c{
		{"perfect, one exchange", func(*EvaluationInput) {}, 100, 75, HealthLabelHealthy},
		{"perfect, two exchanges", func(in *EvaluationInput) { in.Plan.FuturesProvider = "binance" }, 96, 71, HealthLabelWatch},
		{"3% drift, margin watch", func(in *EvaluationInput) {
			in.SpotState.ValueUSDT = 44.55 * 1.04
			in.FuturesState.MarginRatioPct = 40
		}, 78, 53, HealthLabelDanger},
	}
	for _, tc := range cases {
		in := goodInput()
		tc.mutate(&in)
		got := Evaluate(in).Snapshot
		if got.HealthScore != tc.after || got.HealthLabel != tc.labelAfter {
			t.Errorf("%s: score %d label %q, want %d/%s (it was %d)", tc.name, got.HealthScore, got.HealthLabel, tc.after, tc.labelAfter, tc.before)
		}
		if tc.before-got.HealthScore != 25 {
			t.Errorf("%s: the score should have dropped by exactly 25 (%d -> %d)", tc.name, tc.before, got.HealthScore)
		}
	}
}

// The base quantity of the futures leg is contracts x contract size. A leg compared as
// contracts x price, or with contracts taken as base units, is wrong by the contract
// size: 10x for CHZ on OKX, 0.01x for BTC.
func TestEvaluate_LegMismatch(t *testing.T) {
	t.Run("10x contract (CHZ on OKX): equal quantities do not mismatch", func(t *testing.T) {
		in := goodInput() // 135 contracts x 10 = 1350 CHZ = spot 1350
		r := Evaluate(in)
		if hasCode(r.BreachCodes, planhealth.CodeLegMismatch) || r.LegMismatchPct != 0 {
			t.Fatalf("codes %v, mismatch %v", r.BreachCodes, r.LegMismatchPct)
		}
		// Taking the 135 contracts as base units would be 90% off; that is the bug this guards.
		in.FuturesState.ContractSize = 1
		if r := Evaluate(in); !hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("a real mismatch went unseen: %v", r.BreachCodes)
		}
	})
	t.Run("0.01x contract (BTC on OKX)", func(t *testing.T) {
		in := goodInput()
		in.SpotState.Quantity, in.SpotState.Price, in.SpotState.ValueUSDT = 0.5, 60000, 30000
		in.FuturesState.Contracts, in.FuturesState.ContractSize, in.FuturesState.MarkPrice, in.FuturesState.NotionalUSDT = 50, 0.01, 60000, 30000
		if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("50 contracts x 0.01 = 0.5 BTC matches the spot leg: %v", r.BreachCodes)
		}
		in.FuturesState.Contracts = 40 // 0.4 BTC against 0.5 BTC: 20% apart
		r := Evaluate(in)
		if !hasCode(r.BreachCodes, planhealth.CodeLegMismatch) || !floatAlmostEqual(r.LegMismatchPct, 20, 1e-9) {
			t.Fatalf("codes %v, mismatch %v, want leg_mismatch at 20%%", r.BreachCodes, r.LegMismatchPct)
		}
	})
	t.Run("distinct from price drift", func(t *testing.T) {
		in := goodInput() // equal quantities, but the futures mark is 10% away from spot
		in.FuturesState.NotionalUSDT = 44.55 * 1.1
		r := Evaluate(in)
		if !hasCode(r.BreachCodes, planhealth.CodeDeltaDriftHigh) || hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("codes %v: value drift without a quantity mismatch", r.BreachCodes)
		}
	})
	t.Run("threshold is MaxDeltaDriftPct", func(t *testing.T) {
		in := goodInput()
		in.FuturesState.Contracts = 135 * 0.98 // 2% short, policy 3%
		if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("2%% is inside 3%%: %v", r.BreachCodes)
		}
		in.FuturesState.Contracts = 135 * 0.95
		if r := Evaluate(in); !hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("5%% is outside 3%%: %v", r.BreachCodes)
		}
	})
	t.Run("not measured without the figures", func(t *testing.T) {
		in := goodInput()
		in.FuturesState.ContractSize = 0 // unknown: never guessed
		in.FuturesState.Contracts = 1
		if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("unknown contract size must not be guessed: %v", r.BreachCodes)
		}
		in = goodInput()
		in.SpotState.Quantity = 0
		if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeLegMismatch) {
			t.Fatalf("no spot quantity: %v", r.BreachCodes)
		}
	})
}

func TestEvaluate_ProfitTarget(t *testing.T) {
	in := goodInput()
	in.Plan.RiskPolicy.ProfitTargetUSDT = 10
	in.PnLKnown, in.PnLUSDT = true, 9.99
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeProfitTargetMet) {
		t.Fatalf("below the target: %v", r.BreachCodes)
	}
	in.PnLUSDT = 10
	r := Evaluate(in)
	if !hasCode(r.BreachCodes, planhealth.CodeProfitTargetMet) {
		t.Fatalf("at the target: %v", r.BreachCodes)
	}
	if r.Severity != "warn" { // a notice, not an alarm
		t.Fatalf("severity %q", r.Severity)
	}
	in.PnLKnown = false
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeProfitTargetMet) {
		t.Fatalf("an unknown profit and loss met the target: %v", r.BreachCodes)
	}
	in.PnLKnown, in.Plan.RiskPolicy.ProfitTargetUSDT = true, 0
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeProfitTargetMet) {
		t.Fatalf("no target set: %v", r.BreachCodes)
	}
}

func TestEvaluate_MaxDrawdown(t *testing.T) {
	in := goodInput()
	in.Plan.RiskPolicy.MaxDrawdownUSDT = 5
	in.PnLKnown, in.PnLUSDT = true, -5
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeMaxDrawdownBreached) {
		t.Fatalf("a loss equal to the limit has not passed it: %v", r.BreachCodes)
	}
	in.PnLUSDT = -5.01
	r := Evaluate(in)
	if !hasCode(r.BreachCodes, planhealth.CodeMaxDrawdownBreached) || r.Severity != "danger" {
		t.Fatalf("codes %v severity %q", r.BreachCodes, r.Severity)
	}
	in.PnLKnown = false
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeMaxDrawdownBreached) {
		t.Fatalf("an unknown profit and loss breached the limit: %v", r.BreachCodes)
	}
	in.PnLKnown, in.Plan.RiskPolicy.MaxDrawdownUSDT = true, 0
	if r := Evaluate(in); hasCode(r.BreachCodes, planhealth.CodeMaxDrawdownBreached) {
		t.Fatalf("no limit set: %v", r.BreachCodes)
	}
}

func TestEvaluate_RecoveryRequired(t *testing.T) {
	in := goodInput()
	in.Plan.Status = PlanStatusRecoveryRequired
	r := Evaluate(in)
	if !hasCode(r.BreachCodes, planhealth.CodeRecoveryRequired) || r.Severity != "danger" {
		t.Fatalf("codes %v severity %q", r.BreachCodes, r.Severity)
	}
	// It still reports alongside a data failure, which returns early.
	in.FuturesState.Available = false
	r = Evaluate(in)
	if !hasCode(r.BreachCodes, planhealth.CodeDataUnavailable) || !hasCode(r.BreachCodes, planhealth.CodeRecoveryRequired) || r.Severity != "critical" {
		t.Fatalf("codes %v severity %q", r.BreachCodes, r.Severity)
	}
	if IsCriticalBreachCode(planhealth.CodeRecoveryRequired) {
		t.Fatal("recovery_required is not a critical code")
	}
}

// Severity and the recommended action are unchanged for the codes that existed.
func TestEvaluate_ExistingSeverityAndActionUnchanged(t *testing.T) {
	cases := []struct {
		codes    []string
		severity string
		action   string
	}{
		{[]string{"liquidation_distance_low", "margin_critical"}, "critical", "Liquidation distance is dangerously low. Reduce position or add margin immediately."},
		{[]string{"margin_critical"}, "critical", "Margin ratio is critical. Consider reducing position size or unwinding immediately."},
		{[]string{"funding_negative"}, "danger", "Plan health has degraded. Review position and consider unwinding or rebalancing."},
		{[]string{"delta_drift_high"}, "danger", "Delta drift has exceeded policy threshold. Rebalance positions to restore hedge."},
		{[]string{"funding_reversal"}, "danger", "Funding rates are reversing. Monitor closely; consider unwinding if reversal sustains."},
		{[]string{"funding_below_min"}, "warn", "Current funding rate is below policy minimum. Plan profitability is declining."},
		{[]string{"exit_spread_target_met"}, "warn", "Exit spread target met. Consider unwinding to lock in gains from basis convergence."},
	}
	for _, c := range cases {
		if got := determineSeverity(c.codes); got != c.severity {
			t.Errorf("%v: severity %q, want %q", c.codes, got, c.severity)
		}
		if got := recommendAction(c.codes, MonitorSnapshot{}); got != c.action {
			t.Errorf("%v: action %q, want %q", c.codes, got, c.action)
		}
	}
	for code, want := range map[string]bool{
		"data_unavailable": true, "margin_critical": true, "liquidation_distance_low": true,
		"funding_negative": false, "leg_mismatch": false, "profit_target_met": false,
	} {
		if IsCriticalBreachCode(code) != want {
			t.Errorf("IsCriticalBreachCode(%q) != %v", code, want)
		}
	}
	if labelForScore(75) != HealthLabelHealthy || labelForScore(40) != HealthLabelDanger {
		t.Error("label thresholds moved")
	}
}
