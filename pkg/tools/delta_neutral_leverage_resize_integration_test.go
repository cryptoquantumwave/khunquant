package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

// TestCreateDeltaNeutralPlanSizesEqualNotional tests that create_delta_neutral_plan
// sizes both legs to equal notional using the formula N = (capital - reserve) * L / (L + 1).
func TestCreateDeltaNeutralPlanSizesEqualNotional(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}
	tool := NewCreateDeltaNeutralPlanTool(cfg, store, cronService)

	// Test case: capital=10000, leverage=2, reserve=500
	// Expected: N = (10000 - 500) * 2 / (2 + 1) = 9500 * 0.667 = 6333.33
	args := map[string]any{
		"plan_name":        "Test Equal Notional",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         2.0,
		"risk_policy": map[string]any{
			"reserve_margin_usdt": 500.0,
		},
	}

	result := tool.Execute(ctx, args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify the plan was saved with equal notionals
	plans, err := store.ListPlans(ctx, deltaneutral.QueryFilter{Limit: 10})
	if err != nil || len(plans) == 0 {
		t.Fatalf("Expected plan to be saved, got error or empty list")
	}

	plan := plans[0]

	// Expected notional: (10000 - 500) * 2 / 3 = 6333.33, rounded to 2 decimals
	expectedNotional := (10000.0 - 500.0) * 2.0 / 3.0
	expectedNotional = float64(int(expectedNotional*100)) / 100 // Round like the tool does

	if plan.SpotNotionalUSDT != expectedNotional {
		t.Errorf("Expected spot notional %.2f, got %.2f", expectedNotional, plan.SpotNotionalUSDT)
	}

	if plan.FuturesNotionalUSDT != expectedNotional {
		t.Errorf("Expected futures notional %.2f, got %.2f", expectedNotional, plan.FuturesNotionalUSDT)
	}

	if plan.SpotNotionalUSDT != plan.FuturesNotionalUSDT {
		t.Errorf("Expected spot and futures notionals to be equal, got %.2f and %.2f",
			plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT)
	}

	if plan.FuturesLeverage != 2 {
		t.Errorf("Expected leverage 2, got %d", plan.FuturesLeverage)
	}
}

// TestCreateDeltaNeutralPlanRejectsExcessiveLeverage tests that create tool rejects leverage > max_leverage.
func TestCreateDeltaNeutralPlanRejectsExcessiveLeverage(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}
	tool := NewCreateDeltaNeutralPlanTool(cfg, store, cronService)

	args := map[string]any{
		"plan_name":        "Test Excess Leverage",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         5.0, // Requested leverage
		"risk_policy": map[string]any{
			"max_leverage": 3.0, // Max allowed is 3
		},
	}

	result := tool.Execute(ctx, args)
	if !result.IsError {
		t.Error("Expected error when leverage exceeds max_leverage")
	}

	if !strings.Contains(result.ForLLM, "exceeds max_leverage") {
		t.Errorf("Expected 'exceeds max_leverage' in error, got: %s", result.ForLLM)
	}
}

// TestResizeDeltaNeutralPositionDecreaseValidation tests dry-run resize with -10% delta_pct.
func TestResizeDeltaNeutralPositionDecreaseValidation(t *testing.T) {
	resetRateLimiter(t)
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}

	// Create an active plan with 5000/5000 notional
	plan := &deltaneutral.Plan{
		Name:                "test-resize-decrease",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusActive,
		Mode:                deltaneutral.ExecutionModeApproval,
		SpotProvider:        "binance",
		SpotAccount:         "spot",
		SpotSymbol:          "BTC/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesAccount:      "futures",
		FuturesSymbol:       "BTC/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000.0,
		FuturesNotionalUSDT: 5000.0,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewResizeDeltaNeutralPositionTool(cfg, store)

	// Dry-run resize: -10% = -500 USDT on each leg
	result := tool.Execute(ctx, map[string]any{
		"plan_id":   float64(plan.ID),
		"delta_pct": -10.0,
		"confirm":   false,
	})

	if result.IsError {
		t.Fatalf("dry-run resize should not error, got: %s", result.ForLLM)
	}

	review := result.ForUser
	if review == "" {
		review = result.ForLLM
	}

	// Verify the review shows -500 delta and 4500/4500 new notionals
	if !strings.Contains(review, "4500") {
		t.Errorf("Expected '4500' (new notional) in review, got: %s", review)
	}

	if !strings.Contains(review, "-500") && !strings.Contains(review, "decrease") {
		t.Errorf("Expected decrease/delta info in review, got: %s", review)
	}

	// Verify no execution was created (dry-run)
	execs, err := store.ListExecutions(ctx, plan.ID, 10, 0)
	if err == nil && len(execs) > 0 {
		t.Error("dry-run should not create an execution record")
	}
}

