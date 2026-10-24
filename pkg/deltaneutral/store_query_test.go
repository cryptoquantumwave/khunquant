package deltaneutral

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// planCounter gives each seeded plan a unique name across the test binary.
var planCounter int64

// ── helpers ──────────────────────────────────────────────────────────────────

func seedMinimalPlan(t *testing.T, store *Store, provider, symbol, status string) int64 {
	t.Helper()
	n := atomic.AddInt64(&planCounter, 1)
	now := time.Now().UTC()
	plan := &Plan{
		Name:            fmt.Sprintf("test-plan-%d-%s", n, status),
		Asset:           "BTC",
		Status:          status,
		Mode:            ExecutionModeApproval,
		SpotProvider:    provider,
		SpotSymbol:      symbol[:len(symbol)-5], // strip :USDT for spot
		SpotSide:        "buy",
		FuturesProvider: provider,
		FuturesSymbol:   symbol,
		FuturesSide:     "short",
		FuturesLeverage: 1,
		CapitalUSDT:     1000,
		RiskPolicy:      DefaultRiskPolicy(),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if len(symbol) < 6 {
		plan.SpotSymbol = "BTC/USDT"
	}
	id, err := store.SavePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("seedMinimalPlan: %v", err)
	}
	return id
}

func seedExecution(t *testing.T, store *Store, planID int64, state string) int64 {
	t.Helper()
	now := time.Now().UTC()
	exec := &Execution{
		PlanID:      planID,
		AttemptID:   "attempt-" + state,
		State:       state,
		RequestedAt: now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	id, err := store.SaveExecution(context.Background(), exec)
	if err != nil {
		t.Fatalf("seedExecution: %v", err)
	}
	return id
}

func seedLeg(t *testing.T, store *Store, execID int64, legType, side, legState string, qty, fee float64) {
	t.Helper()
	now := time.Now().UTC()
	leg := &ExecutionLeg{
		ExecutionID:    execID,
		LegType:        legType,
		Provider:       "binance",
		Symbol:         "BTC/USDT",
		Side:           side,
		OrderType:      "market",
		State:          legState,
		FilledQuantity: qty,
		FeeUSDT:        fee,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := store.SaveExecutionLeg(context.Background(), leg); err != nil {
		t.Fatalf("seedLeg: %v", err)
	}
}

// ── PlanOpenQuantities ────────────────────────────────────────────────────────

// TestPlanOpenQuantities_NoPlan: plan with no executions returns (0, 0, nil).
func TestPlanOpenQuantities_NoExecutions(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if futBase != 0 || spotQty != 0 {
		t.Errorf("empty plan: got (%.4f, %.4f), want (0, 0)", futBase, spotQty)
	}
}

// TestPlanOpenQuantities_OpenOnly: a single open execution records futures sell
// + spot buy. Net must equal those quantities.
func TestPlanOpenQuantities_OpenOnly(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	execID := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, execID, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, -0.0148)
	seedLeg(t, store, execID, string(LegTypeSpot), "buy", string(LegStateFilled), 253.5, -0.0801)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if futBase != 250.0 {
		t.Errorf("futuresBase: got %.4f, want 250.0", futBase)
	}
	if spotQty != 253.5 {
		t.Errorf("spotQty: got %.4f, want 253.5", spotQty)
	}
}

// TestPlanOpenQuantities_WithResizeDecrease: an open execution + a resize
// decrease execution. Net = open − decrease.
func TestPlanOpenQuantities_WithResizeDecrease(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	// Open
	openExec := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, openExec, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, -0.0148)
	seedLeg(t, store, openExec, string(LegTypeSpot), "buy", string(LegStateFilled), 253.5, -0.0801)

	// Resize decrease (close half)
	resizeExec := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, resizeExec, string(LegTypeFutures), "buy", string(LegStateFilled), 125.0, -0.0074)
	seedLeg(t, store, resizeExec, string(LegTypeSpot), "sell", string(LegStateFilled), 126.75, -0.0401)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wantFut := 250.0 - 125.0
	wantSpot := 253.5 - 126.75
	if math.Abs(futBase-wantFut) > 1e-9 {
		t.Errorf("futuresBase: got %.6f, want %.6f", futBase, wantFut)
	}
	if math.Abs(spotQty-wantSpot) > 1e-9 {
		t.Errorf("spotQty: got %.6f, want %.6f", spotQty, wantSpot)
	}
}

