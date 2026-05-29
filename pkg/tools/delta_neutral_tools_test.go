package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
)

func newTestCronServiceForDN(t *testing.T) *cron.CronService {
	t.Helper()
	return cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
}

func TestCreateDeltaNeutralPlanTool_RejectBitkubFutures(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Try to use bitkub as futures_provider (should reject)
	args := map[string]any{
		"plan_name":        "Test Plan",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "bitkub", // spot-only provider
		"futures_symbol":   "BTC/USDT",
		"capital_usdt":     1000.0,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for bitkub futures_provider, got success")
	}
	if result.ForLLM == "" {
		t.Errorf("Expected error message, got empty string")
	}
}

func TestCreateDeltaNeutralPlanTool_RejectBinanceEthFutures(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Try to use binanceth as futures_provider (should reject)
	args := map[string]any{
		"plan_name":        "Test Plan",
		"asset":            "ETH",
		"spot_provider":    "binance",
		"spot_symbol":      "ETH/USDT",
		"futures_provider": "binanceth", // spot-only provider
		"futures_symbol":   "ETH/USDT:USDT",
		"capital_usdt":     5000.0,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for binanceth futures_provider, got success")
	}
}

func TestCreateDeltaNeutralPlanTool_RejectInvalidMonitorInterval(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	args := map[string]any{
		"plan_name":        "Test Plan",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     1000.0,
		"monitor_interval": "2m30s", // invalid
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for invalid monitor_interval, got success")
	}
}

func TestCreateDeltaNeutralPlanTool_Success(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	args := map[string]any{
		"plan_name":        "BTC Funding Harvest Q1",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_account":     "my_spot",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_account":  "my_futures",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         5.0,
		"monitor_interval": "5m",
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify the plan was saved
	plans, err := store.ListPlans(context.Background(), deltaneutral.QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("Failed to list plans: %v", err)
	}
	if len(plans) == 0 {
		t.Errorf("Expected 1 plan to be saved, got 0")
	}
	if len(plans) > 0 {
		plan := plans[0]
		if plan.Name != "BTC Funding Harvest Q1" {
			t.Errorf("Expected plan name 'BTC Funding Harvest Q1', got '%s'", plan.Name)
		}
		if plan.Status != deltaneutral.PlanStatusDraft {
			t.Errorf("Expected status 'draft', got '%s'", plan.Status)
		}
		if plan.FuturesLeverage != 5 {
			t.Errorf("Expected leverage 5, got %d", plan.FuturesLeverage)
		}
		if !plan.Enabled {
			t.Errorf("Expected plan to be enabled by default")
		}
		if plan.CronJobID == "" {
			t.Errorf("Expected cron job ID to be set")
		}
	}
}

func TestCreateDeltaNeutralPlanTool_CrossExchange(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	args := map[string]any{
		"plan_name":        "Cross-Exchange BTC",
		"asset":            "BTC",
		"spot_provider":    "bitkub",
		"spot_symbol":      "BTC/THB",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     5000.0,
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success for cross-exchange setup, got error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), deltaneutral.QueryFilter{Limit: 10})
	if len(plans) > 0 && !plans[0].CrossExchange {
		t.Errorf("Expected CrossExchange flag to be true for different providers")
	}
}

