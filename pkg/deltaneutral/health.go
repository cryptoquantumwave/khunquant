package deltaneutral

import (
	"math"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
)

// Evaluate is the deterministic health evaluator for delta-neutral plans.
// It takes an EvaluationInput and returns a HealthEvaluation with no I/O, network, or DB.
func Evaluate(input EvaluationInput) HealthEvaluation {
	result := HealthEvaluation{
		Snapshot: MonitorSnapshot{
			PlanID:                   input.Plan.ID,
			CheckedAt:                input.Now,
			SpotPrice:                input.SpotState.Price,
			SpotQuantity:             input.SpotState.Quantity,
			SpotValueUSDT:            input.SpotState.ValueUSDT,
			FuturesMarkPrice:         input.FuturesState.MarkPrice,
			FuturesContracts:         input.FuturesState.Contracts,
			FuturesNotionalUSDT:      input.FuturesState.NotionalUSDT,
			FuturesUnrealizedPnLUSDT: input.FuturesState.UnrealizedPnLUSDT,
			CurrentFundingRate:       input.FundingInfo.CurrentRate,
			EstimatedNextFundingUSDT: input.FundingInfo.EstimatedNextUSDT,
			LiquidationPrice:         input.FuturesState.LiquidationPrice,
			MarginRatioPct:           input.FuturesState.MarginRatioPct,
			CrossExchange:            input.Plan.SpotProvider != input.Plan.FuturesProvider,
			DataStatus:               DataStatusOK,
		},
		DataStatus: DataStatusOK,
		PnLUSDT:    input.PnLUSDT,
		PnLKnown:   input.PnLKnown && finiteNum(input.PnLUSDT),
	}

	// Step 1: Check data availability first
	if input.Plan.Status != PlanStatusDraft && input.Plan.Status != PlanStatusReady {
		// For active plans, check data availability. A zero mark price means the
		// futures position could not be read, so liquidation distance cannot be
		// computed — treat it as a data failure rather than letting
		// computeLiquidationDistance report a misleading "100% safe".
		futuresDataMissing := !input.FuturesState.Available || input.FuturesState.MarkPrice == 0
		if !input.SpotState.Available || futuresDataMissing || !input.FundingInfo.Available {
			if input.Plan.RiskPolicy.EscalateOnDataFailure {
				result.DataStatus = DataStatusError
				result.ThresholdBreached = true
				codes := []string{planhealth.CodeDataUnavailable}
				if input.Plan.Status == PlanStatusRecoveryRequired {
					codes = append(codes, planhealth.CodeRecoveryRequired)
				}
				result.BreachCodes = codes
				result.Severity = planhealth.Severity(codes)
				result.RecommendedAction = planhealth.RecommendedAction(codes)
				result.Snapshot.DataStatus = DataStatusError
				result.Snapshot.ThresholdBreached = true
				result.Snapshot.BreachCodes = codes
				return result
			}
			// If escalation is off, continue with unavailable status
			result.DataStatus = DataStatusUnavailable
			result.Snapshot.DataStatus = DataStatusUnavailable
		}
	}

	// Step 2: Normalize spot and futures values
	spotValueUSDT := input.SpotState.ValueUSDT
	futuresNotionalUSDT := math.Abs(input.FuturesState.NotionalUSDT)

	// Step 3: Compute delta drift
	deltaDrift := computeDeltaDrift(spotValueUSDT, futuresNotionalUSDT)
	result.Snapshot.DeltaDriftPct = deltaDrift

	// Step 3.5: Propagate spread values from input
	result.Snapshot.EntrySpreadPct = input.EntrySpreadPct
	result.Snapshot.ExitSpreadPct = input.ExitSpreadPct

	// Step 4: Compute liquidation distance
	liquidationDistance := computeLiquidationDistance(input.FuturesState.MarkPrice, input.FuturesState.LiquidationPrice)
	result.Snapshot.LiquidationDistancePct = liquidationDistance

	// Step 5: Classify funding state
	fundingState := classifyFunding(input.FundingInfo, input.Plan.RiskPolicy)
	result.Snapshot.FundingState = fundingState

	// Step 6: Classify margin state
	marginState := classifyMargin(input.FuturesState.MarginRatioPct, liquidationDistance, input.Plan.RiskPolicy)
	result.Snapshot.MarginState = marginState

	// Step 7: Cross-exchange flag (already set above)
	crossExchange := result.Snapshot.CrossExchange

	// Step 8: Compute health score
	healthScore := computeHealthScore(
		fundingState,
		marginState,
		deltaDrift,
		liquidationDistance,
		input.FuturesState.MarginRatioPct,
		input.Plan.RiskPolicy,
		crossExchange,
		input.FundingInfo.CurrentRate,
	)
	// The two components that need data of their own score nothing without it.
	healthScore = clampScore(healthScore + liquidityPoints(input) + profitProgressPoints(input))
	result.Snapshot.HealthScore = healthScore
	result.Snapshot.HealthLabel = labelForScore(healthScore)

	// Step 9: Detect threshold breaches
	breachCodes := detectBreaches(
		deltaDrift,
		liquidationDistance,
		fundingState,
		marginState,
		input.FundingInfo,
		input.Plan.RiskPolicy,
		input.ExitSpreadPct,
		input.Plan.ExitRules,
	)
	// Breaches that need more than the figures above: the quantities of the legs,
	// the plan's status, and its profit and loss against the policy.
	var legBreach bool
	result.LegMismatchPct, legBreach = legMismatch(input)
	if legBreach {
		breachCodes = append(breachCodes, planhealth.CodeLegMismatch)
	}
	if input.Plan.Status == PlanStatusRecoveryRequired {
		breachCodes = append(breachCodes, planhealth.CodeRecoveryRequired)
	}
	if input.PnLKnown && finiteNum(input.PnLUSDT) {
		if dd := input.Plan.RiskPolicy.MaxDrawdownUSDT; dd > 0 && -input.PnLUSDT > dd {
			breachCodes = append(breachCodes, planhealth.CodeMaxDrawdownBreached)
		}
		if pt := input.Plan.RiskPolicy.ProfitTargetUSDT; pt > 0 && input.PnLUSDT >= pt {
			breachCodes = append(breachCodes, planhealth.CodeProfitTargetMet)
		}
	}
	result.BreachCodes = breachCodes
	result.ThresholdBreached = len(breachCodes) > 0
	result.Snapshot.BreachCodes = breachCodes
	result.Snapshot.ThresholdBreached = len(breachCodes) > 0

	// Step 10: Determine severity and recommended action
	if len(breachCodes) > 0 {
		result.Severity = determineSeverity(breachCodes)
		result.RecommendedAction = recommendAction(breachCodes, result.Snapshot)
	}

	// Set agent invoked flag to false (the handler decides)
	result.Snapshot.AgentInvoked = false

	return result
}

