package deltaneutral

import (
	"testing"
	"time"
)

func TestDeltaDrift(t *testing.T) {
	tests := []struct {
		name       string
		spotVal    float64
		futuresVal float64
		expected   float64
	}{
		{
			name:       "balanced",
			spotVal:    10000,
			futuresVal: 10000,
			expected:   0,
		},
		{
			name:       "50% mismatch",
			spotVal:    10000,
			futuresVal: 5000,
			expected:   50,
		},
		{
			name:       "zero spot",
			spotVal:    0,
			futuresVal: 10000,
			expected:   100,
		},
		{
			name:       "zero both",
			spotVal:    0,
			futuresVal: 0,
			expected:   0,
		},
		{
			name:       "small drift",
			spotVal:    10000,
			futuresVal: 10100,
			expected:   1,
		},
		{
			name:       "negative notional",
			spotVal:    10000,
			futuresVal: -10000,
			expected:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeDeltaDrift(tt.spotVal, tt.futuresVal)
			if !floatAlmostEqual(got, tt.expected, 0.1) {
				t.Errorf("computeDeltaDrift(%f, %f) = %f, want %f", tt.spotVal, tt.futuresVal, got, tt.expected)
			}
		})
	}
}

func TestLiquidationDistance(t *testing.T) {
	tests := []struct {
		name     string
		mark     float64
		liq      float64
		expected float64
	}{
		{
			name:     "50% distance",
			mark:     100,
			liq:      50,
			expected: 50,
		},
		{
			name:     "10% distance",
			mark:     100,
			liq:      90,
			expected: 10,
		},
		{
			name:     "zero mark",
			mark:     0,
			liq:      50,
			expected: 100,
		},
		{
			name:     "zero liq",
			mark:     100,
			liq:      0,
			expected: 100,
		},
		{
			name:     "very close",
			mark:     100,
			liq:      99.5,
			expected: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeLiquidationDistance(tt.mark, tt.liq)
			if !floatAlmostEqual(got, tt.expected, 0.1) {
				t.Errorf("computeLiquidationDistance(%f, %f) = %f, want %f", tt.mark, tt.liq, got, tt.expected)
			}
		})
	}
}

func TestClassifyFunding(t *testing.T) {
	tests := []struct {
		name     string
		fundInfo FundingInfo
		policy   RiskPolicy
		expected string
	}{
		{
			name: "unavailable",
			fundInfo: FundingInfo{
				Available:   false,
				CurrentRate: 0.001,
			},
			policy:   DefaultRiskPolicy(),
			expected: "unavailable",
		},
		{
			name: "positive",
			fundInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.001,
				RecentRates: []float64{0.0009, 0.0008, 0.001},
			},
			policy:   DefaultRiskPolicy(),
			expected: "positive",
		},
		{
			name: "negative (without reversal cycles)",
			fundInfo: FundingInfo{
				Available:   true,
				CurrentRate: -0.0005,
				RecentRates: []float64{0.0001},
			},
			policy:   DefaultRiskPolicy(),
			expected: "negative",
		},
		{
			name: "below minimum",
			fundInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.00001,
				RecentRates: []float64{0.0001, 0.00002, 0.00001},
			},
			policy: RiskPolicy{
				MinFundingRate:        0.0001,
				FundingReversalCycles: 2,
			},
			expected: "watch",
		},
		{
			name: "funding reversal",
			fundInfo: FundingInfo{
				Available:   true,
				CurrentRate: -0.0001,
				RecentRates: []float64{0.0001, -0.00005, -0.0001},
			},
			policy: RiskPolicy{
				MinFundingRate:        0.00001,
				FundingReversalCycles: 2,
			},
			expected: "reversing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFunding(tt.fundInfo, tt.policy)
			if got != tt.expected {
				t.Errorf("classifyFunding(...) = %s, want %s", got, tt.expected)
			}
		})
	}
}