func TestUpdateDeltaNeutralPlanTool_ChangeMonitorInterval(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	// Create a plan first
	plan := &deltaneutral.Plan{
		Name:            "Test Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusDraft,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)
	plan.ID = planID

	// Add a dummy cron job
	ms, _ := deltaneutral.IntervalToMS("5m")
	job, _ := cronService.AddJob(
		"test-dn-job",
		cron.CronSchedule{Kind: "every", EveryMS: &ms},
		"test message",
		false,
		"",
		"",
	)
	plan.CronJobID = job.ID
	store.UpdatePlan(context.Background(), plan)

	// Now update the monitor interval
	tool := NewUpdateDeltaNeutralPlanTool(&config.Config{}, store, cronService)
	args := map[string]any{
		"plan_id":          float64(planID),
		"monitor_interval": "10m",
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify the plan was updated
	updated, _ := store.GetPlan(context.Background(), planID)
	if updated.MonitorInterval != "10m" {
		t.Errorf("Expected monitor_interval to be '10m', got '%s'", updated.MonitorInterval)
	}
}

func TestDeleteDeltaNeutralPlanTool_RejectActivePlan(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	// Create an ACTIVE plan
	plan := &deltaneutral.Plan{
		Name:            "Active Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusActive, // ACTIVE
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	tool := NewDeleteDeltaNeutralPlanTool(store, cronService)
	args := map[string]any{
		"plan_id": float64(planID),
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error when deleting active plan, got success")
	}

	// Verify plan still exists
	existing, _ := store.GetPlan(context.Background(), planID)
	if existing == nil {
		t.Errorf("Expected plan to still exist after failed deletion")
	}
}

func TestDeleteDeltaNeutralPlanTool_AllowDraftPlan(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	// Create a DRAFT plan
	plan := &deltaneutral.Plan{
		Name:            "Draft Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusDraft, // DRAFT
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	tool := NewDeleteDeltaNeutralPlanTool(store, cronService)
	args := map[string]any{
		"plan_id": float64(planID),
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success deleting draft plan, got error: %s", result.ForLLM)
	}

	// Verify plan was deleted
	existing, err := store.GetPlan(context.Background(), planID)
	if existing != nil {
		t.Errorf("Expected plan to be deleted, but it still exists")
	}
	if err == nil {
		t.Errorf("Expected error when fetching deleted plan")
	}
}

func TestGetDeltaNeutralSummaryTool(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())

	// Create a plan
	plan := &deltaneutral.Plan{
		Name:            "Test Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusActive,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     10000.0,
		MonitorInterval: "5m",
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	tool := NewGetDeltaNeutralSummaryTool(store)
	args := map[string]any{
		"plan_id": float64(planID),
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}
	if result.ForUser == "" {
		t.Errorf("Expected output for user, got empty string")
	}
}

func TestGetDeltaNeutralHistoryTool(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())

	// Create a plan
	plan := &deltaneutral.Plan{
		Name:            "Test Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusActive,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     10000.0,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	tool := NewGetDeltaNeutralHistoryTool(store)
	args := map[string]any{
		"plan_id": float64(planID),
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}
	if result.ForUser == "" {
		t.Errorf("Expected output for user, got empty string")
	}
}

func TestCreateDeltaNeutralPlanTool_EqualNotionalSizing(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Test case 1: capital 10000, leverage 1, no reserve
	// N = (10000 - 0) * 1 / (1 + 1) = 5000
	args := map[string]any{
		"plan_name":        "Equal Notional Test 1",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_account":     "my_spot",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_account":  "my_futures",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         1.0,
		"monitor_interval": "5m",
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), deltaneutral.QueryFilter{Limit: 10})
	if len(plans) == 0 {
		t.Fatalf("Expected plan to be saved")
	}
	plan := plans[len(plans)-1]
	expectedNotional := 5000.0
	if plan.SpotNotionalUSDT != expectedNotional || plan.FuturesNotionalUSDT != expectedNotional {
		t.Errorf("Expected both notionals to be %.2f, got spot=%.2f, futures=%.2f",
			expectedNotional, plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT)
	}
	if plan.ReserveMarginUSDT != 0 {
		t.Errorf("Expected reserve margin 0, got %.2f", plan.ReserveMarginUSDT)
	}
}

func TestCreateDeltaNeutralPlanTool_SizingWithReserve(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Test case 2: capital 10000, leverage 3, reserve 1000
	// N = (10000 - 1000) * 3 / (3 + 1) = 9000 * 3 / 4 = 6750
	args := map[string]any{
		"plan_name":        "Equal Notional Test 2",
		"asset":            "ETH",
		"spot_provider":    "binance",
		"spot_symbol":      "ETH/USDT",
		"futures_provider": "okx",
		"futures_symbol":   "ETH/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         3.0,
		"monitor_interval": "5m",
		"risk_policy": map[string]any{
			"reserve_margin_usdt": 1000.0,
		},
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), deltaneutral.QueryFilter{Limit: 10})
	if len(plans) == 0 {
		t.Fatalf("Expected plan to be saved")
	}
	plan := plans[len(plans)-1]
	expectedNotional := 6750.0
	if plan.SpotNotionalUSDT != expectedNotional || plan.FuturesNotionalUSDT != expectedNotional {
		t.Errorf("Expected both notionals to be %.2f, got spot=%.2f, futures=%.2f",
			expectedNotional, plan.SpotNotionalUSDT, plan.FuturesNotionalUSDT)
	}
	if plan.ReserveMarginUSDT != 1000.0 {
		t.Errorf("Expected reserve margin 1000.0, got %.2f", plan.ReserveMarginUSDT)
	}
}

func TestCreateDeltaNeutralPlanTool_RejectLeverageExceedsMax(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Try leverage 10 with max_leverage 5 (should reject)
	args := map[string]any{
		"plan_name":        "Leverage Test",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         10.0,
		"monitor_interval": "5m",
		"risk_policy": map[string]any{
			"max_leverage": 5.0,
		},
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for leverage 10 with max_leverage 5, got success")
	}
	if result.ForLLM == "" {
		t.Errorf("Expected error message, got empty string")
	}
}

func TestCreateDeltaNeutralPlanTool_RejectReserveNotLessThanCapital(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	// Try reserve >= capital (should reject)
	args := map[string]any{
		"plan_name":        "Reserve Test",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         1.0,
		"monitor_interval": "5m",
		"risk_policy": map[string]any{
			"reserve_margin_usdt": 10000.0, // equal to capital, should reject
		},
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for reserve >= capital, got success")
	}
	if result.ForLLM == "" {
		t.Errorf("Expected error message, got empty string")
	}
}

func TestCreateDeltaNeutralPlanTool_SummaryIncludesNotionalAndReserve(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)

	tool := NewCreateDeltaNeutralPlanTool(&config.Config{}, store, cronService)

	args := map[string]any{
		"plan_name":        "Summary Test",
		"asset":            "BTC",
		"spot_provider":    "binance",
		"spot_account":     "spot1",
		"spot_symbol":      "BTC/USDT",
		"futures_provider": "binance",
		"futures_account":  "fut1",
		"futures_symbol":   "BTC/USDT:USDT",
		"capital_usdt":     10000.0,
		"leverage":         2.0,
		"monitor_interval": "5m",
		"risk_policy": map[string]any{
			"reserve_margin_usdt": 500.0,
		},
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify the summary output includes notional and reserve
	output := result.ForUser
	if !strings.Contains(output, "notional") {
		t.Errorf("Expected 'notional' in summary output")
	}
	if !strings.Contains(output, "Reserve margin") {
		t.Errorf("Expected 'Reserve margin' in summary output")
	}
}

// --- Leverage update tests ---

func TestUpdateDeltaNeutralPlanTool_DraftPlanLeverageStored(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a draft plan with leverage 2
	plan := &deltaneutral.Plan{
		Name:            "Draft Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusDraft,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		FuturesLeverage: 2,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	// Update leverage to 3 (no exchange call needed for draft)
	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)
	args := map[string]any{
		"plan_id":  float64(planID),
		"leverage": 3.0,
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify leverage was stored
	updated, _ := store.GetPlan(context.Background(), planID)
	if updated.FuturesLeverage != 3 {
		t.Errorf("Expected leverage 3, got %d", updated.FuturesLeverage)
	}
}

func TestUpdateDeltaNeutralPlanTool_LeverageExceedsMax(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a draft plan with max_leverage 5
	policy := deltaneutral.DefaultRiskPolicy()
	policy.MaxLeverage = 5
	plan := &deltaneutral.Plan{
		Name:            "Draft Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusDraft,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		FuturesLeverage: 2,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      policy,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	// Try to update leverage to 10 (exceeds max_leverage 5)
	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)
	args := map[string]any{
		"plan_id":  float64(planID),
		"leverage": 10.0,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for leverage exceeding max_leverage, got success")
	}
	if !strings.Contains(result.ForLLM, "exceeds max_leverage") {
		t.Errorf("Expected error message about max_leverage, got: %s", result.ForLLM)
	}

	// Verify leverage was NOT changed
	updated, _ := store.GetPlan(context.Background(), planID)
	if updated.FuturesLeverage != 2 {
		t.Errorf("Expected leverage to remain 2, got %d", updated.FuturesLeverage)
	}
}

func TestUpdateDeltaNeutralPlanTool_ActivePlanWithoutConfirm(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create an active plan
	plan := &deltaneutral.Plan{
		Name:            "Active Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusActive,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		FuturesLeverage: 2,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	// Try to update leverage WITHOUT confirm=true (should fail)
	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)
	args := map[string]any{
		"plan_id":  float64(planID),
		"leverage": 3.0,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for active plan without confirm, got success")
	}
	if !strings.Contains(result.ForLLM, "requires confirm=true") {
		t.Errorf("Expected error message about confirm, got: %s", result.ForLLM)
	}

	// Verify leverage was NOT changed
	updated, _ := store.GetPlan(context.Background(), planID)
	if updated.FuturesLeverage != 2 {
		t.Errorf("Expected leverage to remain 2, got %d", updated.FuturesLeverage)
	}
}

func TestUpdateDeltaNeutralPlanTool_ReadyPlanLeverageStored(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a ready plan
	plan := &deltaneutral.Plan{
		Name:            "Ready Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusReady,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		FuturesLeverage: 2,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	// Update leverage (no exchange call needed for ready status)
	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)
	args := map[string]any{
		"plan_id":  float64(planID),
		"leverage": 4.0,
	}

	result := tool.Execute(context.Background(), args)
	if result.IsError {
		t.Fatalf("Expected success, got error: %s", result.ForLLM)
	}

	// Verify leverage was stored
	updated, _ := store.GetPlan(context.Background(), planID)
	if updated.FuturesLeverage != 4 {
		t.Errorf("Expected leverage 4, got %d", updated.FuturesLeverage)
	}
}

func TestUpdateDeltaNeutralPlanTool_RejectClosedPlanLeverage(t *testing.T) {
	store, _ := deltaneutral.NewStore(t.TempDir())
	cronService := newTestCronServiceForDN(t)
	cfg := &config.Config{}

	// Create a closed plan
	plan := &deltaneutral.Plan{
		Name:            "Closed Plan",
		Asset:           "BTC",
		Status:          deltaneutral.PlanStatusClosed,
		Mode:            deltaneutral.ExecutionModeApproval,
		SpotProvider:    "binance",
		SpotSymbol:      "BTC/USDT",
		FuturesProvider: "binance",
		FuturesSymbol:   "BTC/USDT:USDT",
		CapitalUSDT:     1000.0,
		FuturesLeverage: 2,
		MonitorInterval: "5m",
		Enabled:         true,
		RiskPolicy:      deltaneutral.DefaultRiskPolicy(),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}
	planID, _ := store.SavePlan(context.Background(), plan)

	// Try to update leverage on a closed plan
	tool := NewUpdateDeltaNeutralPlanTool(cfg, store, cronService)
	args := map[string]any{
		"plan_id":  float64(planID),
		"leverage": 3.0,
	}

	result := tool.Execute(context.Background(), args)
	if !result.IsError {
		t.Errorf("Expected error for closed plan, got success")
	}
	if !strings.Contains(result.ForLLM, "cannot change leverage") {
		t.Errorf("Expected error message about closed plan, got: %s", result.ForLLM)
	}
}
