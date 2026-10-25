package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// lossGuardOrder is a small limit order on the dnfake provider; confirm=false so
// a pass ends at the dry-run message and nothing is placed.
func lossGuardOrder(symbol, side string) map[string]any {
	return map[string]any{
		"provider": "dnfake", "symbol": symbol, "type": "limit", "side": side,
		"amount": 1.0, "price": 10.0, "confirm": false,
	}
}

// TestCreateOrder_DailyLossBlocksBuyNotSpotSell: a tripped limit stops a buy but
// never a spot sell, which only reduces exposure and must stay possible.
func TestCreateOrder_DailyLossBlocksBuyNotSpotSell(t *testing.T) {
	tripLossTracker(t, 100)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	tool := NewCreateOrderTool(riskCfg(true, 100, 0))

	buy := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "buy"))
	if !buy.IsError || !strings.Contains(buy.ForLLM, "daily loss") {
		t.Fatalf("a buy must be refused by a tripped limit, got: %+v", buy)
	}

	sell := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "sell"))
	if sell.IsError || !strings.Contains(sell.ForLLM, "Dry-run: all safety checks passed") {
		t.Fatalf("a spot sell must pass a tripped limit, got: %+v", sell)
	}
	noOrders(t, spot, fut)
}

// TestCreateOrder_DailyLossBlocksContractSell: on a contract symbol a sell can
// open a short, so it is gated like a buy.
func TestCreateOrder_DailyLossBlocksContractSell(t *testing.T) {
	tripLossTracker(t, 100)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	tool := NewCreateOrderTool(riskCfg(true, 100, 0))

	for _, side := range []string{"sell", "SELL"} {
		res := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT:USDT", side))
		if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
			t.Errorf("side=%q: a contract-symbol sell must be refused by a tripped limit, got: %+v", side, res)
		}
	}
	noOrders(t, spot, fut)
}

// TestCreateOrder_DailyLossBlocksMarginSell: a spot-symbol sell that carries
// margin / borrow / leverage params can open or enlarge a short, so it is gated
// like a buy. A plain sell, or one with an execution-only param, still passes
// even with allow_margin=true.
func TestCreateOrder_DailyLossBlocksMarginSell(t *testing.T) {
	tripLossTracker(t, 100)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	cfg := riskCfg(true, 100, 0)
	cfg.TradingRisk.AllowMargin = true
	tool := NewCreateOrderTool(cfg)

	for _, params := range []map[string]any{
		{"tdMode": "cross"},
		{"sideEffectType": "MARGIN_BUY"},
		{"autoBorrow": true},
		{"leverage": 3},
		{"posSide": "short"},
		{"someBorrowFlag": true},
	} {
		args := lossGuardOrder("BTC/USDT", "sell")
		args["params"] = params
		res := tool.Execute(context.Background(), args)
		if !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
			t.Errorf("params %v: a margin-capable sell must be refused by a tripped limit, got: %+v", params, res)
		}
	}

	plain := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "sell"))
	if plain.IsError {
		t.Errorf("a plain spot sell with allow_margin=true must still pass, got: %+v", plain)
	}
	args := lossGuardOrder("BTC/USDT", "sell")
	args["params"] = map[string]any{"clientOrderId": "abc"}
	if res := tool.Execute(context.Background(), args); res.IsError {
		t.Errorf("an execution-only param must not gate a spot sell, got: %+v", res)
	}
	noOrders(t, spot, fut)
}

// TestCreateOrder_EquityUnknown_FailsClosed: with the gateway's equity guard
// armed, a tracked account with no sample cannot open risk, a spot sell still
// can, and the first sample lifts the refusal.
func TestCreateOrder_EquityUnknown_FailsClosed(t *testing.T) {
	tr := swapLossTracker(t)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	cfg := riskCfg(true, 100, 0)
	key := broker.AccountKey(cfg, "dnfake", "")
	tr.EnableEquityGuard([]string{key}, 15*time.Minute)
	tool := NewCreateOrderTool(cfg)

	buy := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "buy"))
	if !buy.IsError || !strings.Contains(buy.ForLLM, "equity unknown") {
		t.Fatalf("a buy on an unsampled tracked account must fail closed, got: %+v", buy)
	}
	if sell := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "sell")); sell.IsError {
		t.Errorf("a spot sell is never gated, got: %+v", sell)
	}

	tr.SampleAccount(key, 1000, time.Now())
	if buy := tool.Execute(context.Background(), lossGuardOrder("BTC/USDT", "buy")); strings.Contains(buy.ForLLM, "equity unknown") || strings.Contains(buy.ForLLM, "daily loss") {
		t.Errorf("once the account is sampled the loss gate must pass, got: %+v", buy)
	}
	noOrders(t, spot, fut)
}

// TestDailyLossGuard_Acceptance walks the whole story with daily_loss_limit_usd
// = 50: a 60 USD equity drop blocks create_order and futures_open_position, the
// block survives a restart, and a reset written to the state file (what
// `khunquant risk reset-daily-loss` does) lifts it on the next check.
func TestDailyLossGuard_Acceptance(t *testing.T) {
	statePath := broker.LossStatePath(filepath.Join(t.TempDir(), "workspace"))
	tr := swapLossTracker(t)
	tr.SetStorePath(statePath)
	spot, fut := newDNFakes()
	installDNFakes(t, spot, fut)
	cfg := riskCfg(true, 50, 0)
	key := broker.AccountKey(cfg, "dnfake", "")
	tr.EnableEquityGuard([]string{key}, time.Hour)

	now := time.Now()
	tr.SampleAccount(key, 1000, now)
	tr.SampleAccount(key, 940, now.Add(time.Second)) // simulated 60 USD equity drop

	marketBuy := map[string]any{"provider": "dnfake", "symbol": "BTC/USDT", "type": "market", "side": "buy", "amount": 1.0, "confirm": true}
	futOpen := map[string]any{
		"provider": "dnfake", "symbol": "BTC/USDT:USDT", "side": "long",
		"amount": 1.0, "leverage": 3.0, "confirm": true,
	}
	assertBlocked := func(stage string) {
		t.Helper()
		if res := NewCreateOrderTool(cfg).Execute(context.Background(), marketBuy); !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
			t.Fatalf("%s: create_order market buy must be blocked, got: %+v", stage, res)
		}
		if res := NewFuturesOpenPositionTool(cfg).Execute(context.Background(), futOpen); !res.IsError || !strings.Contains(res.ForLLM, "daily loss") {
			t.Fatalf("%s: futures_open_position must be blocked, got: %+v", stage, res)
		}
	}
	assertBlocked("after the drop")
	noOrders(t, spot, fut)

	// A restart: a fresh tracker on the same state file.
	restarted := &broker.DailyLossTracker{}
	restarted.SetStorePath(statePath)
	broker.GlobalLossTracker = restarted // swapLossTracker's cleanup restores the original
	restarted.EnableEquityGuard([]string{key}, time.Hour)
	assertBlocked("after a restart")

	// The CLI path: a separate tracker resets the file; the running one sees it.
	cli := &broker.DailyLossTracker{}
	cli.SetStorePath(statePath)
	if err := cli.Reset("withdrawal"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := restarted.CheckDailyLossFor(50, key); err != nil {
		t.Fatalf("the reset must lift the block on the next check, got: %v", err)
	}
	if res := NewFuturesOpenPositionTool(cfg).Execute(context.Background(), futOpen); res.IsError && strings.Contains(res.ForLLM, "daily loss") {
		t.Fatalf("futures_open_position still blocked after the reset: %+v", res)
	}
}