func TestClassifyMargin(t *testing.T) {
	tests := []struct {
		name        string
		marginRatio float64
		liqDistance float64
		policy      RiskPolicy
		expected    string
	}{
		{
			name:        "safe",
			marginRatio: 100,
			liqDistance: 50,
			policy: RiskPolicy{
				MinLiquidationDistancePct: 25,
			},
			expected: "safe",
		},
		{
			name:        "watch",
			marginRatio: 30,
			liqDistance: 40,
			policy: RiskPolicy{
				MinLiquidationDistancePct: 25,
			},
			expected: "watch",
		},
		{
			name:        "danger",
			marginRatio: 50,
			liqDistance: 20,
			policy: RiskPolicy{
				MinLiquidationDistancePct: 25,
			},
			expected: "danger",
		},
		{
			name:        "critical",
			marginRatio: 10,
			liqDistance: 10,
			policy: RiskPolicy{
				MinLiquidationDistancePct: 25,
			},
			expected: "critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMargin(tt.marginRatio, tt.liqDistance, tt.policy)
			if got != tt.expected {
				t.Errorf("classifyMargin(%f, %f, ...) = %s, want %s", tt.marginRatio, tt.liqDistance, got, tt.expected)
			}
		})
	}
}

func TestHealthScoreComponents(t *testing.T) {
	tests := []struct {
		name               string
		fundingState       string
		marginState        string
		deltaDriftPct      float64
		liquidationDistPct float64
		marginRatioPct     float64
		policy             RiskPolicy
		crossExchange      bool
		fundingRate        float64
		minScore           int
		maxScore           int
	}{
		{
			name:               "healthy plan",
			fundingState:       "positive",
			marginState:        "safe",
			deltaDriftPct:      0.5,
			liquidationDistPct: 50,
			marginRatioPct:     100,
			policy:             DefaultRiskPolicy(),
			crossExchange:      false,
			fundingRate:        0.001,
			minScore:           75,
			maxScore:           100,
		},
		{
			name:               "watch plan",
			fundingState:       "watch",
			marginState:        "watch",
			deltaDriftPct:      5,
			liquidationDistPct: 30,
			marginRatioPct:     40,
			policy:             DefaultRiskPolicy(),
			crossExchange:      false,
			fundingRate:        0.00001,
			minScore:           30, // 5 funding + 15 margin + 8 delta + 10 exchange: the 25 free points are gone
			maxScore:           50,
		},
		{
			name:               "danger plan",
			fundingState:       "negative",
			marginState:        "danger",
			deltaDriftPct:      15,
			liquidationDistPct: 20,
			marginRatioPct:     20,
			policy:             DefaultRiskPolicy(),
			crossExchange:      false,
			fundingRate:        -0.0001,
			minScore:           10,
			maxScore:           50,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeHealthScore(
				tt.fundingState,
				tt.marginState,
				tt.deltaDriftPct,
				tt.liquidationDistPct,
				tt.marginRatioPct,
				tt.policy,
				tt.crossExchange,
				tt.fundingRate,
			)
			if got < tt.minScore || got > tt.maxScore {
				t.Errorf("computeHealthScore(...) = %d, want in range [%d, %d]", got, tt.minScore, tt.maxScore)
			}
		})
	}
}

func TestLabelForScore(t *testing.T) {
	tests := []struct {
		score    int
		expected string
	}{
		{95, HealthLabelExcellent},
		{90, HealthLabelExcellent},
		{89, HealthLabelHealthy},
		{75, HealthLabelHealthy},
		{74, HealthLabelWatch},
		{60, HealthLabelWatch},
		{59, HealthLabelDanger},
		{40, HealthLabelDanger},
		{39, HealthLabelCritical},
		{0, HealthLabelCritical},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := labelForScore(tt.score)
			if got != tt.expected {
				t.Errorf("labelForScore(%d) = %s, want %s", tt.score, got, tt.expected)
			}
		})
	}
}