// computeDeltaDrift computes the delta drift percentage
// Formula: abs(spot_value - abs(futures_notional)) / max(spot_value, abs(futures_notional)) * 100
func computeDeltaDrift(spotValueUSDT, futuresNotionalUSDT float64) float64 {
	spotAbs := math.Abs(spotValueUSDT)
	futuresAbs := math.Abs(futuresNotionalUSDT)

	// Guard against divide-by-zero
	if spotAbs == 0 && futuresAbs == 0 {
		return 0
	}

	maxVal := math.Max(spotAbs, futuresAbs)
	if maxVal == 0 {
		return 0
	}

	drift := math.Abs(spotAbs-futuresAbs) / maxVal * 100
	return math.Min(drift, 100) // Cap at 100%
}

// computeLiquidationDistance computes the liquidation distance percentage
// Formula: abs(mark_price - liquidation_price) / mark_price * 100
func computeLiquidationDistance(markPrice, liquidationPrice float64) float64 {
	// Guard against mark_price == 0
	if markPrice == 0 {
		// If mark price is zero, assume safe (no valid liq price)
		return 100
	}

	// If liquidation price is 0, treat as "no liq price" => safe distance
	if liquidationPrice == 0 {
		return 100
	}

	distance := math.Abs(markPrice-liquidationPrice) / math.Abs(markPrice) * 100
	return math.Min(distance, 100) // Cap at 100%
}

// classifyFunding classifies funding state from FundingInfo
func classifyFunding(fundingInfo FundingInfo, policy RiskPolicy) string {
	if !fundingInfo.Available {
		return "unavailable"
	}

	rate := fundingInfo.CurrentRate

	// Check negative first (single negative takes priority over reversal detection)
	if rate < 0 {
		// Also check for reversal if we have recent rates with N consecutive negatives
		if len(fundingInfo.RecentRates) > 0 {
			consecutiveNeg := 0
			for i := len(fundingInfo.RecentRates) - 1; i >= 0; i-- {
				if fundingInfo.RecentRates[i] < 0 {
					consecutiveNeg++
				} else {
					break
				}
			}
			if consecutiveNeg >= policy.FundingReversalCycles && policy.FundingReversalCycles > 0 {
				return "reversing"
			}
		}
		return "negative"
	}

	// Check below minimum
	if rate < policy.MinFundingRate && policy.MinFundingRate > 0 {
		return "watch"
	}

	// Positive above minimum
	if rate > 0 {
		return "positive"
	}

	return "neutral"
}