// TestPlanOpenQuantities_CountsLegsOfAnyExecutionState: filled legs count whatever the
// state of their execution (a recovery_required open still holds a real position).
// This replaces the old both_legs_filled-only semantics, which made a recovery_required
// plan look flat and let unwind fall back to whole-account balances.
func TestPlanOpenQuantities_CountsLegsOfAnyExecutionState(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusRecoveryRequired)

	recExec := seedExecution(t, store, planID, string(ExecutionStateRecoveryRequired))
	seedLeg(t, store, recExec, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, recExec, string(LegTypeSpot), "buy", string(LegStateFilled), 253.5, 0)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if futBase != 250.0 || spotQty != 253.5 {
		t.Errorf("recovery_required legs must count: got (%.4f, %.4f), want (250, 253.5)", futBase, spotQty)
	}
}

// TestOpenLegQuantities_NetsAcrossExecutionStates: open (both_legs_filled), a
// recovery_required half-open, a resize-down (both_legs_filled, opposite side) and an
// unwind (unwound, opposite side) all net together; failed legs never count.
func TestOpenLegQuantities_NetsAcrossExecutionStates(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	other := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	open := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, open, string(LegTypeFutures), "sell", string(LegStateFilled), 100.0, 0)
	seedLeg(t, store, open, string(LegTypeSpot), "buy", string(LegStateFilled), 100.0, 0)

	// A later, half-completed increase: futures filled, spot failed.
	half := seedExecution(t, store, planID, string(ExecutionStateRecoveryRequired))
	seedLeg(t, store, half, string(LegTypeFutures), "sell", string(LegStateFilled), 40.0, 0)
	seedLeg(t, store, half, string(LegTypeSpot), "buy", string(LegStateFailed), 40.0, 0)

	// Resize down.
	down := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, down, string(LegTypeFutures), "buy", string(LegStateFilled), 30.0, 0)
	seedLeg(t, store, down, string(LegTypeSpot), "sell", string(LegStateFilled), 30.0, 0)

	// An unwinding row that only closed the futures leg so far.
	unw := seedExecution(t, store, planID, string(ExecutionStateUnwinding))
	seedLeg(t, store, unw, string(LegTypeFutures), "buy", string(LegStateFilled), 10.0, 0)

	// Another plan must not leak in.
	oe := seedExecution(t, store, other, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, oe, string(LegTypeFutures), "sell", string(LegStateFilled), 999.0, 0)

	fut, spot, err := store.OpenLegQuantities(ctx, planID)
	if err != nil {
		t.Fatalf("OpenLegQuantities: %v", err)
	}
	if math.Abs(fut-(100+40-30-10)) > 1e-9 {
		t.Errorf("futures net: got %.6f, want 100", fut)
	}
	if math.Abs(spot-(100-30)) > 1e-9 {
		t.Errorf("spot net: got %.6f, want 70", spot)
	}
}