func TestDetectBreaches(t *testing.T) {
	policy := RiskPolicy{
		MinFundingRate:            0.00005,
		MaxDeltaDriftPct:          3,
		MinLiquidationDistancePct: 25,
		FundingReversalCycles:     2,
	}

	tests := []struct {
		name                string
		deltaDrift          float64
		liquidationDist     float64
		fundingState        string
		marginState         string
		fundingInfo         FundingInfo
		expectedBreaches    []string
		shouldContainBreach string
	}{
		{
			name:            "liquidation distance low",
			deltaDrift:      1,
			liquidationDist: 20,
			fundingState:    "positive",
			marginState:     "safe",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.0001,
			},
			expectedBreaches:    []string{"liquidation_distance_low"},
			shouldContainBreach: "liquidation_distance_low",
		},
		{
			name:            "delta drift high",
			deltaDrift:      5,
			liquidationDist: 50,
			fundingState:    "positive",
			marginState:     "safe",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.0001,
			},
			expectedBreaches:    []string{"delta_drift_high"},
			shouldContainBreach: "delta_drift_high",
		},
		{
			name:            "funding negative",
			deltaDrift:      1,
			liquidationDist: 50,
			fundingState:    "negative",
			marginState:     "safe",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: -0.00001,
			},
			expectedBreaches:    []string{"funding_negative"},
			shouldContainBreach: "funding_negative",
		},
		{
			name:            "funding reversal",
			deltaDrift:      1,
			liquidationDist: 50,
			fundingState:    "reversing",
			marginState:     "safe",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: -0.00001,
				RecentRates: []float64{0.0001, -0.00005, -0.0001},
			},
			expectedBreaches:    []string{"funding_reversal"},
			shouldContainBreach: "funding_reversal",
		},
		{
			name:            "margin danger",
			deltaDrift:      1,
			liquidationDist: 50,
			fundingState:    "positive",
			marginState:     "danger",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.0001,
			},
			expectedBreaches:    []string{"margin_danger"},
			shouldContainBreach: "margin_danger",
		},
		{
			name:            "no breaches",
			deltaDrift:      1,
			liquidationDist: 50,
			fundingState:    "positive",
			marginState:     "safe",
			fundingInfo: FundingInfo{
				Available:   true,
				CurrentRate: 0.0001,
			},
			expectedBreaches: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectBreaches(
				tt.deltaDrift,
				tt.liquidationDist,
				tt.fundingState,
				tt.marginState,
				tt.fundingInfo,
				policy,
				0,           // exitSpreadPct: disabled in these existing tests
				ExitRules{}, // no exit spread target
			)

			if len(tt.expectedBreaches) == 0 && len(got) > 0 {
				t.Errorf("detectBreaches(...) returned %v, expected no breaches", got)
			}

			if tt.shouldContainBreach != "" {
				found := false
				for _, code := range got {
					if code == tt.shouldContainBreach {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("detectBreaches(...) = %v, should contain %s", got, tt.shouldContainBreach)
				}
			}
		})
	}
}

func TestEvaluateDataUnavailable(t *testing.T) {
	now := time.Now()
	plan := Plan{
		ID:              1,
		Name:            "test",
		Status:          PlanStatusActive,
		SpotProvider:    "binance",
		FuturesProvider: "binance",
		RiskPolicy:      DefaultRiskPolicy(),
	}
	plan.RiskPolicy.EscalateOnDataFailure = true

	input := EvaluationInput{
		Plan: plan,
		SpotState: SpotState{
			Available: false,
		},
		FuturesState: FuturesState{
			Available: true,
		},
		FundingInfo: FundingInfo{
			Available: true,
		},
		Now: now,
	}

	result := Evaluate(input)

	if result.DataStatus != DataStatusError {
		t.Errorf("Expected DataStatus=%s, got %s", DataStatusError, result.DataStatus)
	}

	if !result.ThresholdBreached {
		t.Errorf("Expected ThresholdBreached=true")
	}

	found := false
	for _, code := range result.BreachCodes {
		if code == "data_unavailable" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Expected breach code 'data_unavailable' in %v", result.BreachCodes)
	}

	if result.Severity != "critical" {
		t.Errorf("Expected Severity='critical', got %s", result.Severity)
	}

	if result.Snapshot.DataStatus != DataStatusError {
		t.Errorf("Expected Snapshot.DataStatus=%s, got %s", DataStatusError, result.Snapshot.DataStatus)
	}
}

// TestEvaluateZeroMarkPriceEscalates verifies M1: an active plan whose futures leg
// reports Available=true but a zero mark price (position could not be read) is treated
// as a data failure, not a misleading "100% safe" liquidation distance.
func TestEvaluateZeroMarkPriceEscalates(t *testing.T) {
	now := time.Now()
	plan := Plan{
		ID:              1,
		Name:            "test",
		Status:          PlanStatusActive,
		SpotProvider:    "binance",
		FuturesProvider: "binance",
		RiskPolicy:      DefaultRiskPolicy(),
	}
	plan.RiskPolicy.EscalateOnDataFailure = true

	input := EvaluationInput{
		Plan:      plan,
		SpotState: SpotState{Available: true, Price: 50000, Quantity: 1, ValueUSDT: 50000},
		FuturesState: FuturesState{
			Available:    true,
			MarkPrice:    0, // zero mark price → position unreadable
			NotionalUSDT: 50000,
		},
		FundingInfo: FundingInfo{Available: true, CurrentRate: 0.0001},
		Now:         now,
	}

	result := Evaluate(input)

	if result.DataStatus != DataStatusError {
		t.Errorf("Expected DataStatus=%s, got %s", DataStatusError, result.DataStatus)
	}
	found := false
	for _, code := range result.BreachCodes {
		if code == "data_unavailable" {
			found = true
		}
	}
	if !found {
		t.Errorf("Expected breach code 'data_unavailable' for zero mark price, got %v", result.BreachCodes)
	}
	if result.Severity != "critical" {
		t.Errorf("Expected Severity='critical', got %s", result.Severity)
	}
}

