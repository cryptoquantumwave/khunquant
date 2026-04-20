package broker_test

import (
	"context"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func setupSimProvider(lastPrice float64) *broker.SimulationProvider {
	mp := NewMockProvider("binance")
	mp.FetchTickerFn = func(symbol string) (ccxt.Ticker, error) {
		return ccxt.Ticker{Symbol: &symbol, Last: &lastPrice}, nil
	}
	return broker.NewSimulationProvider(mp)
}

func TestSimulationProvider_ID(t *testing.T) {
	sim := setupSimProvider(50000)
	if !strings.HasSuffix(sim.ID(), ":sim") {
		t.Errorf("expected ID to end with :sim, got %q", sim.ID())
	}
}

func TestSimulationProvider_MarketOrder_FillsAtLastPrice(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	order, err := sim.CreateOrder(ctx, "BTC/USDT", "market", "buy", 1, nil, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("market order: expected status %q, got %v", "closed", order.Status)
	}
	if order.Price == nil || *order.Price != lastPrice {
		t.Errorf("market order: expected fill price %f, got %v", lastPrice, order.Price)
	}
	if order.Filled == nil || *order.Filled != 1.0 {
		t.Errorf("market order: expected filled=1, got %v", order.Filled)
	}
}

func TestSimulationProvider_LimitBuy_MarketableFills(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit buy at price >= market → should fill.
	limitPrice := 51000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "buy", 0.5, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("expected closed for marketable limit buy, got %v", order.Status)
	}
}

func TestSimulationProvider_LimitBuy_NotMarketableStaysOpen(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit buy at price < market → should not fill.
	limitPrice := 49000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "buy", 0.5, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "open" {
		t.Errorf("expected open for non-marketable limit buy, got %v", order.Status)
	}
}

func TestSimulationProvider_LimitSell_MarketableFills(t *testing.T) {
	lastPrice := 50000.0
	sim := setupSimProvider(lastPrice)
	ctx := context.Background()

	// Limit sell at price <= market → should fill.
	limitPrice := 49000.0
	order, err := sim.CreateOrder(ctx, "BTC/USDT", "limit", "sell", 1, &limitPrice, nil)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "closed" {
		t.Errorf("expected closed for marketable limit sell, got %v", order.Status)
	}
}

func TestSimulationProvider_CancelOrder(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	order, err := sim.CancelOrder(ctx, "order-123", "BTC/USDT")
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if order.Status == nil || *order.Status != "canceled" {
		t.Errorf("expected status canceled, got %v", order.Status)
	}
	if order.Id == nil || *order.Id != "order-123" {
		t.Errorf("expected id order-123, got %v", order.Id)
	}
}

func TestSimulationProvider_Transfer(t *testing.T) {
	sim := setupSimProvider(50000)
	ctx := context.Background()

	entry, err := sim.Transfer(ctx, "USDT", 1000, "spot", "futures")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if entry.Currency == nil || *entry.Currency != "USDT" {
		t.Errorf("expected currency USDT, got %v", entry.Currency)
	}
	if entry.Amount == nil || *entry.Amount != 1000 {
		t.Errorf("expected amount 1000, got %v", entry.Amount)
	}
}

func TestSimulationProvider_Category(t *testing.T) {
	sim := setupSimProvider(50000)
	if sim.Category() != broker.CategoryCrypto {
		t.Errorf("expected CategoryCrypto, got %v", sim.Category())
	}
}
