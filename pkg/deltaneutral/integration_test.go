package deltaneutral

import (
	"context"
	"testing"
	"time"
)

// TestIntegrationPlanCreationAndSnapshot tests the complete flow:
// save a healthy Plan (status active), build a healthy EvaluationInput,
// call Evaluate, save the resulting snapshot, then assert LatestSnapshot
// returns it with the right health score/label and ThresholdBreached=false.
func TestIntegrationPlanCreationAndSnapshot(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create and save a healthy plan
	plan := createTestPlan(t, "healthy-plan")
	plan.Status = PlanStatusActive
	plan.SpotProvider = "binance"
	plan.FuturesProvider = "bybit"
	plan.RiskPolicy = DefaultRiskPolicy()

	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Build a healthy EvaluationInput
	now := time.Now()
	input := EvaluationInput{
		Plan: *plan,
		SpotState: SpotState{
			Available: true,
			Price:     2000.0,
			Quantity:  5.0,
			ValueUSDT: 10000.0,
		},
		FuturesState: FuturesState{
			Available:         true,
			MarkPrice:         2000.0,
			Contracts:         5.0,
			NotionalUSDT:      10000.0,
			UnrealizedPnLUSDT: 50.0,
			LiquidationPrice:  1000.0,
			MarginRatioPct:    75.0,
		},
		FundingInfo: FundingInfo{
			Available:         true,
			CurrentRate:       0.00005,
			EstimatedNextUSDT: 50.0,
			RecentRates:       []float64{0.00004, 0.00005, 0.00005},
			NextFundingTime:   now.Add(8 * time.Hour),
		},
		Now: now,
	}

	// Evaluate
	evaluation := Evaluate(input)

	// Assertions on evaluation result
	if evaluation.ThresholdBreached {
		t.Errorf("Expected healthy plan; ThresholdBreached should be false, got true")
	}
	if len(evaluation.BreachCodes) > 0 {
		t.Errorf("Expected no breach codes, got %v", evaluation.BreachCodes)
	}
	if evaluation.DataStatus != DataStatusOK {
		t.Errorf("Expected DataStatus=ok, got %s", evaluation.DataStatus)
	}

	// Save the snapshot
	snapshot := evaluation.Snapshot
	snapshot.PlanID = planID
	snapshot.CreatedAt = now

	snapshotID, err := store.SaveSnapshot(ctx, &snapshot)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Retrieve LatestSnapshot
	latest, err := store.LatestSnapshot(ctx, planID)
	if err != nil {
		t.Fatalf("LatestSnapshot failed: %v", err)
	}

	if latest == nil {
		t.Fatal("LatestSnapshot returned nil")
	}

	// Verify snapshot fields
	if latest.ID != snapshotID {
		t.Errorf("Snapshot ID mismatch: got %d, expected %d", latest.ID, snapshotID)
	}
	if latest.PlanID != planID {
		t.Errorf("PlanID mismatch: got %d, expected %d", latest.PlanID, planID)
	}
	if latest.ThresholdBreached {
		t.Errorf("Expected ThresholdBreached=false, got true")
	}
	// A perfect cross-exchange hedge scores 71 now that liquidity and profit progress
	// are no longer awarded without data (it was 96): "watch", with the thresholds kept.
	if latest.HealthLabel != HealthLabelWatch || latest.HealthScore != 71 {
		t.Errorf("Expected watch/71 for a perfect cross-exchange plan, got %s/%d", latest.HealthLabel, latest.HealthScore)
	}
	if latest.HealthScore <= 0 {
		t.Errorf("Expected positive health score, got %d", latest.HealthScore)
	}

	// List snapshots
	snapshots, err := store.ListSnapshots(ctx, planID, 10, 0)
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}
	if len(snapshots) != 1 {
		t.Errorf("Expected 1 snapshot, got %d", len(snapshots))
	}
}