// classifyMargin classifies margin state based on ratio and liquidation distance
func classifyMargin(marginRatioPct, liquidationDistancePct float64, policy RiskPolicy) string {
	// Critical: liquidation distance very low
	if liquidationDistancePct < (policy.MinLiquidationDistancePct * 0.5) {
		return "critical"
	}

	// Danger: liquidation distance below minimum
	if liquidationDistancePct < policy.MinLiquidationDistancePct {
		return "danger"
	}

	// Watch: margin ratio is getting tight (< 50%) but liquidation distance is ok
	if marginRatioPct > 0 && marginRatioPct < 50 {
		return "watch"
	}

	// Safe: good margin and liquidation distance
	return "safe"
}

// computeHealthScore computes the part of the 0-100 health score that is judged from
// the figures every check has. Components:
//   - Funding: 20 points
//   - Margin/liquidation: 25 points
//   - Delta balance: 20 points
//   - Exchange/execution risk: 10 points (6 when the legs are on different exchanges)
//
// The other two components, liquidity/slippage (10 points, see liquidityPoints) and
// profit progress (15, see profitProgressPoints), need data of their own and score 0
// without it. They used to be awarded unconditionally ("assume ok for now"), which
// handed every plan 25 points it had not earned.
func computeHealthScore(
	fundingState string,
	marginState string,
	deltaDriftPct float64,
	liquidationDistancePct float64,
	marginRatioPct float64,
	policy RiskPolicy,
	crossExchange bool,
	fundingRate float64,
) int {
	score := 0

	// Funding health: 20 points
	fundingScore := 0
	switch fundingState {
	case "positive":
		fundingScore = 20
	case "neutral":
		fundingScore = 10
	case "watch":
		fundingScore = 5
	case "negative":
		fundingScore = 0
	case "reversing":
		fundingScore = 2
	case "unavailable":
		fundingScore = 0
	}
	score += fundingScore

	// Margin/liquidation health: 25 points
	marginScore := 0
	switch marginState {
	case "safe":
		marginScore = 25
	case "watch":
		marginScore = 15
	case "danger":
		marginScore = 5
	case "critical":
		marginScore = 0
	}
	score += marginScore

	// Delta balance: 20 points
	// Excellent: < 1% drift, Good: < 3%, Watch: < 10%, Poor: >= 10%
	deltaScore := 0
	if deltaDriftPct < 1 {
		deltaScore = 20
	} else if deltaDriftPct < 3 {
		deltaScore = 15
	} else if deltaDriftPct < 10 {
		deltaScore = 8
	} else if deltaDriftPct < 20 {
		deltaScore = 3
	} else {
		deltaScore = 0
	}
	score += deltaScore

	// Exchange/execution risk: 10 points
	exchangeScore := 10
	if crossExchange {
		// Subtract from exchange score for cross-exchange
		exchangeScore = 6
	}
	score += exchangeScore

	return clampScore(score)
}

func clampScore(score int) int {
	return min(max(score, 0), 100)
}

// liquidityPoints scores the liquidity/slippage component (10 points) from a
// measured slippage: full marks within the policy's limit (20 bps when it names
// none), half within twice that, nothing beyond. No measurement, no points.
func liquidityPoints(in EvaluationInput) int {
	if in.Liquidity == nil || !finiteNum(in.Liquidity.SlippageBps) || in.Liquidity.SlippageBps < 0 {
		return 0
	}
	limit := in.Plan.RiskPolicy.MaxSlippageBps
	if limit <= 0 {
		limit = 20
	}
	switch s := in.Liquidity.SlippageBps; {
	case s <= limit:
		return 10
	case s <= 2*limit:
		return 5
	}
	return 0
}

