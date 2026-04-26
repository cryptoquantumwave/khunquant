package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

func newTestDCAStore(t *testing.T) *dca.Store {
	t.Helper()
	s, err := dca.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("dca.NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestCronService(t *testing.T) *cron.CronService {
	t.Helper()
	return cron.NewCronService(filepath.Join(t.TempDir(), "cron.json"), nil)
}

func newTestCreatePlanTool(t *testing.T) *CreateDCAPlanTool {
	t.Helper()
	return NewCreateDCAPlanTool(config.DefaultConfig(), newTestDCAStore(t), newTestCronService(t))
}

func testCtx() context.Context {
	return WithToolContext(context.Background(), "test", "user-1")
}

func TestCreateDCAPlan_MissingPlanName(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
	})
	if !result.IsError {
		t.Fatal("expected error when plan_name is missing")
	}
}

func TestCreateDCAPlan_MissingProvider(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
	})
	if !result.IsError {
		t.Fatal("expected error when provider is missing")
	}
}

func TestCreateDCAPlan_InvalidAmount(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(0),
		"frequency_expr":   "0 9 * * 1",
	})
	if !result.IsError {
		t.Fatal("expected error for amount_per_order=0")
	}
}

func TestCreateDCAPlan_InvalidCronExpr(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "not-valid-cron",
	})
	if !result.IsError {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestCreateDCAPlan_NoFrequencyExpr(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
	})
	if !result.IsError {
		t.Fatal("expected error when frequency_expr is missing and trigger_type is not indicator")
	}
	if !strings.Contains(result.ForLLM, "frequency_expr is required") {
		t.Errorf("expected 'frequency_expr is required', got: %s", result.ForLLM)
	}
}

func TestCreateDCAPlan_Success(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Weekly BTC",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(500),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "DCA plan created") {
		t.Errorf("expected success message, got: %s", result.ForUser)
	}

	// Verify plan persisted in store
	plans, err := store.ListPlans(context.Background(), dca.QueryFilter{})
	if err != nil {
		t.Fatalf("ListPlans: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plans))
	}
	if plans[0].Name != "Weekly BTC" {
		t.Errorf("plan name = %q, want Weekly BTC", plans[0].Name)
	}
}

func TestCreateDCAPlan_DefaultSideBuy(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	tool.Execute(testCtx(), map[string]any{
		"plan_name":        "BuyDefault",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].Side != "buy" {
		t.Errorf("side = %q, want buy", plans[0].Side)
	}
}

func TestCreateDCAPlan_SellSide(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	tool.Execute(testCtx(), map[string]any{
		"plan_name":        "SellPlan",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"side":             "sell",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].Side != "sell" {
		t.Errorf("side = %q, want sell", plans[0].Side)
	}
}

func TestCreateDCAPlan_WithEndDate(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "EndDatePlan",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"end_date":         "2099-12-31",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].EndDate == nil {
		t.Fatal("expected EndDate to be set")
	}
	if plans[0].EndDate.Year() != 2099 {
		t.Errorf("EndDate year = %d, want 2099", plans[0].EndDate.Year())
	}
}

func TestCreateDCAPlan_WithIndicatorTrigger(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":          "RSI DCA",
		"provider":           "bitkub",
		"symbol":             "BTC/THB",
		"amount_per_order":   float64(100),
		"trigger_type":       "indicator",
		"trigger_indicator":  "rsi",
		"trigger_condition":  "oversold",
		"trigger_timeframe":  "1h",
		"notify_channel":     "test",
		"notify_chat_id":     "user-1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "0 * * * *") {
		t.Errorf("expected auto-derived 1h cron '0 * * * *' in output, got: %s", result.ForUser)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].TriggerConfig == nil {
		t.Fatal("expected TriggerConfig to be set")
	}
	if plans[0].TriggerConfig.Indicator != "rsi" {
		t.Errorf("indicator = %q, want rsi", plans[0].TriggerConfig.Indicator)
	}
}

func TestCreateDCAPlan_IndicatorMissingIndicator(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":         "Test",
		"provider":          "bitkub",
		"symbol":            "BTC/THB",
		"amount_per_order":  float64(100),
		"trigger_type":      "indicator",
		"trigger_condition": "oversold",
		"trigger_timeframe": "1h",
		"notify_channel":    "test",
		"notify_chat_id":    "user-1",
	})
	if !result.IsError {
		t.Fatal("expected error when trigger_indicator is missing")
	}
}

func TestCreateDCAPlan_IndicatorMissingCondition(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":         "Test",
		"provider":          "bitkub",
		"symbol":            "BTC/THB",
		"amount_per_order":  float64(100),
		"trigger_type":      "indicator",
		"trigger_indicator": "rsi",
		"trigger_timeframe": "1h",
		"notify_channel":    "test",
		"notify_chat_id":    "user-1",
	})
	if !result.IsError {
		t.Fatal("expected error when trigger_condition is missing")
	}
}

func TestCreateDCAPlan_IndicatorMissingTimeframe(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":         "Test",
		"provider":          "bitkub",
		"symbol":            "BTC/THB",
		"amount_per_order":  float64(100),
		"trigger_type":      "indicator",
		"trigger_indicator": "rsi",
		"trigger_condition": "oversold",
		"notify_channel":    "test",
		"notify_chat_id":    "user-1",
	})
	if !result.IsError {
		t.Fatal("expected error when trigger_timeframe is missing")
	}
}