// TestIntegrationForcedThresholdBreach tests threshold breach scenario:
// build an EvaluationInput that breaches a policy (e.g., liquidation distance below minimum),
// call Evaluate, assert ThresholdBreached=true with expected breach code;
// persist snapshot + Alert and verify LatestAlert returns it.
func TestIntegrationForcedThresholdBreach(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create and save a plan
	plan := createTestPlan(t, "breach-plan")
	plan.Status = PlanStatusActive
	plan.RiskPolicy = DefaultRiskPolicy()

	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Build EvaluationInput that breaches liquidation distance
	now := time.Now()
	input := EvaluationInput{
		Plan: *plan,
		SpotState: SpotState{
			Available: true,
			Price:     2000.0,
			Quantity:  5.0,
			ValueUSDT: 10000.0,
		},
		FuturesState: FuturesState{
			Available:         true,
			MarkPrice:         2000.0,
			Contracts:         5.0,
			NotionalUSDT:      10000.0,
			UnrealizedPnLUSDT: 50.0,
			LiquidationPrice:  1900.0, // Very close to mark price -> low liquidation distance
			MarginRatioPct:    10.0,   // Also low margin
		},
		FundingInfo: FundingInfo{
			Available:         true,
			CurrentRate:       0.00005,
			EstimatedNextUSDT: 50.0,
			RecentRates:       []float64{0.00004, 0.00005, 0.00005},
			NextFundingTime:   now.Add(8 * time.Hour),
		},
		Now: now,
	}

	// Evaluate
	evaluation := Evaluate(input)

	// Assertions
	if !evaluation.ThresholdBreached {
		t.Errorf("Expected ThresholdBreached=true, got false")
	}
	if len(evaluation.BreachCodes) == 0 {
		t.Errorf("Expected breach codes, got empty")
	}

	// Check for expected breach code
	hasLiquidationBreach := false
	for _, code := range evaluation.BreachCodes {
		if code == "liquidation_distance_low" {
			hasLiquidationBreach = true
			break
		}
	}
	if !hasLiquidationBreach {
		t.Errorf("Expected 'liquidation_distance_low' breach code, got %v", evaluation.BreachCodes)
	}

	// Save the snapshot
	snapshot := evaluation.Snapshot
	snapshot.PlanID = planID
	snapshot.CreatedAt = now

	snapshotID, err := store.SaveSnapshot(ctx, &snapshot)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Create and save an Alert based on the breach
	alert := &Alert{
		PlanID:            planID,
		SnapshotID:        &snapshotID,
		TriggeredAt:       now,
		Severity:          evaluation.Severity,
		Code:              "liquidation_distance_low",
		Message:           "Liquidation distance is below policy minimum",
		RecommendedAction: evaluation.RecommendedAction,
		AgentInvoked:      false,
		DeliveredChannel:  "",
		DeliveredChatID:   "",
		CreatedAt:         now,
	}

	alertID, err := store.SaveAlert(ctx, alert)
	if err != nil {
		t.Fatalf("SaveAlert failed: %v", err)
	}

	// Retrieve LatestAlert
	latest, err := store.LatestAlert(ctx, planID)
	if err != nil {
		t.Fatalf("LatestAlert failed: %v", err)
	}

	if latest == nil {
		t.Fatal("LatestAlert returned nil")
	}

	// Verify alert fields
	if latest.ID != alertID {
		t.Errorf("Alert ID mismatch: got %d, expected %d", latest.ID, alertID)
	}
	if latest.PlanID != planID {
		t.Errorf("Alert PlanID mismatch: got %d, expected %d", latest.PlanID, planID)
	}
	if latest.Code != "liquidation_distance_low" {
		t.Errorf("Alert code mismatch: got %s, expected 'liquidation_distance_low'", latest.Code)
	}

	// List alerts
	alerts, err := store.ListAlerts(ctx, planID, 10, 0)
	if err != nil {
		t.Fatalf("ListAlerts failed: %v", err)
	}
	if len(alerts) != 1 {
		t.Errorf("Expected 1 alert, got %d", len(alerts))
	}
}