// TestHasOpenExposure covers flat, open, offset-by-unwind, fee dust and unrecorded fills.
func TestHasOpenExposure(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	check := func(name string, planID int64, want bool) {
		t.Helper()
		got, err := store.HasOpenExposure(ctx, planID)
		if err != nil {
			t.Fatalf("%s: HasOpenExposure: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: HasOpenExposure = %v, want %v", name, got, want)
		}
	}

	// No executions at all.
	empty := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusFailed)
	check("no legs", empty, false)

	// Opened, never unwound: a failed plan still holding a position.
	openPlan := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusFailed)
	oe := seedExecution(t, store, openPlan, string(ExecutionStateRecoveryRequired))
	seedLeg(t, store, oe, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, 0)
	check("open futures only", openPlan, true)

	// Fully unwound (opposite-side legs of the same size).
	flat := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusClosed)
	fe := seedExecution(t, store, flat, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, fe, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, fe, string(LegTypeSpot), "buy", string(LegStateFilled), 253.5, 0)
	ue := seedExecution(t, store, flat, string(ExecutionStateUnwound))
	seedLeg(t, store, ue, string(LegTypeFutures), "buy", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, ue, string(LegTypeSpot), "sell", string(LegStateFilled), 253.5, 0)
	check("fully unwound", flat, false)

	// Spot fee dust: buy leg records gross 253.5, the unwind could only sell the
	// fee-reduced free balance (0.1% less). Must NOT count as exposure.
	dust := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusClosed)
	de := seedExecution(t, store, dust, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, de, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, de, string(LegTypeSpot), "buy", string(LegStateFilled), 253.5, 0)
	due := seedExecution(t, store, dust, string(ExecutionStateUnwound))
	seedLeg(t, store, due, string(LegTypeFutures), "buy", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, due, string(LegTypeSpot), "sell", string(LegStateFilled), 253.5*0.999, 0)
	check("unwound with spot fee dust", dust, false)

	// A real residual (spot half sold) is exposure.
	half := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusRecoveryRequired)
	he := seedExecution(t, store, half, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, he, string(LegTypeFutures), "sell", string(LegStateFilled), 250.0, 0)
	seedLeg(t, store, he, string(LegTypeSpot), "buy", string(LegStateFilled), 250.0, 0)
	hue := seedExecution(t, store, half, string(ExecutionStateRecoveryRequired))
	seedLeg(t, store, hue, string(LegTypeFutures), "buy", string(LegStateFilled), 250.0, 0)
	check("futures closed, spot still open", half, true)

	// Failed legs never count.
	failedLeg := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusFailed)
	fle := seedExecution(t, store, failedLeg, string(ExecutionStateFailed))
	seedLeg(t, store, fle, string(LegTypeFutures), "sell", string(LegStateFailed), 250.0, 0)
	check("only failed legs", failedLeg, false)

	// An order that went through but whose leg row was never saved.
	unrec := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusRecoveryRequired)
	ure := seedExecution(t, store, unrec, string(ExecutionStatePlacingFirstLeg))
	exec, err := store.GetExecution(ctx, ure)
	if err != nil {
		t.Fatalf("GetExecution: %v", err)
	}
	exec.ErrorMsg = UnrecordedFillMarker + " order_id=abc symbol=BTC/USDT:USDT side=sell filled_qty=250"
	if err := store.UpdateExecution(ctx, exec); err != nil {
		t.Fatalf("UpdateExecution: %v", err)
	}
	check("unrecorded fill marker", unrec, true)
}

// TestPlanOpenQuantities_FailedLegStateExcluded: a filled execution whose legs
// are in 'failed' state (partial fill scenario) must not count.
func TestPlanOpenQuantities_FailedLegStateExcluded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	execID := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))
	// One leg filled, one failed
	seedLeg(t, store, execID, string(LegTypeFutures), "sell", string(LegStateFilled), 100.0, 0)
	seedLeg(t, store, execID, string(LegTypeSpot), "buy", string(LegStateFailed), 100.0, 0)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the filled futures leg counts
	if futBase != 100.0 {
		t.Errorf("futuresBase: got %.4f, want 100.0", futBase)
	}
	if spotQty != 0 {
		t.Errorf("spotQty: got %.4f, want 0 (failed leg excluded)", spotQty)
	}
}

