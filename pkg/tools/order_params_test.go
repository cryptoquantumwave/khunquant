package tools

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestSanitizeOrderParams_AllowsExecutionOptions(t *testing.T) {
	risk := config.TradingRiskConfig{}

	out, err := sanitizeOrderParams(map[string]interface{}{
		"clientOrderId": "abc-123",
		"timeInForce":   "GTC",
		"postOnly":      true,
		"reduceOnly":    true,
		"stopPrice":     100.5,
	}, risk)
	if err != nil {
		t.Fatalf("unexpected error for ordinary execution options: %v", err)
	}
	if len(out) != 5 {
		t.Errorf("expected all five options to be forwarded, got %v", out)
	}
	if out["clientOrderId"] != "abc-123" {
		t.Errorf("value was not preserved: %v", out)
	}
}

// TestSanitizeOrderParams_RefusesMarginModeWhenDisallowed is the H-5 case: a
// spot order turned into a cross-margin one through the passthrough dictionary
// while allow_margin was false.
func TestSanitizeOrderParams_RefusesMarginModeWhenDisallowed(t *testing.T) {
	risk := config.TradingRiskConfig{AllowMargin: false}

	for _, params := range []map[string]interface{}{
		{"tdMode": "cross"},
		{"marginMode": "isolated"},
		{"sideEffectType": "MARGIN_BUY"},
		{"autoBorrowRepay": true},
		{"isIsolated": "TRUE"},
	} {
		out, err := sanitizeOrderParams(params, risk)
		if err == nil {
			t.Errorf("sanitizeOrderParams(%v) was allowed with allow_margin=false; got %v", params, out)
		}
		if out != nil {
			t.Errorf("expected no parameters to be forwarded on refusal, got %v", out)
		}
	}
}

func TestSanitizeOrderParams_AllowsMarginModeWhenPermitted(t *testing.T) {
	risk := config.TradingRiskConfig{AllowMargin: true}

	out, err := sanitizeOrderParams(map[string]interface{}{"tdMode": "cross"}, risk)
	if err != nil {
		t.Fatalf("unexpected error with allow_margin=true: %v", err)
	}
	if out["tdMode"] != "cross" {
		t.Errorf("expected tdMode to be forwarded, got %v", out)
	}
}

func TestSanitizeOrderParams_LeverageKeysFollowTheLeverageFlag(t *testing.T) {
	if _, err := sanitizeOrderParams(
		map[string]interface{}{"leverage": 20},
		config.TradingRiskConfig{AllowLeverage: false},
	); err == nil {
		t.Error("leverage parameter was allowed with allow_leverage=false")
	}

	out, err := sanitizeOrderParams(
		map[string]interface{}{"leverage": 20},
		config.TradingRiskConfig{AllowLeverage: true},
	)
	if err != nil {
		t.Fatalf("unexpected error with allow_leverage=true: %v", err)
	}
	if out["leverage"] != 20 {
		t.Errorf("expected leverage to be forwarded, got %v", out)
	}
}

func TestSanitizeOrderParams_RefusesUnknownKeys(t *testing.T) {
	_, err := sanitizeOrderParams(
		map[string]interface{}{"someNewExchangeFlag": true},
		config.TradingRiskConfig{AllowMargin: true, AllowLeverage: true},
	)
	if err == nil {
		t.Fatal("an unrecognised parameter was forwarded")
	}
	if !strings.Contains(err.Error(), "someNewExchangeFlag") {
		t.Errorf("the refused key should be named so the model can correct itself, got: %v", err)
	}
}

func TestSanitizeOrderParams_EmptyInput(t *testing.T) {
	for _, raw := range []any{nil, map[string]interface{}{}, "not a map"} {
		out, err := sanitizeOrderParams(raw, config.TradingRiskConfig{})
		if err != nil {
			t.Errorf("sanitizeOrderParams(%v) errored: %v", raw, err)
		}
		if out != nil {
			t.Errorf("expected nil params for %v, got %v", raw, out)
		}
	}
}