// TestIntegrationDataUnavailableEscalation tests data-unavailable escalation:
// EvaluationInput with FuturesState.Available=false and
// RiskPolicy.EscalateOnDataFailure=true -> Evaluate returns DataStatus="error",
// ThresholdBreached=true, breach code "data_unavailable";
// persist + read back.
func TestIntegrationDataUnavailableEscalation(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create plan with escalation enabled
	plan := createTestPlan(t, "data-unavail-plan")
	plan.Status = PlanStatusActive
	plan.RiskPolicy = DefaultRiskPolicy()
	plan.RiskPolicy.EscalateOnDataFailure = true

	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Build EvaluationInput with unavailable futures data
	now := time.Now()
	input := EvaluationInput{
		Plan: *plan,
		SpotState: SpotState{
			Available: true,
			Price:     2000.0,
			Quantity:  5.0,
			ValueUSDT: 10000.0,
		},
		FuturesState: FuturesState{
			Available: false, // DATA UNAVAILABLE
		},
		FundingInfo: FundingInfo{
			Available: true,
		},
		Now: now,
	}

	// Evaluate
	evaluation := Evaluate(input)

	// Assertions
	if evaluation.DataStatus != DataStatusError {
		t.Errorf("Expected DataStatus='error', got '%s'", evaluation.DataStatus)
	}
	if !evaluation.ThresholdBreached {
		t.Errorf("Expected ThresholdBreached=true, got false")
	}

	// Check for data_unavailable breach code
	hasDataUnavailableBreach := false
	for _, code := range evaluation.BreachCodes {
		if code == "data_unavailable" {
			hasDataUnavailableBreach = true
			break
		}
	}
	if !hasDataUnavailableBreach {
		t.Errorf("Expected 'data_unavailable' breach code, got %v", evaluation.BreachCodes)
	}

	// Save snapshot
	snapshot := evaluation.Snapshot
	snapshot.PlanID = planID
	snapshot.CreatedAt = now

	snapshotID, err := store.SaveSnapshot(ctx, &snapshot)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Retrieve and verify
	retrieved, err := store.LatestSnapshot(ctx, planID)
	if err != nil {
		t.Fatalf("LatestSnapshot failed: %v", err)
	}

	if retrieved.ID != snapshotID {
		t.Errorf("Snapshot ID mismatch")
	}
	if retrieved.DataStatus != DataStatusError {
		t.Errorf("Expected DataStatus='error' in retrieved snapshot, got '%s'", retrieved.DataStatus)
	}
	if !retrieved.ThresholdBreached {
		t.Errorf("Expected ThresholdBreached=true in retrieved snapshot")
	}
}

// TestIntegrationExecutionSuccessPath tests the complete execution success flow:
// create a plan, save an Execution walking the legal states:
// validating->awaiting_approval->placing_first_leg->first_leg_filled->placing_second_leg->both_legs_filled
// assert each step via CanTransition, save two ExecutionLeg rows (futures + spot, both filled),
// set plan status active; assert ListExecutionLegs returns both and the execution is terminal success.
func TestIntegrationExecutionSuccessPath(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create and save a plan
	plan := createTestPlan(t, "exec-success-plan")
	plan.Status = PlanStatusActive

	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create execution
	now := time.Now()
	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-success",
		State:       string(ExecutionStatePending),
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	// Walk through the state machine
	states := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg,
		ExecutionStateBothLegsFilled,
	}

	for i := 0; i < len(states)-1; i++ {
		from := states[i]
		to := states[i+1]

		// Verify transition is legal
		if !CanTransition(from, to) {
			t.Fatalf("Illegal transition: %s -> %s", from, to)
		}

		// Update execution state
		exec.State = string(to)
		exec.UpdatedAt = now

		err = store.UpdateExecution(ctx, exec)
		if err != nil {
			t.Fatalf("UpdateExecution to %s failed: %v", to, err)
		}
	}

	// Save two execution legs: futures + spot
	futuresLeg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               string(LegTypeFutures),
		Provider:              "bybit",
		Account:               "futures1",
		Symbol:                "ETHUSDT",
		Side:                  "short",
		OrderType:             "limit",
		RequestedAmount:       5.0,
		RequestedNotionalUSDT: 10000.0,
		RequestedPrice:        2000.0,
		OrderID:               "order-futures-001",
		State:                 string(LegStateFilled),
		FilledQuantity:        5.0,
		FilledNotionalUSDT:    10000.0,
		AvgFillPrice:          2000.0,
		FeeUSDT:               20.0,
		ErrorMsg:              "",
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	spotLeg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               string(LegTypeSpot),
		Provider:              "binance",
		Account:               "spot1",
		Symbol:                "ETHUSDT",
		Side:                  "buy",
		OrderType:             "limit",
		RequestedAmount:       5.0,
		RequestedNotionalUSDT: 10000.0,
		RequestedPrice:        2000.0,
		OrderID:               "order-spot-001",
		State:                 string(LegStateFilled),
		FilledQuantity:        5.0,
		FilledNotionalUSDT:    10000.0,
		AvgFillPrice:          2000.0,
		FeeUSDT:               20.0,
		ErrorMsg:              "",
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	_, err = store.SaveExecutionLeg(ctx, futuresLeg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg (futures) failed: %v", err)
	}

	_, err = store.SaveExecutionLeg(ctx, spotLeg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg (spot) failed: %v", err)
	}

	// Update plan status to active
	err = store.UpdatePlanStatus(ctx, planID, PlanStatusActive)
	if err != nil {
		t.Fatalf("UpdatePlanStatus failed: %v", err)
	}

	// Retrieve and verify execution
	retrieved, err := store.GetExecution(ctx, execID)
	if err != nil {
		t.Fatalf("GetExecution failed: %v", err)
	}

	if retrieved.State != string(ExecutionStateBothLegsFilled) {
		t.Errorf("Expected execution state 'both_legs_filled', got '%s'", retrieved.State)
	}

	// Verify terminal success state
	if !IsTerminal(ExecutionState(retrieved.State)) {
		t.Errorf("Expected terminal state, but %s is not terminal", retrieved.State)
	}

	// List and verify execution legs
	legs, err := store.ListExecutionLegs(ctx, execID)
	if err != nil {
		t.Fatalf("ListExecutionLegs failed: %v", err)
	}

	if len(legs) != 2 {
		t.Errorf("Expected 2 legs, got %d", len(legs))
	}

	// Verify both legs are filled
	for _, leg := range legs {
		if leg.State != string(LegStateFilled) {
			t.Errorf("Expected leg state 'filled', got '%s'", leg.State)
		}
		if leg.FilledQuantity != 5.0 {
			t.Errorf("Expected filled quantity 5.0, got %f", leg.FilledQuantity)
		}
	}

	// Verify futures leg
	if legs[0].LegType == string(LegTypeFutures) {
		if legs[0].OrderID != "order-futures-001" {
			t.Errorf("Futures leg order ID mismatch")
		}
	} else if legs[1].LegType == string(LegTypeFutures) {
		if legs[1].OrderID != "order-futures-001" {
			t.Errorf("Futures leg order ID mismatch")
		}
	}
}

