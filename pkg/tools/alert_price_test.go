package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
)

func newTestCronServiceForAlerts(t *testing.T) *cron.CronService {
	t.Helper()
	return cron.NewCronService(filepath.Join(t.TempDir(), "alerts.json"), nil)
}

func TestSetPriceAlert_MissingAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when action is missing")
	}
}

func TestSetPriceAlert_UnknownAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "unknown",
	})
	if !result.IsError {
		t.Fatal("expected error for unknown action")
	}
}

func TestSetPriceAlert_ListAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestSetPriceAlert_CancelMissingID(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "cancel",
	})
	if !result.IsError {
		t.Fatal("expected error when alert_id is missing for cancel")
	}
}

func TestSetPriceAlert_CancelEmptyID(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":   "cancel",
		"alert_id": "",
	})
	if !result.IsError {
		t.Fatal("expected error when alert_id is empty")
	}
}

func TestSetPriceAlert_CancelNonexistent(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":   "cancel",
		"alert_id": "nonexistent",
	})
	if !result.IsError {
		t.Fatal("expected error when alert doesn't exist")
	}
}

func TestSetPriceAlert_CreateMissingProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"symbol":    "BTC/USDT",
		"condition": "above",
		"threshold": 50000.0,
	})
	if !result.IsError {
		t.Fatal("expected error when provider is missing")
	}
}

func TestSetPriceAlert_CreateMissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"condition": "above",
		"threshold": 50000.0,
	})
	if !result.IsError {
		t.Fatal("expected error when symbol is missing")
	}
}

func TestSetPriceAlert_CreateMissingCondition(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"threshold": 50000.0,
	})
	if !result.IsError {
		t.Fatal("expected error when condition is missing")
	}
}

func TestSetPriceAlert_CreateInvalidCondition(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "invalid",
		"threshold": 50000.0,
	})
	if !result.IsError {
		t.Fatal("expected error for invalid condition")
	}
}

func TestSetPriceAlert_CreateValidConditions(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	validConditions := []string{"above", "below"}
	for _, cond := range validConditions {
		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"condition": cond,
			"threshold": 50000.0,
		})
		// Should create the alert
		if result.IsError {
			t.Fatalf("unexpected error for condition %s: %s", cond, result.ForLLM)
		}
	}
}

func TestSetPriceAlert_CreateMissingThreshold(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "above",
	})
	if !result.IsError {
		t.Fatal("expected error when threshold is missing")
	}
}

func TestSetPriceAlert_CreateZeroThreshold(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "above",
		"threshold": 0.0,
	})
	if !result.IsError {
		t.Fatal("expected error when threshold is zero")
	}
}

func TestSetPriceAlert_ParametersSchema(t *testing.T) {
	tool := NewSetPriceAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	params := tool.Parameters()

	if params == nil {
		t.Fatal("Parameters() should not return nil")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in Parameters")
	}

	expectedProps := []string{"action", "provider", "symbol", "condition", "threshold", "message", "channel", "chat_id", "recurring", "alert_id", "account"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q in Parameters", prop)
		}
	}
}

func TestSetPriceAlert_Name(t *testing.T) {
	tool := NewSetPriceAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	name := tool.Name()
	if name != NameSetPriceAlert {
		t.Errorf("Name() = %q, want %q", name, NameSetPriceAlert)
	}
}

func TestSetPriceAlert_Description(t *testing.T) {
	tool := NewSetPriceAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	desc := tool.Description()
	if desc == "" {
		t.Fatal("Description() should not be empty")
	}
}

func TestSetPriceAlert_ListEmpty(t *testing.T) {
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestSetPriceAlert_ListWithAlerts(t *testing.T) {
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc)

	// Create an alert
	createResult := tool.Execute(WithToolContext(context.Background(), "telegram", "chat-1"), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "above",
		"threshold": 50000.0,
	})
	if createResult.IsError {
		t.Fatalf("failed to create alert: %s", createResult.ForLLM)
	}

	// List alerts
	listResult := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if listResult.IsError {
		t.Fatalf("unexpected error: %s", listResult.ForLLM)
	}
}

func TestSetPriceAlert_CancelSuccess(t *testing.T) {
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc)

	// Create an alert
	createResult := tool.Execute(WithToolContext(context.Background(), "telegram", "chat-1"), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "above",
		"threshold": 50000.0,
	})
	if createResult.IsError {
		t.Fatalf("failed to create alert: %s", createResult.ForLLM)
	}

	// Extract alert ID from the jobs
	jobs := cronSvc.ListJobs(false)
	if len(jobs) == 0 {
		t.Fatal("expected alert job to be created")
	}
	alertID := jobs[0].ID

	// Cancel the alert
	cancelResult := tool.Execute(context.Background(), map[string]any{
		"action":   "cancel",
		"alert_id": alertID,
	})
	if cancelResult.IsError {
		t.Fatalf("unexpected error: %s", cancelResult.ForLLM)
	}

	// Verify job was removed
	jobs = cronSvc.ListJobs(false)
	for _, j := range jobs {
		if j.ID == alertID {
			t.Fatal("alert was not removed after cancel")
		}
	}
}

func TestSetPriceAlert_CooldownStoredAndValidated(t *testing.T) {
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetPriceAlertTool(config.DefaultConfig(), cronSvc)
	ctx := WithToolContext(context.Background(), "test", "chat-1")
	args := func(cooldown float64) map[string]any {
		return map[string]any{
			"action": "create", "provider": "binance", "symbol": "BTC/USDT",
			"condition": "above", "threshold": 50000.0, "recurring": true,
			"cooldown_minutes": cooldown,
		}
	}

	if res := tool.Execute(ctx, args(-5)); !res.IsError {
		t.Fatal("negative cooldown_minutes must be rejected")
	}
	if res := tool.Execute(ctx, args(90)); res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	jobs := cronSvc.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	var p PriceAlertPayload
	if err := json.Unmarshal([]byte(jobs[0].Payload.Message), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.CooldownMinutes != 90 || !p.Recurring {
		t.Fatalf("unexpected payload: %+v", p)
	}
}

func TestPriceAlertPayload_LegacyJSONDecodes(t *testing.T) {
	legacy := `{"provider":"binance","account":"","symbol":"BTC/USDT","condition":"above","threshold":100,"alert_msg":"hi","recurring":true}`
	var p PriceAlertPayload
	if err := json.Unmarshal([]byte(legacy), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ProviderID != "binance" || p.Threshold != 100 || p.AlertMsg != "hi" || !p.Recurring {
		t.Fatalf("legacy fields not decoded: %+v", p)
	}
	if p.CooldownMinutes != 0 || EffectiveAlertCooldownMinutes(p.CooldownMinutes) != DefaultAlertCooldownMinutes {
		t.Fatalf("absent cooldown must default to %d", DefaultAlertCooldownMinutes)
	}
}
