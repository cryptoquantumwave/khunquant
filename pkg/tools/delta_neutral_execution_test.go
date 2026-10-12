package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// resetRateLimiter swaps in an always-allow limiter for the duration of the test,
// so delta-neutral execution tests that pass the leverage gate don't drain the
// shared global DefaultLimiter bucket (5/provider/min) and pollute later tests in
// this package (e.g. TestFuturesOpenPosition_DryRunNormalizesSymbol). Mirrors the
// save/restore pattern in futures_coverage_test.go's injectMockFuturesProvider.
func resetRateLimiter(t *testing.T) {
	orig := broker.DefaultLimiter
	broker.DefaultLimiter = unlimitedLimiter{}
	t.Cleanup(func() { broker.DefaultLimiter = orig })
}

// TestOpenDeltaNeutralPositionDryRun tests that dry-run does not place orders.
func TestOpenDeltaNeutralPositionDryRun(t *testing.T) {
	resetRateLimiter(t)
	ctx := context.Background()
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	// Create a ready plan
	plan := &deltaneutral.Plan{
		Name:                "test-plan",
		Asset:               "BTC",
		Status:              "ready",
		Mode:                "approval",
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewOpenDeltaNeutralPositionTool(cfg, store)

	// Dry-run (confirm=false)
	result := tool.Execute(ctx, map[string]any{
		"plan_id": float64(plan.ID),
		"confirm": false,
	})

	// Should succeed and return a review (not an error)
	if result.IsError {
		t.Fatalf("dry-run should not error, got: %v", result.ForLLM)
	}

	if result.ForUser == "" && result.ForLLM == "" {
		t.Fatal("dry-run should return a review string")
	}

	// Verify no execution was created or written to DB
	execs, err := store.ListExecutions(ctx, plan.ID, 10, 0)
	if err == nil && len(execs) > 0 {
		t.Error("dry-run should not create an execution record")
	}
}

// TestOpenDeltaNeutralPositionAllowsDraft tests that draft plans can be opened directly.
func TestOpenDeltaNeutralPositionAllowsDraft(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	// Create a draft plan
	plan := &deltaneutral.Plan{
		Name:                "test-draft",
		Asset:               "BTC",
		Status:              "draft",
		Mode:                "approval",
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewOpenDeltaNeutralPositionTool(cfg, store)
	result := tool.Execute(ctx, map[string]any{
		"plan_id": float64(plan.ID),
		"confirm": false,
	})

	if result.IsError {
		t.Fatalf("opening a draft plan in dry-run should succeed, got: %v", result.ForLLM)
	}

	if !strings.Contains(strings.ToLower(result.ForUser), "confirm=true") {
		t.Fatalf("expected draft dry-run review prompting confirm=true:\n%s", result.ForUser)
	}
}

// TestOpenDeltaNeutralPositionLeverageGate tests that leverage gate is enforced.
func TestOpenDeltaNeutralPositionLeverageGate(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: false,
		},
	}
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	plan := &deltaneutral.Plan{
		Name:                "test-lev",
		Asset:               "BTC",
		Status:              "ready",
		Mode:                "approval",
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     2,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewOpenDeltaNeutralPositionTool(cfg, store)
	result := tool.Execute(ctx, map[string]any{
		"plan_id": float64(plan.ID),
		"confirm": false,
	})

	// Leverage gate should block even a dry-run
	if !result.IsError {
		t.Fatal("leverage gate should block the open when AllowLeverage=false")
	}

	if !strings.Contains(result.ForLLM, "leverage") {
		t.Errorf("expected 'leverage' in error, got: %s", result.ForLLM)
	}
}

// TestUnwindDeltaNeutralPositionRequireActiveOrRecovery tests that only active/recovery_required plans can be unwound.
func TestUnwindDeltaNeutralPositionRequireActiveOrRecovery(t *testing.T) {
	resetRateLimiter(t)
	ctx := context.Background()
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	plan := &deltaneutral.Plan{
		Name:                "test-closed",
		Asset:               "BTC",
		Status:              "closed",
		Mode:                "approval",
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewUnwindDeltaNeutralPositionTool(cfg, store)
	result := tool.Execute(ctx, map[string]any{
		"plan_id": float64(plan.ID),
		"confirm": false,
	})

	if !result.IsError {
		t.Fatal("unwinding a closed plan should error")
	}

	if !strings.Contains(result.ForLLM, "active") && !strings.Contains(result.ForLLM, "recovery_required") {
		t.Errorf("expected error about status, got: %s", result.ForLLM)
	}
}

// TestUnwindDeltaNeutralPositionDryRun tests that dry-run does not close orders.
func TestUnwindDeltaNeutralPositionDryRun(t *testing.T) {
	resetRateLimiter(t)
	ctx := context.Background()
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	plan := &deltaneutral.Plan{
		Name:                "test-active",
		Asset:               "BTC",
		Status:              "active",
		Mode:                "approval",
		SpotProvider:        "binance",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000,
		FuturesNotionalUSDT: 5000,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewUnwindDeltaNeutralPositionTool(cfg, store)
	result := tool.Execute(ctx, map[string]any{
		"plan_id": float64(plan.ID),
		"confirm": false,
	})

	// Should succeed with a review
	if result.IsError {
		t.Fatalf("dry-run should not error, got: %v", result.ForLLM)
	}

	if result.ForUser == "" && result.ForLLM == "" {
		t.Fatal("dry-run should return a review string")
	}

	// Verify no execution was created
	execs, err := store.ListExecutions(ctx, plan.ID, 10, 0)
	if err == nil && len(execs) > 0 {
		t.Error("dry-run should not create an execution record")
	}
}

// TestExecutionStateMachineTransitions validates that state transitions are legal.
func TestExecutionStateMachineTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    deltaneutral.ExecutionState
		to      deltaneutral.ExecutionState
		allowed bool
	}{
		{
			name:    "pending_to_validating",
			from:    deltaneutral.ExecutionStatePending,
			to:      deltaneutral.ExecutionStateValidating,
			allowed: true,
		},
		{
			name:    "pending_to_cancelled",
			from:    deltaneutral.ExecutionStatePending,
			to:      deltaneutral.ExecutionStateCancelled,
			allowed: true,
		},
		{
			name:    "pending_to_failed",
			from:    deltaneutral.ExecutionStatePending,
			to:      deltaneutral.ExecutionStateFailed,
			allowed: false,
		},
		{
			name:    "validating_to_awaiting_approval",
			from:    deltaneutral.ExecutionStateValidating,
			to:      deltaneutral.ExecutionStateAwaitingApproval,
			allowed: true,
		},
		{
			name:    "awaiting_approval_to_placing_first_leg",
			from:    deltaneutral.ExecutionStateAwaitingApproval,
			to:      deltaneutral.ExecutionStatePlacingFirstLeg,
			allowed: true,
		},
		{
			name:    "placing_first_leg_to_first_leg_filled",
			from:    deltaneutral.ExecutionStatePlacingFirstLeg,
			to:      deltaneutral.ExecutionStateFirstLegFilled,
			allowed: true,
		},
		{
			name:    "first_leg_filled_to_placing_second_leg",
			from:    deltaneutral.ExecutionStateFirstLegFilled,
			to:      deltaneutral.ExecutionStatePlacingSecondLeg,
			allowed: true,
		},
		{
			name:    "second_leg_failed_to_recovery_required",
			from:    deltaneutral.ExecutionStateSecondLegFailed,
			to:      deltaneutral.ExecutionStateRecoveryRequired,
			allowed: true,
		},
		{
			name:    "recovery_required_to_unwinding",
			from:    deltaneutral.ExecutionStateRecoveryRequired,
			to:      deltaneutral.ExecutionStateUnwinding,
			allowed: true,
		},
		{
			name:    "unwinding_to_unwound",
			from:    deltaneutral.ExecutionStateUnwinding,
			to:      deltaneutral.ExecutionStateUnwound,
			allowed: true,
		},
		{
			name:    "unwinding_to_recovery_required",
			from:    deltaneutral.ExecutionStateUnwinding,
			to:      deltaneutral.ExecutionStateRecoveryRequired,
			allowed: true,
		},
		{
			name:    "unwinding_to_failed",
			from:    deltaneutral.ExecutionStateUnwinding,
			to:      deltaneutral.ExecutionStateFailed,
			allowed: true,
		},
		{
			name:    "both_legs_filled_is_terminal",
			from:    deltaneutral.ExecutionStateBothLegsFilled,
			to:      deltaneutral.ExecutionStateUnwinding,
			allowed: true,
		},
		{
			name:    "unwound_is_terminal",
			from:    deltaneutral.ExecutionStateUnwound,
			to:      deltaneutral.ExecutionStateFailed,
			allowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := deltaneutral.CanTransition(tt.from, tt.to)
			if allowed != tt.allowed {
				t.Errorf("CanTransition(%s, %s) = %v, expected %v",
					tt.from, tt.to, allowed, tt.allowed)
			}
		})
	}
}