// TestIsCriticalBreachCode verifies M2's critical-code set used to bypass alert cooldown.
func TestIsCriticalBreachCode(t *testing.T) {
	critical := []string{"data_unavailable", "margin_critical", "liquidation_distance_low"}
	for _, c := range critical {
		if !IsCriticalBreachCode(c) {
			t.Errorf("expected %q to be critical", c)
		}
	}
	nonCritical := []string{"funding_negative", "funding_reversal", "margin_danger", "delta_drift_high", "funding_below_min", ""}
	for _, c := range nonCritical {
		if IsCriticalBreachCode(c) {
			t.Errorf("expected %q to be non-critical", c)
		}
	}
}

func TestEvaluateHealthyPlan(t *testing.T) {
	now := time.Now()
	plan := Plan{
		ID:              1,
		Name:            "test",
		Status:          PlanStatusActive,
		SpotProvider:    "binance",
		FuturesProvider: "binance",
		RiskPolicy:      DefaultRiskPolicy(),
	}

	input := EvaluationInput{
		Plan: plan,
		SpotState: SpotState{
			Available: true,
			Price:     50000,
			Quantity:  1,
			ValueUSDT: 50000,
		},
		FuturesState: FuturesState{
			Available:         true,
			MarkPrice:         50000,
			Contracts:         1,
			NotionalUSDT:      50000,
			UnrealizedPnLUSDT: 100,
			LiquidationPrice:  30000,
			MarginRatioPct:    80,
		},
		FundingInfo: FundingInfo{
			Available:         true,
			CurrentRate:       0.0001,
			EstimatedNextUSDT: 5,
			RecentRates:       []float64{0.00009, 0.0001, 0.00011},
			NextFundingTime:   now.Add(time.Hour),
		},
		Now: now,
	}

	result := Evaluate(input)

	if !result.ThresholdBreached {
		// Healthy plan should not breach
		if len(result.BreachCodes) > 0 {
			t.Logf("Healthy plan had breaches: %v", result.BreachCodes)
		}
	}

	if result.Snapshot.HealthScore < 75 {
		t.Errorf("Expected HealthScore >= 75, got %d", result.Snapshot.HealthScore)
	}

	label := result.Snapshot.HealthLabel
	if label != HealthLabelHealthy && label != HealthLabelExcellent {
		t.Errorf("Expected HealthLabel in [healthy, excellent], got %s", label)
	}

	if result.Snapshot.CrossExchange {
		t.Errorf("Expected CrossExchange=false for same-provider plan")
	}
}

func TestEvaluateCrossExchange(t *testing.T) {
	now := time.Now()
	plan := Plan{
		ID:              1,
		Name:            "test-cross",
		Status:          PlanStatusActive,
		SpotProvider:    "binance",
		FuturesProvider: "okx",
		RiskPolicy:      DefaultRiskPolicy(),
	}

	input := EvaluationInput{
		Plan: plan,
		SpotState: SpotState{
			Available: true,
			Price:     50000,
			Quantity:  1,
			ValueUSDT: 50000,
		},
		FuturesState: FuturesState{
			Available:         true,
			MarkPrice:         50000,
			Contracts:         1,
			NotionalUSDT:      50000,
			UnrealizedPnLUSDT: 100,
			LiquidationPrice:  30000,
			MarginRatioPct:    80,
		},
		FundingInfo: FundingInfo{
			Available:         true,
			CurrentRate:       0.0001,
			EstimatedNextUSDT: 5,
			RecentRates:       []float64{0.00009, 0.0001, 0.00011},
			NextFundingTime:   now.Add(time.Hour),
		},
		Now: now,
	}

	result := Evaluate(input)

	if !result.Snapshot.CrossExchange {
		t.Errorf("Expected CrossExchange=true for different providers")
	}

	// Score should be lower for cross-exchange (due to exchange risk penalty)
	// Compare with same-exchange scenario
	sameExchangePlan := plan
	sameExchangePlan.FuturesProvider = "binance"
	sameExchangeInput := input
	sameExchangeInput.Plan = sameExchangePlan

	sameResult := Evaluate(sameExchangeInput)

	if result.Snapshot.HealthScore >= sameResult.Snapshot.HealthScore {
		t.Errorf("Cross-exchange score %d should be lower than same-exchange score %d",
			result.Snapshot.HealthScore, sameResult.Snapshot.HealthScore)
	}
}

