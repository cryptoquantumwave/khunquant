package broker

import (
	"context"
	"fmt"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// SimulationProvider wraps a real Provider and intercepts all TradingProvider
// calls, returning realistic simulated fills based on live market prices.
// It fully satisfies all 4 broker interfaces — reads are delegated to the
// underlying provider unchanged.
//
// Enable globally via config.TradingRiskConfig.PaperTradingMode = true,
// or per-tool by wrapping any provider:
//
//	sim := broker.NewSimulationProvider(realProvider)
type SimulationProvider struct {
	inner    Provider
	orderSeq int64
}

// NewSimulationProvider wraps inner so that order-mutating calls are simulated.
// inner must implement at least PortfolioProvider + MarketDataProvider.
func NewSimulationProvider(inner Provider) *SimulationProvider {
	return &SimulationProvider{inner: inner}
}

// --- Provider ---

func (s *SimulationProvider) ID() string              { return s.inner.ID() + ":sim" }
func (s *SimulationProvider) Category() AssetCategory { return s.inner.Category() }
func (s *SimulationProvider) GetMarketStatus(ctx context.Context, symbol string) (MarketStatus, error) {
	return s.inner.GetMarketStatus(ctx, symbol)
}

// --- PortfolioProvider (delegated) ---

func (s *SimulationProvider) GetBalances(ctx context.Context) ([]Balance, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support portfolio data")
	}
	return pp.GetBalances(ctx)
}

func (s *SimulationProvider) GetWalletBalances(ctx context.Context, walletType string) ([]WalletBalance, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support portfolio data")
	}
	return pp.GetWalletBalances(ctx, walletType)
}

func (s *SimulationProvider) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return 0, fmt.Errorf("simulation: inner provider does not support price fetching")
	}
	return pp.FetchPrice(ctx, asset, quote)
}

func (s *SimulationProvider) SupportedWalletTypes() []string {
	pp, ok := s.inner.(PortfolioProvider)
	if !ok {
		return nil
	}
	return pp.SupportedWalletTypes()
}

// --- MarketDataProvider (delegated) ---

func (s *SimulationProvider) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return ccxt.Ticker{}, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchTicker(ctx, symbol)
}

func (s *SimulationProvider) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchTickers(ctx, symbols)
}

func (s *SimulationProvider) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchOHLCV(ctx, symbol, timeframe, since, limit)
}

func (s *SimulationProvider) FetchOrderBook(ctx context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return ccxt.OrderBook{}, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.FetchOrderBook(ctx, symbol, depth)
}

func (s *SimulationProvider) LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	md, ok := s.inner.(MarketDataProvider)
	if !ok {
		return nil, fmt.Errorf("simulation: inner provider does not support market data")
	}
	return md.LoadMarkets(ctx)
}

// --- TradingProvider (simulated) ---

// CreateOrder simulates an order fill using the live ticker price.
// Market orders fill immediately at last price.
// Limit orders fill immediately if price is marketable (buy <= ask, sell >= bid),
// otherwise they are returned as "open" (partial simulation).
func (s *SimulationProvider) CreateOrder(
	ctx context.Context,
	symbol, orderType, side string,
	amount float64,
	price *float64,
	_ map[string]interface{},
) (ccxt.Order, error) {
	s.orderSeq++
	id := fmt.Sprintf("sim-%d-%d", time.Now().UnixMilli(), s.orderSeq)
	nowMs := time.Now().UnixMilli()

	var fillPrice float64
	status := "open"

	// Fetch live price for realistic simulation
	if md, ok := s.inner.(MarketDataProvider); ok {
		if ticker, err := md.FetchTicker(ctx, symbol); err == nil {
			if ticker.Last != nil {
				fillPrice = *ticker.Last
			}
			// Market orders always fill at last price
			if orderType == "market" {
				status = "closed"
			}
			// Limit orders: fill if marketable
			if orderType == "limit" && price != nil {
				if side == "buy" && *price >= fillPrice {
					status = "closed"
					fillPrice = *price
				} else if side == "sell" && *price <= fillPrice {
					status = "closed"
					fillPrice = *price
				}
			}
		}
	}
	if fillPrice == 0 && price != nil {
		fillPrice = *price
	}

	filled := 0.0
	cost := 0.0
	if status == "closed" {
		filled = amount
		cost = amount * fillPrice
	}

	order := ccxt.Order{
		Id:       &id,
		Symbol:   &symbol,
		Type:     &orderType,
		Side:     &side,
		Amount:   &amount,
		Price:    &fillPrice,
		Filled:   &filled,
		Cost:     &cost,
		Status:   &status,
		Datetime:  ptr(time.Now().UTC().Format(time.RFC3339)),
		Timestamp: &nowMs,
	}
	return order, nil
}

func (s *SimulationProvider) CancelOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	cancelled := "canceled"
	zero := 0.0
	return ccxt.Order{
		Id:     &id,
		Symbol: &symbol,
		Status: &cancelled,
		Filled: &zero,
	}, nil
}

func (s *SimulationProvider) FetchOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	status := "open"
	zero := 0.0
	return ccxt.Order{
		Id:     &id,
		Symbol: &symbol,
		Status: &status,
		Filled: &zero,
	}, nil
}

func (s *SimulationProvider) FetchOpenOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	return []ccxt.Order{}, nil
}

func (s *SimulationProvider) FetchClosedOrders(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.Order, error) {
	return []ccxt.Order{}, nil
}

func (s *SimulationProvider) FetchMyTrades(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.Trade, error) {
	return []ccxt.Trade{}, nil
}

// --- TransferProvider (simulated) ---

func (s *SimulationProvider) Transfer(_ context.Context, asset string, amount float64, from, to string) (ccxt.TransferEntry, error) {
	id := fmt.Sprintf("sim-transfer-%d", time.Now().UnixMilli())
	return ccxt.TransferEntry{
		Id:       &id,
		Currency: &asset,
		Amount:   &amount,
		FromAccount: &from,
		ToAccount:   &to,
	}, nil
}

// ptr returns a pointer to v. Duplicated locally to avoid cross-package dep.
func ptr(v string) *string { return &v }