// TestPlanOpenQuantities_OtherPlanNotLeaked: legs from a different plan must
// not appear in the query for this plan.
func TestPlanOpenQuantities_OtherPlanNotLeaked(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planA := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	planB := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	execB := seedExecution(t, store, planB, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, execB, string(LegTypeFutures), "sell", string(LegStateFilled), 500.0, 0)
	seedLeg(t, store, execB, string(LegTypeSpot), "buy", string(LegStateFilled), 505.0, 0)

	futBase, spotQty, err := store.PlanOpenQuantities(context.Background(), planA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if futBase != 0 || spotQty != 0 {
		t.Errorf("plan A must not see plan B's legs: got (%.4f, %.4f)", futBase, spotQty)
	}
}

// ── SumPlanExecutionFees ──────────────────────────────────────────────────────

// TestSumPlanExecutionFees_Empty: plan with no legs → 0.
func TestSumPlanExecutionFees_Empty(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	fee, err := store.SumPlanExecutionFees(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee != 0 {
		t.Errorf("empty plan: got %v, want 0", fee)
	}
}

// TestSumPlanExecutionFees_NegativeSum: multiple filled legs with negative fee
// values must sum to a negative total (fees are costs).
func TestSumPlanExecutionFees_NegativeSum(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	execID := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))

	// Open: two negative legs
	seedLeg(t, store, execID, string(LegTypeFutures), "sell", string(LegStateFilled), 250, -0.0148)
	seedLeg(t, store, execID, string(LegTypeSpot), "buy", string(LegStateFilled), 253, -0.0801)

	// Unwind: two more negative legs (separate execution)
	closeExec := seedExecution(t, store, planID, string(ExecutionStateUnwound))
	seedLeg(t, store, closeExec, string(LegTypeFutures), "buy", string(LegStateFilled), 250, -0.0148)
	seedLeg(t, store, closeExec, string(LegTypeSpot), "sell", string(LegStateFilled), 253, -0.0080)

	wantFee := -0.0148 + -0.0801 + -0.0148 + -0.0080 // -0.1177

	fee, err := store.SumPlanExecutionFees(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee >= 0 {
		t.Errorf("fee should be negative (cost), got %v", fee)
	}
	if math.Abs(fee-wantFee) > 1e-9 {
		t.Errorf("fee sum: got %.8f, want %.8f", fee, wantFee)
	}
}

// TestSumPlanExecutionFees_FailedLegExcluded: a leg in 'failed' state must not
// contribute to the fee sum even if FeeUSDT is set.
func TestSumPlanExecutionFees_FailedLegExcluded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	execID := seedExecution(t, store, planID, string(ExecutionStateBothLegsFilled))

	seedLeg(t, store, execID, string(LegTypeFutures), "sell", string(LegStateFilled), 250, -0.0148)
	// This failed leg has a fee set — must be ignored
	seedLeg(t, store, execID, string(LegTypeSpot), "buy", string(LegStateFailed), 250, -9999.0)

	fee, err := store.SumPlanExecutionFees(context.Background(), planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(fee-(-0.0148)) > 1e-9 {
		t.Errorf("failed leg fee must be excluded: got %.6f, want %.6f", fee, -0.0148)
	}
}

// TestSumPlanExecutionFees_OtherPlanNotLeaked: fees from another plan must not
// be included.
func TestSumPlanExecutionFees_OtherPlanNotLeaked(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planA := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	planB := seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)

	execB := seedExecution(t, store, planB, string(ExecutionStateBothLegsFilled))
	seedLeg(t, store, execB, string(LegTypeFutures), "sell", string(LegStateFilled), 250, -99.99)

	fee, err := store.SumPlanExecutionFees(context.Background(), planA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fee != 0 {
		t.Errorf("plan A must not see plan B fees: got %v", fee)
	}
}

// ── ActivePlansForFuturesSymbol ───────────────────────────────────────────────

// TestActivePlansForFuturesSymbol_ReturnsActiveAndRecovery: only active and
// recovery_required plans on the same provider+symbol should be returned.
func TestActivePlansForFuturesSymbol_ReturnsActiveAndRecovery(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	provider := "okx"
	symbol := "BTC/USDT:USDT"

	activeID := seedMinimalPlan(t, store, provider, symbol, PlanStatusActive)
	recovID := seedMinimalPlan(t, store, provider, symbol, PlanStatusRecoveryRequired)
	closedID := seedMinimalPlan(t, store, provider, symbol, PlanStatusClosed)
	draftID := seedMinimalPlan(t, store, provider, symbol, PlanStatusDraft)
	_ = closedID
	_ = draftID

	// Exclude one active plan (simulates self-exclusion during unwind)
	ids, err := store.ActivePlansForFuturesSymbol(context.Background(), provider, symbol, activeID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ids) != 1 {
		t.Fatalf("expected 1 result (only recovery_required), got %d: %v", len(ids), ids)
	}
	if ids[0] != recovID {
		t.Errorf("expected recovID=%d, got %d", recovID, ids[0])
	}
}

// TestActivePlansForFuturesSymbol_DifferentProviderExcluded: plans on a different
// provider do not conflict even if the symbol is the same.
func TestActivePlansForFuturesSymbol_DifferentProviderExcluded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	seedMinimalPlan(t, store, "binance", "BTC/USDT:USDT", PlanStatusActive)
	refID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)

	ids, err := store.ActivePlansForFuturesSymbol(context.Background(), "okx", "BTC/USDT:USDT", refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The binance plan should NOT appear in okx results
	if len(ids) != 0 {
		t.Errorf("different-provider plan must not conflict: got ids %v", ids)
	}
}