func TestEvaluateIntegration(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		plan    Plan
		input   EvaluationInput
		checkFn func(t *testing.T, result HealthEvaluation)
	}{
		{
			name: "liquidation distance breach",
			plan: Plan{
				ID:              1,
				Name:            "liq-breach",
				Status:          PlanStatusActive,
				SpotProvider:    "binance",
				FuturesProvider: "binance",
				RiskPolicy: RiskPolicy{
					MinLiquidationDistancePct: 25,
					MaxDeltaDriftPct:          3,
					FundingReversalCycles:     2,
					EscalateOnDataFailure:     true,
				},
			},
			input: EvaluationInput{
				Plan: Plan{
					ID:              1,
					Name:            "liq-breach",
					Status:          PlanStatusActive,
					SpotProvider:    "binance",
					FuturesProvider: "binance",
					RiskPolicy: RiskPolicy{
						MinLiquidationDistancePct: 25,
						MaxDeltaDriftPct:          3,
						FundingReversalCycles:     2,
						EscalateOnDataFailure:     true,
					},
				},
				SpotState: SpotState{
					Available: true,
					ValueUSDT: 10000,
				},
				FuturesState: FuturesState{
					Available:        true,
					MarkPrice:        100,
					NotionalUSDT:     10000,
					LiquidationPrice: 99,
					MarginRatioPct:   20,
				},
				FundingInfo: FundingInfo{
					Available:   true,
					CurrentRate: 0.0001,
					RecentRates: []float64{0.00009, 0.0001},
				},
				Now: now,
			},
			checkFn: func(t *testing.T, result HealthEvaluation) {
				if !result.ThresholdBreached {
					t.Errorf("Expected breach for low liquidation distance")
				}
				found := false
				for _, code := range result.BreachCodes {
					if code == "liquidation_distance_low" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected 'liquidation_distance_low' breach, got %v", result.BreachCodes)
				}
			},
		},
		{
			name: "delta drift breach",
			plan: Plan{
				ID:              2,
				Name:            "delta-breach",
				Status:          PlanStatusActive,
				SpotProvider:    "binance",
				FuturesProvider: "binance",
				RiskPolicy: RiskPolicy{
					MinLiquidationDistancePct: 25,
					MaxDeltaDriftPct:          3,
					FundingReversalCycles:     2,
					EscalateOnDataFailure:     true,
				},
			},
			input: EvaluationInput{
				Plan: Plan{
					ID:              2,
					Name:            "delta-breach",
					Status:          PlanStatusActive,
					SpotProvider:    "binance",
					FuturesProvider: "binance",
					RiskPolicy: RiskPolicy{
						MinLiquidationDistancePct: 25,
						MaxDeltaDriftPct:          3,
						FundingReversalCycles:     2,
						EscalateOnDataFailure:     true,
					},
				},
				SpotState: SpotState{
					Available: true,
					ValueUSDT: 10000,
				},
				FuturesState: FuturesState{
					Available:        true,
					MarkPrice:        100,
					NotionalUSDT:     5000,
					LiquidationPrice: 50,
					MarginRatioPct:   80,
				},
				FundingInfo: FundingInfo{
					Available:   true,
					CurrentRate: 0.0001,
					RecentRates: []float64{0.00009, 0.0001},
				},
				Now: now,
			},
			checkFn: func(t *testing.T, result HealthEvaluation) {
				if !result.ThresholdBreached {
					t.Errorf("Expected breach for high delta drift")
				}
				found := false
				for _, code := range result.BreachCodes {
					if code == "delta_drift_high" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected 'delta_drift_high' breach, got %v", result.BreachCodes)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Evaluate(tt.input)
			tt.checkFn(t, result)
		})
	}
}