// profitProgressPoints scores the profit progress component (15 points): the share
// of the plan's profit target reached so far. It needs a known profit and loss and a
// target, and scores nothing without both.
func profitProgressPoints(in EvaluationInput) int {
	target := in.Plan.RiskPolicy.ProfitTargetUSDT
	if !in.PnLKnown || !finiteNum(in.PnLUSDT) || !finiteNum(target) || target <= 0 {
		return 0
	}
	share := math.Min(math.Max(in.PnLUSDT/target, 0), 1)
	return int(math.Round(15 * share))
}

func finiteNum(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// legMismatch measures how far the two legs are apart in QUANTITY: the spot quantity
// against the futures base quantity, which is the contract count times the size of one
// contract (a position's Contracts is a count, never a base quantity: 135 contracts of
// CHZ/USDT:USDT on OKX is 1350 CHZ). It is distinct from delta drift, which compares
// the legs' values and so moves with the price basis between spot and futures.
//
// It returns the mismatch in percent of the spot quantity (capped at 100) and whether
// it breaches the policy's MaxDeltaDriftPct. It measures nothing, and so breaches
// nothing, when a leg could not be read, the contract size is unknown or the spot
// quantity is not positive.
func legMismatch(in EvaluationInput) (pct float64, breach bool) {
	if !in.SpotState.Available || !in.FuturesState.Available {
		return 0, false
	}
	spotQty, size, contracts := in.SpotState.Quantity, in.FuturesState.ContractSize, in.FuturesState.Contracts
	for _, v := range []float64{spotQty, size, contracts} {
		if !finiteNum(v) {
			return 0, false
		}
	}
	if spotQty <= 0 || size <= 0 || contracts < 0 {
		return 0, false
	}
	futuresBase := contracts * size
	pct = math.Min(math.Abs(spotQty-futuresBase)/spotQty*100, 100)
	return pct, math.Abs(spotQty-futuresBase)/spotQty*100 > in.Plan.RiskPolicy.MaxDeltaDriftPct
}

// labelForScore returns the health label for a given score
func labelForScore(score int) string { return planhealth.LabelForScore(score) }

// detectBreaches detects threshold breaches and returns breach codes
func detectBreaches(
	deltaDriftPct float64,
	liquidationDistancePct float64,
	fundingState string,
	marginState string,
	fundingInfo FundingInfo,
	policy RiskPolicy,
	exitSpreadPct float64,
	exitRules ExitRules,
) []string {
	var breaches []string

	// Funding breaches
	if fundingState == "negative" {
		breaches = append(breaches, "funding_negative")
	}

	if fundingState == "unavailable" {
		breaches = append(breaches, "funding_unavailable")
	}

	if fundingInfo.Available && fundingInfo.CurrentRate < policy.MinFundingRate && policy.MinFundingRate > 0 {
		breaches = append(breaches, "funding_below_min")
	}

	if fundingState == "reversing" {
		breaches = append(breaches, "funding_reversal")
	}

	// Liquidation distance breach
	if liquidationDistancePct < policy.MinLiquidationDistancePct {
		breaches = append(breaches, "liquidation_distance_low")
	}

	// Delta drift breach
	if deltaDriftPct > policy.MaxDeltaDriftPct {
		breaches = append(breaches, "delta_drift_high")
	}

	// Margin danger/critical
	if marginState == "danger" {
		breaches = append(breaches, "margin_danger")
	} else if marginState == "critical" {
		breaches = append(breaches, "margin_critical")
	}

	// Exit spread target: profit-taking signal (non-critical)
	if exitRules.TargetExitSpreadPct != 0 && exitSpreadPct >= exitRules.TargetExitSpreadPct {
		breaches = append(breaches, "exit_spread_target_met")
	}

	return breaches
}

// IsCriticalBreachCode reports whether a breach code is critical severity: one whose
// condition can deteriorate further under the same code (liquidation distance
// shrinking, margin ratio climbing, a monitor gone blind). Critical codes always map to
// "critical" severity and are protective: the legacy cron monitor never silenced them,
// and the trigger path re-sends them on the protective repeat interval while they
// persist. The definition is in planhealth, where the trigger reads it too.
func IsCriticalBreachCode(code string) bool { return planhealth.IsCritical(code) }

// determineSeverity determines the worst severity among breaches: "critical",
// "danger" or "warn".
func determineSeverity(breachCodes []string) string { return planhealth.Severity(breachCodes) }

// recommendAction returns a recommended action based on breach codes.
func recommendAction(breachCodes []string, _ MonitorSnapshot) string {
	return planhealth.RecommendedAction(breachCodes)
}