// TestActivePlansForFuturesSymbol_DifferentSymbolExcluded: different symbol on
// same provider should not match.
func TestActivePlansForFuturesSymbol_DifferentSymbolExcluded(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	seedMinimalPlan(t, store, "okx", "ETH/USDT:USDT", PlanStatusActive)
	refID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)

	ids, err := store.ActivePlansForFuturesSymbol(context.Background(), "okx", "BTC/USDT:USDT", refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("different-symbol plan must not conflict: got %v", ids)
	}
}

// TestActivePlansForFuturesSymbol_NoConflict: when no other active plans exist
// the function returns an empty slice without error.
func TestActivePlansForFuturesSymbol_NoConflict(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	id := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)

	ids, err := store.ActivePlansForFuturesSymbol(context.Background(), "okx", "BTC/USDT:USDT", id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("no conflict expected, got ids %v", ids)
	}
}

// ── ListSnapshotSeries + downsampleSeries ─────────────────────────────────────

func seedSnapshot(t *testing.T, store *Store, planID int64, checkedAt time.Time, fundingAPY, earnAPY, combinedAPY, fundingRate float64) {
	t.Helper()
	snap := &MonitorSnapshot{
		PlanID:             planID,
		CheckedAt:          checkedAt,
		CurrentFundingRate: fundingRate,
		FundingAPYPct:      fundingAPY,
		EarnAPYPct:         earnAPY,
		CombinedAPYPct:     combinedAPY,
		DataStatus:         "ok",
		CreatedAt:          time.Now().UTC(),
	}
	if _, err := store.SaveSnapshot(context.Background(), snap); err != nil {
		t.Fatalf("seedSnapshot: %v", err)
	}
}

// TestListSnapshotSeries_ReturnAllRaw: maxPoints<=0 returns all rows in
// ascending time order without downsampling.
func TestListSnapshotSeries_ReturnAllRaw(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedSnapshot(t, store, planID, base.Add(time.Duration(i)*time.Hour),
			float64(i)*10, 13.0, float64(i)*10+13.0, float64(i)*0.0001)
	}

	pts, err := store.ListSnapshotSeries(context.Background(), planID, time.Time{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) != 5 {
		t.Fatalf("expected 5 raw points, got %d", len(pts))
	}
	// Ascending time order
	for i := 1; i < len(pts); i++ {
		if pts[i].CheckedAt.Before(pts[i-1].CheckedAt) {
			t.Errorf("points not in ascending order at index %d", i)
		}
	}
}

// TestListSnapshotSeries_SinceFilter: rows before `since` must be excluded.
func TestListSnapshotSeries_SinceFilter(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		seedSnapshot(t, store, planID, base.Add(time.Duration(i)*time.Hour), float64(i)*10, 13, float64(i)*10+13, 0.0001)
	}

	// Only rows from hour 3 onwards (hours 3, 4, 5 → 3 rows)
	since := base.Add(3 * time.Hour)
	pts, err := store.ListSnapshotSeries(context.Background(), planID, since, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) != 3 {
		t.Fatalf("since filter: expected 3 rows, got %d", len(pts))
	}
	if pts[0].CheckedAt.Before(since) {
		t.Errorf("first row (%v) is before since (%v)", pts[0].CheckedAt, since)
	}
}

