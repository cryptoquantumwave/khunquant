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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "not-valid-cron"},
	})
	if !result.IsError {
		t.Fatal("expected error for invalid cron expression")
	}
}

func TestCreateDCAPlan_NoSchedule(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
	})
	if !result.IsError {
		t.Fatal("expected error when schedule.cron and trigger are both absent")
	}
	if !strings.Contains(result.ForLLM, "schedule.cron is required") {
		t.Errorf("expected 'schedule.cron is required', got: %s", result.ForLLM)
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"notify":           map[string]any{"channel": "test", "chat_id": "user-1"},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "DCA plan created") {
		t.Errorf("expected success message, got: %s", result.ForUser)
	}

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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"end_date":         "2099-12-31",
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
		"plan_name":        "RSI DCA",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"trigger": map[string]any{
			"timeframe": "1h",
			"indicators": []any{
				map[string]any{"alias": "rsi14", "kind": "rsi", "params": map[string]any{"period": float64(14)}},
			},
			"expression": "rsi14 < 30",
		},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
	// Auto-derived cron for 1h should appear in output.
	if !strings.Contains(result.ForUser, "0 * * * *") {
		t.Errorf("expected auto-derived 1h cron '0 * * * *' in output, got: %s", result.ForUser)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].Trigger == nil {
		t.Fatal("expected Trigger to be set")
	}
	if plans[0].Trigger.Expression != "rsi14 < 30" {
		t.Errorf("expression = %q, want rsi14 < 30", plans[0].Trigger.Expression)
	}
	if len(plans[0].Trigger.Indicators) != 1 || plans[0].Trigger.Indicators[0].Alias != "rsi14" {
		t.Errorf("unexpected indicators: %+v", plans[0].Trigger.Indicators)
	}
}

func TestCreateDCAPlan_TriggerMissingTimeframe(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"trigger": map[string]any{
			// missing timeframe
			"expression": "rsi14 < 30",
		},
	})
	if !result.IsError {
		t.Fatal("expected error when trigger.timeframe is missing")
	}
}

func TestCreateDCAPlan_TriggerMissingExpression(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"trigger": map[string]any{
			"timeframe": "1h",
			// missing expression
		},
	})
	if !result.IsError {
		t.Fatal("expected error when trigger.expression is missing")
	}
}

func TestCreateDCAPlan_TriggerBadAlias(t *testing.T) {
	// Compile-time check: expression references alias not declared in indicators.
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"trigger": map[string]any{
			"timeframe": "1h",
			"indicators": []any{
				map[string]any{"alias": "rsi14", "kind": "rsi"},
			},
			"expression": "rs14 < 30", // typo: rs14 instead of rsi14
		},
	})
	if !result.IsError {
		t.Fatal("expected compile error for undefined alias rs14")
	}
}

func TestCreateDCAPlan_TriggerLookbackClamped(t *testing.T) {
	tc, errResult := parseTrigger(map[string]any{
		"timeframe":  "1h",
		"lookback":   float64(9999), // above max 1000
		"expression": "close > open",
	})
	if errResult != nil {
		t.Fatalf("expected no error, got: %s", errResult.ForLLM)
	}
	if tc.Lookback != 1000 {
		t.Errorf("lookback clamped to %d, want 1000", tc.Lookback)
	}
}

func TestCreateDCAPlan_TriggerLookbackMinimum(t *testing.T) {
	tc, errResult := parseTrigger(map[string]any{
		"timeframe":  "1h",
		"lookback":   float64(5), // below min 30
		"expression": "close > open",
	})
	if errResult != nil {
		t.Fatalf("expected no error, got: %s", errResult.ForLLM)
	}
	if tc.Lookback != 30 {
		t.Errorf("lookback clamped to %d, want 30", tc.Lookback)
	}
}

func TestCreateDCAPlan_GuardrailMissingPeriod(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "Test",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(100),
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"guardrails":       map[string]any{"max_executions_per_period": float64(2)},
		// missing guardrails.period
	})
	if !result.IsError {
		t.Fatal("expected error when guardrails.period is missing but max_executions_per_period is set")
	}
	if !strings.Contains(result.ForLLM, "guardrails.period is required") {
		t.Errorf("expected 'guardrails.period is required', got: %s", result.ForLLM)
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"start_date":       "2025-01-15",
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"end_date":         "not-a-date",
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].CronJobID == "" {
		t.Error("expected CronJobID to be stored in plan")
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
		"end_date":         "2020-01-01",
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
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("no plan created")
	}
	if plans[0].Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", plans[0].Timezone)
	}
}

func TestCreateDCAPlan_AmountUnitBase(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "BTC Base Units",
		"provider":         "bitkub",
		"symbol":           "BTC/THB",
		"amount_per_order": float64(0.001),
		"amount_unit":      "base",
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].AmountUnit != "base" {
		t.Errorf("AmountUnit = %q, want base", plans[0].AmountUnit)
	}
}

func TestCreateDCAPlan_SettradeDefaultsToBase(t *testing.T) {
	store := newTestDCAStore(t)
	tool := NewCreateDCAPlanTool(config.DefaultConfig(), store, newTestCronService(t))

	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "PTT Shares",
		"provider":         "settrade",
		"symbol":           "PTT",
		"amount_per_order": float64(10),
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}

	plans, _ := store.ListPlans(context.Background(), dca.QueryFilter{})
	if len(plans) == 0 {
		t.Fatal("expected plan to be created")
	}
	if plans[0].AmountUnit != "base" {
		t.Errorf("AmountUnit = %q, want base (auto-defaulted for settrade)", plans[0].AmountUnit)
	}
}

func TestCreateDCAPlan_SettradeRejectsQuoteUnit(t *testing.T) {
	tool := newTestCreatePlanTool(t)
	result := tool.Execute(testCtx(), map[string]any{
		"plan_name":        "PTT Quote",
		"provider":         "settrade",
		"symbol":           "PTT",
		"amount_per_order": float64(1000),
		"amount_unit":      "quote",
		"schedule":         map[string]any{"cron": "0 9 * * 1"},
	})
	if !result.IsError {
		t.Fatal("expected error when using amount_unit=quote with settrade")
	}
}