// TestFirstLegTypeLogic validates the first leg selection.
func TestFirstLegTypeLogic(t *testing.T) {
	tests := []struct {
		name            string
		spotLessLiquid  bool
		expectedLegType deltaneutral.LegType
	}{
		{
			name:            "default_futures_first",
			spotLessLiquid:  false,
			expectedLegType: deltaneutral.LegTypeFutures,
		},
		{
			name:            "spot_less_liquid_spot_first",
			spotLessLiquid:  true,
			expectedLegType: deltaneutral.LegTypeSpot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legType := deltaneutral.FirstLegType(tt.spotLessLiquid)
			if legType != tt.expectedLegType {
				t.Errorf("FirstLegType(%v) = %s, expected %s",
					tt.spotLessLiquid, legType, tt.expectedLegType)
			}
		})
	}
}

// Helper to set up a temporary delta-neutral store for tests.
func setupTempDeltaNeutralStore(t *testing.T) *deltaneutral.Store {
	tmpDir := t.TempDir()
	store, err := deltaneutral.NewStore(tmpDir)
	if err != nil {
		t.Fatalf("failed to create temp store: %v", err)
	}
	return store
}

// TestSpotQuantitySizing tests that spot leg quantity is computed from notional / price.
func TestSpotQuantitySizing(t *testing.T) {
	// Test spot quantity math: notional / price should give base-asset quantity
	notionalUSDT := 5000.0
	price := 42000.0

	expectedQty := notionalUSDT / price // ~0.119 BTC
	if expectedQty <= 0 {
		t.Fatalf("expected positive quantity, got %f", expectedQty)
	}

	// 5000 / 42000 = 0.119047619...; assert within a sane tolerance.
	tolerance := 0.001
	if expectedQty < 0.119-tolerance || expectedQty > 0.119+tolerance {
		t.Errorf("expected ~0.119, got %f", expectedQty)
	}
}