func TestCreateDCAPlan_GuardrailMissingPeriod(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":           "Test",
		"provider":            "bitkub",
		"symbol":              "BTC/THB",
		"amount_per_order":    float64(100),
		"frequency_expr":      "0 9 * * 1",
		"max_exec_per_period": float64(2),
		// missing exec_period
		"notify_channel": "test",
		"notify_chat_id": "user-1",
	})
	if !result.IsError {
		t.Fatal("expected error when exec_period is missing but max_exec_per_period is set")
	}
	if !strings.Contains(result.ForLLM, "exec_period is required") {
		t.Errorf("expected 'exec_period is required', got: %s", result.ForLLM)
	}
}

func TestCreateDCAPlan_NoHistoryOnJob(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, cronSvc)

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "NoHistoryTest",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	jobs := cronSvc.ListJobs(false)
	var dcaJob *cron.CronJob
	for i := range jobs {
		if strings.HasPrefix(jobs[i].Name, "dca:") {
			dcaJob = &jobs[i]
			break
		}
	}
	if dcaJob == nil {
		t.Fatal("expected a DCA cron job to be registered")
	}
	if !dcaJob.Payload.NoHistory {
		t.Error("expected NoHistory=true on DCA cron job")
	}
}

func TestCreateDCAPlan_StartDate(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "StartDatePlan",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"start_date":       "2025-01-15",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].StartDate.Year() != 2025 {
		t.Errorf("StartDate year = %d, want 2025", plans[0].StartDate.Year())
	}
}

func TestCreateDCAPlan_InvalidEndDate(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"end_date":         "not-a-date",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if !result.IsError {
		t.Fatal("expected error for invalid end_date")
	}
}

func TestCreateDCAPlan_InvalidSide(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"side":             "hold",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
	})
	if !result.IsError {
		t.Fatal("expected error for invalid side")
	}
}

// Verify that notify routing defaults to tool context when not supplied.
func TestCreateDCAPlan_NotifyDefaultsToContext(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, cronSvc)

	ctx := WithToolContext(context.Background(), "telegram", "chat-99")
	tool.Execute(ctx, map[string]any{
		"plan_name":        "CtxNotify",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].NotifyChannel != "telegram" {
		t.Errorf("NotifyChannel = %q, want telegram", plans[0].NotifyChannel)
	}
	if plans[0].NotifyChatID != "chat-99" {
		t.Errorf("NotifyChatID = %q, want chat-99", plans[0].NotifyChatID)
	}
}

// ensure plan.CronJobID is stored after creation
func TestCreateDCAPlan_CronJobIDPersisted(t *testing.T) {
	store := newTestDCAStore(t)
	cronSvc := newTestCronService(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, cronSvc)

	tool.Execute(testCtx(), map[string]any{
		"plan_name":        "CronIDPlan",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].CronJobID == "" {
		t.Error("expected CronJobID to be stored in plan")
	}
}

// buildTriggerConfig validation: period limits
func TestBuildTriggerConfig_CandleLimitCapped(t *testing.T) {
	args := map[string]any{
		"trigger_indicator":    "rsi",
		"trigger_condition":    "oversold",
		"trigger_timeframe":    "1h",
		"trigger_candle_limit": float64(1000),
	}
	tc, result := buildTriggerConfig(args)
	if result != nil {
		t.Fatalf("expected no error, got: %s", result.ForLLM)
	}
	if tc.Limit != 500 {
		t.Errorf("candle_limit capped to %d, want 500", tc.Limit)
	}
}

func TestBuildTriggerConfig_CustomPeriods(t *testing.T) {
	args := map[string]any{
		"trigger_indicator":  "macd",
		"trigger_condition":  "histogram_positive",
		"trigger_timeframe":  "4h",
		"trigger_period":     float64(10),
		"trigger_period2":    float64(20),
		"trigger_period3":    float64(5),
		"trigger_multiplier": float64(1.5),
		"trigger_threshold":  float64(35.0),
	}
	tc, result := buildTriggerConfig(args)
	if result != nil {
		t.Fatalf("expected no error, got: %s", result.ForLLM)
	}
	if tc.Period != 10 || tc.Period2 != 20 || tc.Period3 != 5 {
		t.Errorf("periods = %d/%d/%d, want 10/20/5", tc.Period, tc.Period2, tc.Period3)
	}
	if tc.Multiplier != 1.5 {
		t.Errorf("multiplier = %g, want 1.5", tc.Multiplier)
	}
	if tc.Threshold != 35.0 {
		t.Errorf("threshold = %g, want 35.0", tc.Threshold)
	}
}

// Ensure end_date in the past is accepted at creation (expiry is only checked at execution)
func TestCreateDCAPlan_PastEndDate(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "PastEndPlan",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"end_date":         "2020-01-01",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})
	if result.IsError {
		t.Fatalf("creation with past end_date should succeed: %s", result.ForLLM)
	}
	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if plans[0].EndDate == nil || plans[0].EndDate.Year() != 2020 {
		t.Error("expected past end_date to be persisted")
	}
}

// verify timezone defaults to UTC when not provided
func TestCreateDCAPlan_DefaultTimezone(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	tool.Execute(testCtx(), map[string]any{
		"plan_name":        "TZDefault",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"frequency_expr":   "0 9 * * 1",
		"notify_channel":   "test",
		"notify_chat_id":   "user-1",
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("no plan created")
	}
	if plans[0].Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", plans[0].Timezone)
	}
}

