package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestSetIndicatorAlert_MissingAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{})
	if !result.IsError {
		t.Fatal("expected error when action is missing")
	}
}

func TestSetIndicatorAlert_UnknownAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "unknown",
	})
	if !result.IsError {
		t.Fatal("expected error for unknown action")
	}
}

func TestSetIndicatorAlert_ListAction(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "list",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestSetIndicatorAlert_CancelMissingID(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action": "cancel",
	})
	if !result.IsError {
		t.Fatal("expected error when alert_id is missing")
	}
}

func TestSetIndicatorAlert_CreateMissingProvider(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"symbol":    "BTC/USDT",
		"indicator": "RSI",
		"condition": "above",
		"threshold": 70.0,
	})
	if !result.IsError {
		t.Fatal("expected error when provider is missing")
	}
}

func TestSetIndicatorAlert_CreateMissingSymbol(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"indicator": "RSI",
		"condition": "above",
		"threshold": 70.0,
	})
	if !result.IsError {
		t.Fatal("expected error when symbol is missing")
	}
}

func TestSetIndicatorAlert_CreateMissingIndicator(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"condition": "above",
		"threshold": 70.0,
	})
	if !result.IsError {
		t.Fatal("expected error when indicator is missing")
	}
}

func TestSetIndicatorAlert_CreateMissingCondition(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(context.Background(), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"indicator": "RSI",
		"threshold": 70.0,
	})
	if !result.IsError {
		t.Fatal("expected error when condition is missing")
	}
}

func TestSetIndicatorAlert_CreateDefaultTimeframe(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
		"action":    "create",
		"provider":  "binance",
		"symbol":    "BTC/USDT",
		"indicator": "RSI",
		"condition": "above",
		"threshold": 70.0,
	})
	// Should default timeframe to 1h and create
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.ForLLM)
	}
}

func TestSetIndicatorAlert_CreateValidIndicators(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	validIndicators := []string{"RSI", "MACD", "SMA20", "EMA9"}
	for _, ind := range validIndicators {
		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"indicator": ind,
			"condition": "above",
			"threshold": 50.0,
		})
		if result.IsError {
			t.Fatalf("unexpected error for indicator %s: %s", ind, result.ForLLM)
		}
	}
}

func TestSetIndicatorAlert_CreateValidTimeframes(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	validTimeframes := []string{"1m", "5m", "15m", "1h", "4h", "1d", "1w"}
	for _, tf := range validTimeframes {
		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"indicator": "RSI",
			"timeframe": tf,
			"condition": "above",
			"threshold": 70.0,
		})
		if result.IsError {
			t.Fatalf("unexpected error for timeframe %s: %s", tf, result.ForLLM)
		}
	}
}

func TestSetIndicatorAlert_CreateValidConditions(t *testing.T) {
	cfg := config.DefaultConfig()
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(cfg, cronSvc)

	validConditions := []string{"above", "below"}
	for _, cond := range validConditions {
		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"indicator": "RSI",
			"condition": cond,
			"threshold": 70.0,
		})
		if result.IsError {
			t.Fatalf("unexpected error for condition %s: %s", cond, result.ForLLM)
		}
	}
}

func TestSetIndicatorAlert_ParametersSchema(t *testing.T) {
	tool := NewSetIndicatorAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	params := tool.Parameters()

	if params == nil {
		t.Fatal("Parameters() should not return nil")
	}

	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in Parameters")
	}

	expectedProps := []string{"action", "provider", "symbol", "indicator", "timeframe", "condition", "threshold", "message", "channel", "chat_id", "recurring", "alert_id", "account"}
	for _, prop := range expectedProps {
		if _, ok := props[prop]; !ok {
			t.Errorf("expected property %q in Parameters", prop)
		}
	}
}

func TestSetIndicatorAlert_Name(t *testing.T) {
	tool := NewSetIndicatorAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	name := tool.Name()
	if name != NameSetIndicatorAlert {
		t.Errorf("Name() = %q, want %q", name, NameSetIndicatorAlert)
	}
}

func TestSetIndicatorAlert_Description(t *testing.T) {
	tool := NewSetIndicatorAlertTool(config.DefaultConfig(), newTestCronServiceForAlerts(t))
	desc := tool.Description()
	if desc == "" {
		t.Fatal("Description() should not be empty")
	}
}

func TestIndicatorAlert_InvalidConditionRejected(t *testing.T) {
	for _, cond := range []string{"sideways", "cross_above", "ABOVE", "=="} {
		cronSvc := newTestCronServiceForAlerts(t)
		tool := NewSetIndicatorAlertTool(config.DefaultConfig(), cronSvc)

		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"indicator": "RSI",
			"condition": cond,
			"threshold": 70.0,
		})
		if !result.IsError {
			t.Fatalf("condition %q: expected an error", cond)
		}
		if !strings.Contains(result.ForLLM, "above") || !strings.Contains(result.ForLLM, "below") {
			t.Errorf("condition %q: error should list the allowed values, got %q", cond, result.ForLLM)
		}
		if n := len(cronSvc.ListJobs(true)); n != 0 {
			t.Errorf("condition %q: no job may be created, found %d", cond, n)
		}
	}
}

func TestIndicatorAlert_UnknownIndicatorRejected(t *testing.T) {
	for _, ind := range []string{"STOCH", "BOLL20", "vwap", "123"} {
		cronSvc := newTestCronServiceForAlerts(t)
		tool := NewSetIndicatorAlertTool(config.DefaultConfig(), cronSvc)

		result := tool.Execute(WithToolContext(context.Background(), "test", "chat-1"), map[string]any{
			"action":    "create",
			"provider":  "binance",
			"symbol":    "BTC/USDT",
			"indicator": ind,
			"condition": "above",
			"threshold": 70.0,
		})
		if !result.IsError {
			t.Fatalf("indicator %q: expected an error", ind)
		}
		for _, fam := range []string{"RSI", "MACD", "SMA", "EMA"} {
			if !strings.Contains(result.ForLLM, fam) {
				t.Errorf("indicator %q: error should list %s, got %q", ind, fam, result.ForLLM)
			}
		}
		if n := len(cronSvc.ListJobs(true)); n != 0 {
			t.Errorf("indicator %q: no job may be created, found %d", ind, n)
		}
	}
}

func TestIndicatorAlert_CooldownStoredAndValidated(t *testing.T) {
	cronSvc := newTestCronServiceForAlerts(t)
	tool := NewSetIndicatorAlertTool(config.DefaultConfig(), cronSvc)
	ctx := WithToolContext(context.Background(), "test", "chat-1")
	args := func(cooldown float64) map[string]any {
		return map[string]any{
			"action": "create", "provider": "binance", "symbol": "BTC/USDT",
			"indicator": "SMA20", "condition": "above", "threshold": 50.0, "recurring": true,
			"cooldown_minutes": cooldown,
		}
	}

	if res := tool.Execute(ctx, args(0)); !res.IsError {
		t.Fatal("cooldown_minutes=0 must be rejected")
	}
	if res := tool.Execute(ctx, args(15)); res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	jobs := cronSvc.ListJobs(true)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	var p IndicatorAlertPayload
	if err := json.Unmarshal([]byte(jobs[0].Payload.Message), &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.CooldownMinutes != 15 || p.Indicator != "SMA" || p.Period != 20 {
		t.Fatalf("unexpected payload: %+v", p)
	}
}