// TestIntegrationExecutionOneLeqFailureRecovery tests one-leg failure -> recovery flow:
// walk placing_first_leg->first_leg_filled->placing_second_leg->second_leg_failed->recovery_required
// assert each CanTransition, persist the execution in recovery_required, set plan status recovery_required,
// save filled futures leg + failed spot leg; assert state is recovery_required (NOT terminal)
// and data reflects unhedged exposure. Also assert the ILLEGAL transition
// first_leg_failed->placing_second_leg is rejected by CanTransition.
func TestIntegrationExecutionOneLegFailureRecovery(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()

	// Create and save a plan
	plan := createTestPlan(t, "exec-recovery-plan")
	plan.Status = PlanStatusActive

	planID, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("SavePlan failed: %v", err)
	}

	// Create execution
	now := time.Now()
	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-recovery",
		State:       string(ExecutionStatePending),
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	execID, err := store.SaveExecution(ctx, exec)
	if err != nil {
		t.Fatalf("SaveExecution failed: %v", err)
	}

	// Test ILLEGAL transition: first_leg_failed -> placing_second_leg
	if CanTransition(ExecutionStateFirstLegFailed, ExecutionStatePlacingSecondLeg) {
		t.Errorf("ILLEGAL: first_leg_failed -> placing_second_leg should NOT be allowed")
	}

	// Walk through the recovery path
	states := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg,
		ExecutionStateSecondLegFailed,
		ExecutionStateRecoveryRequired,
	}

	for i := 0; i < len(states)-1; i++ {
		from := states[i]
		to := states[i+1]

		// Verify transition is legal
		if !CanTransition(from, to) {
			t.Fatalf("Illegal transition: %s -> %s", from, to)
		}

		// Update execution state
		exec.State = string(to)
		exec.UpdatedAt = now

		err = store.UpdateExecution(ctx, exec)
		if err != nil {
			t.Fatalf("UpdateExecution to %s failed: %v", to, err)
		}
	}

	// Verify recovery_required is NOT terminal
	if IsTerminal(ExecutionStateRecoveryRequired) {
		t.Errorf("recovery_required should NOT be a terminal state")
	}

	// Save filled futures leg
	futuresLeg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               string(LegTypeFutures),
		Provider:              "bybit",
		Account:               "futures1",
		Symbol:                "ETHUSDT",
		Side:                  "short",
		OrderType:             "limit",
		RequestedAmount:       5.0,
		RequestedNotionalUSDT: 10000.0,
		RequestedPrice:        2000.0,
		OrderID:               "order-futures-001",
		State:                 string(LegStateFilled),
		FilledQuantity:        5.0,
		FilledNotionalUSDT:    10000.0,
		AvgFillPrice:          2000.0,
		FeeUSDT:               20.0,
		ErrorMsg:              "",
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	// Save failed spot leg (not filled)
	spotLeg := &ExecutionLeg{
		ExecutionID:           execID,
		LegType:               string(LegTypeSpot),
		Provider:              "binance",
		Account:               "spot1",
		Symbol:                "ETHUSDT",
		Side:                  "buy",
		OrderType:             "limit",
		RequestedAmount:       5.0,
		RequestedNotionalUSDT: 10000.0,
		RequestedPrice:        2000.0,
		OrderID:               "order-spot-001-failed",
		State:                 string(LegStateFailed),
		FilledQuantity:        0.0, // NOT filled
		FilledNotionalUSDT:    0.0,
		AvgFillPrice:          0.0,
		FeeUSDT:               0.0,
		ErrorMsg:              "Market order rejected due to insufficient liquidity",
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	_, err = store.SaveExecutionLeg(ctx, futuresLeg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg (futures) failed: %v", err)
	}

	_, err = store.SaveExecutionLeg(ctx, spotLeg)
	if err != nil {
		t.Fatalf("SaveExecutionLeg (spot) failed: %v", err)
	}

	// Update plan status to recovery_required
	err = store.UpdatePlanStatus(ctx, planID, PlanStatusRecoveryRequired)
	if err != nil {
		t.Fatalf("UpdatePlanStatus failed: %v", err)
	}

	// Retrieve and verify execution
	retrieved, err := store.GetExecution(ctx, execID)
	if err != nil {
		t.Fatalf("GetExecution failed: %v", err)
	}

	if retrieved.State != string(ExecutionStateRecoveryRequired) {
		t.Errorf("Expected execution state 'recovery_required', got '%s'", retrieved.State)
	}

	// Verify NOT terminal
	if IsTerminal(ExecutionState(retrieved.State)) {
		t.Errorf("recovery_required should NOT be terminal, but IsTerminal returned true")
	}

	// List and verify execution legs
	legs, err := store.ListExecutionLegs(ctx, execID)
	if err != nil {
		t.Fatalf("ListExecutionLegs failed: %v", err)
	}

	if len(legs) != 2 {
		t.Errorf("Expected 2 legs, got %d", len(legs))
	}

	// Find filled and failed legs
	var filledLeg, failedLeg *ExecutionLeg
	for i := range legs {
		if legs[i].State == string(LegStateFilled) {
			filledLeg = &legs[i]
		} else if legs[i].State == string(LegStateFailed) {
			failedLeg = &legs[i]
		}
	}

	if filledLeg == nil {
		t.Errorf("Expected a filled leg, got none")
	} else {
		if filledLeg.FilledQuantity != 5.0 {
			t.Errorf("Expected filled leg quantity 5.0, got %f", filledLeg.FilledQuantity)
		}
	}

	if failedLeg == nil {
		t.Errorf("Expected a failed leg, got none")
	} else {
		if failedLeg.State != string(LegStateFailed) {
			t.Errorf("Expected failed leg state, got %s", failedLeg.State)
		}
		if failedLeg.FilledQuantity != 0.0 {
			t.Errorf("Expected failed leg quantity 0.0, got %f", failedLeg.FilledQuantity)
		}
		if failedLeg.ErrorMsg == "" {
			t.Errorf("Expected error message on failed leg")
		}
	}

	// Verify plan status is recovery_required
	retrievedPlan, err := store.GetPlan(ctx, planID)
	if err != nil {
		t.Fatalf("GetPlan failed: %v", err)
	}

	if retrievedPlan.Status != PlanStatusRecoveryRequired {
		t.Errorf("Expected plan status 'recovery_required', got '%s'", retrievedPlan.Status)
	}
}