// TestResizeDeltaNeutralPositionIncreaseValidation tests dry-run resize with +20% delta_pct.
func TestResizeDeltaNeutralPositionIncreaseValidation(t *testing.T) {
	resetRateLimiter(t)
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}

	// Create an active plan with 5000/5000 notional
	plan := &deltaneutral.Plan{
		Name:                "test-resize-increase",
		Asset:               "ETH",
		Status:              deltaneutral.PlanStatusActive,
		Mode:                deltaneutral.ExecutionModeApproval,
		SpotProvider:        "binance",
		SpotAccount:         "spot",
		SpotSymbol:          "ETH/USDT",
		SpotSide:            "buy",
		FuturesProvider:     "binance",
		FuturesAccount:      "futures",
		FuturesSymbol:       "ETH/USDT:USDT",
		FuturesSide:         "short",
		FuturesLeverage:     1,
		FuturesMarginMode:   "cross",
		CapitalUSDT:         10000,
		SpotNotionalUSDT:    5000.0,
		FuturesNotionalUSDT: 5000.0,
		Enabled:             true,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewResizeDeltaNeutralPositionTool(cfg, store)

	// Dry-run resize: +20% = +1000 USDT on each leg
	result := tool.Execute(ctx, map[string]any{
		"plan_id":   float64(plan.ID),
		"delta_pct": 20.0,
		"confirm":   false,
	})

	if result.IsError {
		t.Fatalf("dry-run resize should not error, got: %s", result.ForLLM)
	}

	review := result.ForUser
	if review == "" {
		review = result.ForLLM
	}

	// Verify the review shows +1000 delta and 6000/6000 new notionals
	if !strings.Contains(review, "6000") {
		t.Errorf("Expected '6000' (new notional) in review, got: %s", review)
	}

	if !strings.Contains(review, "increase") {
		t.Errorf("Expected 'increase' in review, got: %s", review)
	}

	// Verify no execution was created (dry-run)
	execs, err := store.ListExecutions(ctx, plan.ID, 10, 0)
	if err == nil && len(execs) > 0 {
		t.Error("dry-run should not create an execution record")
	}
}

// TestResizeDeltaNeutralPositionExactlyOneParamRequired tests parameter validation.
func TestResizeDeltaNeutralPositionExactlyOneParamRequired(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cfg := &config.Config{}

	plan := &deltaneutral.Plan{
		Name:                "test-param",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusActive,
		Mode:                deltaneutral.ExecutionModeApproval,
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

	tool := NewResizeDeltaNeutralPositionTool(cfg, store)

	tests := []struct {
		name     string
		args     map[string]any
		wantErr  bool
		errSubst string
	}{
		{
			name: "both_params_provided",
			args: map[string]any{
				"plan_id":             float64(plan.ID),
				"delta_pct":           10.0,
				"delta_notional_usdt": 1000.0,
				"confirm":             false,
			},
			wantErr:  true,
			errSubst: "exactly one",
		},
		{
			name: "neither_param_provided",
			args: map[string]any{
				"plan_id": float64(plan.ID),
				"confirm": false,
			},
			wantErr:  true,
			errSubst: "exactly one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(ctx, tt.args)
			if !tt.wantErr && result.IsError {
				t.Errorf("Expected success, got error: %s", result.ForLLM)
			}
			if tt.wantErr && !result.IsError {
				t.Errorf("Expected error, got success")
			}
			if tt.wantErr && !strings.Contains(result.ForLLM, tt.errSubst) {
				t.Errorf("Expected error containing '%s', got: %s", tt.errSubst, result.ForLLM)
			}
		})
	}
}