// TestListSnapshotSeries_Downsampled: when len(raw) > maxPoints the series is
// bucketed to at most maxPoints entries.
func TestListSnapshotSeries_Downsampled(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		seedSnapshot(t, store, planID, base.Add(time.Duration(i)*time.Hour), float64(i)*5, 13, float64(i)*5+13, 0.0001)
	}

	maxPoints := 5
	pts, err := store.ListSnapshotSeries(context.Background(), planID, time.Time{}, maxPoints)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) > maxPoints {
		t.Errorf("expected ≤ %d downsampled points, got %d", maxPoints, len(pts))
	}
	if len(pts) == 0 {
		t.Error("expected at least one bucket")
	}
}

// TestListSnapshotSeries_MetricsAveragedInBucket: within each bucket the
// CombinedAPYPct should be the average of the raw rows in that bucket.
func TestListSnapshotSeries_MetricsAveragedInBucket(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	planID := seedMinimalPlan(t, store, "okx", "BTC/USDT:USDT", PlanStatusActive)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 4 snapshots with known CombinedAPYPct values; downsample to 2 buckets.
	// Bucket 0 → rows 0,1 → avg(10, 20) = 15
	// Bucket 1 → rows 2,3 → avg(30, 40) = 35
	vals := []float64{10, 20, 30, 40}
	for i, v := range vals {
		seedSnapshot(t, store, planID, base.Add(time.Duration(i)*time.Hour), 0, 0, v, 0)
	}

	pts, err := store.ListSnapshotSeries(context.Background(), planID, time.Time{}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pts) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(pts))
	}
	if math.Abs(pts[0].CombinedAPYPct-15.0) > 0.01 {
		t.Errorf("bucket 0 avg: got %.4f, want 15.0", pts[0].CombinedAPYPct)
	}
	if math.Abs(pts[1].CombinedAPYPct-35.0) > 0.01 {
		t.Errorf("bucket 1 avg: got %.4f, want 35.0", pts[1].CombinedAPYPct)
	}
}

// TestDownsampleSeries_EmptyInput: empty slice returns empty.
func TestDownsampleSeries_EmptyInput(t *testing.T) {
	result := downsampleSeries(nil, 5)
	if len(result) != 0 {
		t.Errorf("expected empty, got %d", len(result))
	}
}

// TestDownsampleSeries_SinglePoint: one point with any n returns 1 point.
func TestDownsampleSeries_SinglePoint(t *testing.T) {
	pts := []SnapshotSeriesPoint{{CheckedAt: time.Now(), CombinedAPYPct: 7.5}}
	result := downsampleSeries(pts, 10)
	if len(result) != 1 {
		t.Fatalf("expected 1, got %d", len(result))
	}
	if result[0].CombinedAPYPct != 7.5 {
		t.Errorf("value should be preserved: got %v", result[0].CombinedAPYPct)
	}
}

// TestDownsampleSeries_AllSameTimestamp: when all points share the same time
// (span=0) returns 1 point (no divide-by-zero panic).
func TestDownsampleSeries_AllSameTimestamp(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := []SnapshotSeriesPoint{
		{CheckedAt: ts, CombinedAPYPct: 5.0},
		{CheckedAt: ts, CombinedAPYPct: 10.0},
		{CheckedAt: ts, CombinedAPYPct: 15.0},
	}
	// Should not panic; returns first point
	result := downsampleSeries(pts, 3)
	if len(result) != 1 {
		t.Fatalf("span=0: expected 1 (first point returned), got %d", len(result))
	}
}

// TestDownsampleSeries_RequestedMoreThanAvailable: asking for more buckets than
// raw points returns raw unchanged.
func TestDownsampleSeries_RequestedMoreThanAvailable(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pts := []SnapshotSeriesPoint{
		{CheckedAt: base, CombinedAPYPct: 1},
		{CheckedAt: base.Add(time.Hour), CombinedAPYPct: 2},
	}
	result := downsampleSeries(pts, 10)
	// len(pts) <= 10 so raw is returned as-is
	if len(result) != 2 {
		t.Errorf("expected 2 (raw passthrough), got %d", len(result))
	}
}
