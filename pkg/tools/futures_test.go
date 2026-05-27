package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestNormalizeFuturesSymbol(t *testing.T) {
	tests := map[string]string{
		"BTCUSDT":       "BTC/USDT:USDT",
		"btc_usdt":      "BTC/USDT:USDT",
		"BTC/USDT":      "BTC/USDT:USDT",
		"BTC/USDT:USDT": "BTC/USDT:USDT",
		"ETH/USDC":      "ETH/USDC:USDC",
	}
	for input, want := range tests {
		if got := normalizeFuturesSymbol(input); got != want {
			t.Fatalf("normalizeFuturesSymbol(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFuturesOpenPosition_DryRunNormalizesSymbol(t *testing.T) {
	tool := NewFuturesOpenPositionTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "binance",
		"symbol":   "btcusdt",
		"side":     "long",
		"amount":   0.01,
		"leverage": 3.0,
		"confirm":  false,
	})
	if res.IsError {
		t.Fatalf("Execute returned error: %s", res.ForUser)
	}
	if !strings.Contains(res.ForUser, "BTC/USDT:USDT") {
		t.Fatalf("dry-run output missing normalized futures symbol: %s", res.ForUser)
	}
}

func TestFuturesOpenPosition_RejectsSpotOnlyProvider(t *testing.T) {
	tool := NewFuturesOpenPositionTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{
		"provider": "bitkub",
		"symbol":   "BTC/THB",
		"side":     "long",
		"amount":   1.0,
		"leverage": 2.0,
		"confirm":  true,
	})
	if !res.IsError {
		t.Fatalf("Execute should reject spot-only provider, got: %s", res.ForUser)
	}
	if !strings.Contains(res.ForLLM, "supported only for binance and okx") {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
}

func TestFuturesGetPositions_RequiresProvider(t *testing.T) {
	tool := NewFuturesGetPositionsTool(config.DefaultConfig())
	res := tool.Execute(context.Background(), map[string]any{})
	if !res.IsError || !strings.Contains(res.ForLLM, "provider is required") {
		t.Fatalf("unexpected result: %#v", res)
	}
}