// TestResizeDeltaNeutralPositionNonActiveRejected tests that only active plans can be resized.
func TestResizeDeltaNeutralPositionNonActiveRejected(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cfg := &config.Config{}

	plan := &deltaneutral.Plan{
		Name:                "test-draft",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusDraft,
		Mode:                deltaneutral.ExecutionModeApproval,
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

	tool := NewResizeDeltaNeutralPositionTool(cfg, store)

	result := tool.Execute(ctx, map[string]any{
		"plan_id":   float64(plan.ID),
		"delta_pct": -10.0,
		"confirm":   false,
	})

	if !result.IsError {
		t.Error("Expected error for draft plan resize")
	}

	if !strings.Contains(result.ForLLM, "active") {
		t.Errorf("Expected 'active' in error message, got: %s", result.ForLLM)
	}
}

// TestUpdateDeltaNeutralPlanLeverageOnActiveRequiresConfirm tests leverage changes on active plans.
func TestUpdateDeltaNeutralPlanLeverageOnActiveRequiresConfirm(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{
		TradingRisk: config.TradingRiskConfig{
			AllowLeverage: true,
		},
	}

	// Create an active plan
	plan := &deltaneutral.Plan{
		Name:                "test-active",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusActive,
		Mode:                deltaneutral.ExecutionModeApproval,
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
		RiskPolicy:          deltaneutral.DefaultRiskPolicy(),
		CronJobID:           "test-cron",
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)

	// Try to change leverage without confirm=true
	result := tool.Execute(ctx, map[string]any{
		"plan_id":  float64(plan.ID),
		"leverage": 2.0,
		"confirm":  false,
	})

	if !result.IsError {
		t.Error("Expected error when changing leverage on active plan without confirm=true")
	}

	if !strings.Contains(result.ForLLM, "confirm") {
		t.Errorf("Expected 'confirm' in error message, got: %s", result.ForLLM)
	}
}

// TestUpdateDeltaNeutralPlanLeverageEnforcesMaxLeverage tests max_leverage enforcement.
func TestUpdateDeltaNeutralPlanLeverageEnforcesMaxLeverage(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a draft plan with max_leverage=3
	riskPolicy := deltaneutral.DefaultRiskPolicy()
	riskPolicy.MaxLeverage = 3

	plan := &deltaneutral.Plan{
		Name:                "test-draft-lev",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusDraft,
		Mode:                deltaneutral.ExecutionModeApproval,
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
		RiskPolicy:          riskPolicy,
		CronJobID:           "test-cron",
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)

	// Try to set leverage=5 when max_leverage=3
	result := tool.Execute(ctx, map[string]any{
		"plan_id":  float64(plan.ID),
		"leverage": 5.0,
	})

	if !result.IsError {
		t.Error("Expected error when leverage exceeds max_leverage")
	}

	if !strings.Contains(result.ForLLM, "exceeds max_leverage") {
		t.Errorf("Expected 'exceeds max_leverage' in error, got: %s", result.ForLLM)
	}
}

// TestUpdateDeltaNeutralPlanLeverageOnDraftStored tests that draft plans store leverage for next open.
func TestUpdateDeltaNeutralPlanLeverageOnDraftStored(t *testing.T) {
	ctx := context.Background()
	store := setupTempDeltaNeutralStore(t)
	defer store.Close()

	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a draft plan
	plan := &deltaneutral.Plan{
		Name:                "test-draft-store",
		Asset:               "BTC",
		Status:              deltaneutral.PlanStatusDraft,
		Mode:                deltaneutral.ExecutionModeApproval,
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
		RiskPolicy:          deltaneutral.DefaultRiskPolicy(),
		CronJobID:           "test-cron",
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	_, err := store.SavePlan(ctx, plan)
	if err != nil {
		t.Fatalf("failed to save plan: %v", err)
	}

	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)

	// Update leverage on draft plan (should succeed without confirm)
	result := tool.Execute(ctx, map[string]any{
		"plan_id":  float64(plan.ID),
		"leverage": 2.0,
	})

	if result.IsError {
		t.Fatalf("Expected success updating draft plan leverage, got error: %s", result.ForLLM)
	}

	// Verify leverage was updated
	updated, err := store.GetPlan(ctx, plan.ID)
	if err != nil {
		t.Fatalf("failed to get plan: %v", err)
	}

	if updated.FuturesLeverage != 2 {
		t.Errorf("Expected leverage 2, got %d", updated.FuturesLeverage)
	}
}