// Helper function
func floatAlmostEqual(a, b, tolerance float64) bool {
	if a > b {
		return a-b < tolerance
	}
	return b-a < tolerance
}

// TestExitSpreadTargetBreach verifies that exit_spread_target_met is raised
// when exit spread >= target and is absent when disabled (target=0) or below target.
func TestExitSpreadTargetBreach(t *testing.T) {
	tests := []struct {
		name             string
		targetExitSpread float64
		actualExitSpread float64
		wantBreach       bool
		wantRecommend    string // substring expected in RecommendedAction
	}{
		{
			name:             "exit spread exceeds target",
			targetExitSpread: 0.1,
			actualExitSpread: 0.15,
			wantBreach:       true,
			wantRecommend:    "unwind",
		},
		{
			name:             "exit spread equals target exactly",
			targetExitSpread: 0.1,
			actualExitSpread: 0.1,
			wantBreach:       true,
			wantRecommend:    "unwind",
		},
		{
			name:             "exit spread below target",
			targetExitSpread: 0.1,
			actualExitSpread: 0.05,
			wantBreach:       false,
		},
		{
			name:             "target disabled (zero) — never breaches",
			targetExitSpread: 0,
			actualExitSpread: 99.9,
			wantBreach:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			input := EvaluationInput{
				Plan: Plan{
					ID:              1,
					Name:            "test",
					Status:          PlanStatusActive,
					SpotProvider:    "binance",
					FuturesProvider: "binance",
					RiskPolicy:      DefaultRiskPolicy(),
					ExitRules: ExitRules{
						TargetExitSpreadPct: tt.targetExitSpread,
					},
				},
				SpotState: SpotState{
					Available: true,
					Price:     50000,
					Quantity:  1,
					ValueUSDT: 50000,
				},
				FuturesState: FuturesState{
					Available:         true,
					MarkPrice:         50000,
					Contracts:         1,
					NotionalUSDT:      50000,
					UnrealizedPnLUSDT: 0,
					LiquidationPrice:  30000,
					MarginRatioPct:    80,
				},
				FundingInfo: FundingInfo{
					Available:   true,
					CurrentRate: 0.0001,
				},
				EntrySpreadPct: -0.1,
				ExitSpreadPct:  tt.actualExitSpread,
				Now:            now,
			}

			result := Evaluate(input)

			// Verify breach code presence
			hasCode := false
			for _, code := range result.BreachCodes {
				if code == "exit_spread_target_met" {
					hasCode = true
					break
				}
			}
			if tt.wantBreach && !hasCode {
				t.Errorf("expected breach 'exit_spread_target_met', got codes: %v", result.BreachCodes)
			}
			if !tt.wantBreach && hasCode {
				t.Errorf("unexpected breach 'exit_spread_target_met' (target=%.4f, actual=%.4f)", tt.targetExitSpread, tt.actualExitSpread)
			}

			// Verify recommendation substring when breach expected
			if tt.wantBreach && tt.wantRecommend != "" {
				if !containsSubstring(result.RecommendedAction, tt.wantRecommend) {
					t.Errorf("RecommendedAction = %q, want substring %q", result.RecommendedAction, tt.wantRecommend)
				}
			}

			// Verify spread values propagated to snapshot
			if result.Snapshot.ExitSpreadPct != tt.actualExitSpread {
				t.Errorf("Snapshot.ExitSpreadPct = %.4f, want %.4f", result.Snapshot.ExitSpreadPct, tt.actualExitSpread)
			}
			if result.Snapshot.EntrySpreadPct != -0.1 {
				t.Errorf("Snapshot.EntrySpreadPct = %.4f, want -0.1", result.Snapshot.EntrySpreadPct)
			}
		})
	}
}

// TestExitSpreadNotCritical verifies that exit_spread_target_met is not a critical
// breach code (it's a profit-taking signal, not a risk alert).
func TestExitSpreadNotCritical(t *testing.T) {
	if IsCriticalBreachCode("exit_spread_target_met") {
		t.Error("exit_spread_target_met should not be a critical breach code")
	}
}

// containsSubstring is a helper for substring checks in test assertions.
func containsSubstring(s, substr string) bool {
	if substr == "" {
		return true
	}
	return len(s) >= len(substr) && func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}()
}
