package broker

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// SimulationProvider wraps a real Provider and intercepts all TradingProvider
// calls, returning realistic simulated fills based on live market prices.
// It fully satisfies all 4 broker interfaces — reads are delegated to the
// underlying provider unchanged.
//
// Production code does not construct it directly: TradingProviderFor wraps the
// real provider when config.TradingRiskConfig.PaperTradingMode is on.
type SimulationProvider struct {
	inner Provider
	// orderSeq numbers simulated orders; it is advanced atomically because one
	// provider instance is shared by concurrent tool calls.
	orderSeq atomic.Int64
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

// PaperOrderIDPrefix starts every simulated order ID, so a simulated order can
// never be mistaken for a real exchange ID (and vice versa) in logs, history
// or a follow-up cancel.
const PaperOrderIDPrefix = "paper-"

// CreateOrder simulates an order. Nothing reaches the exchange; only the
// ticker is read.
//
// A market order fills at the price a real one would be valued at: the ask for
// a buy, the bid for a sell, the last price when the ticker has no usable edge
// (resolveOrderPrice, the same rule the order-value cap uses). If no price can
// be determined the order is refused rather than "filled" at zero.
//
// A limit order fills immediately, at its limit price, when it is marketable
// against that same edge; otherwise it is returned "open". Stop and take-profit
// orders are returned "open": there is no trigger engine behind them.
//
// Average, Remaining, Fee and Cost are always set (zero when nothing filled),
// so downstream code that dereferences them behaves as it does for a real fill.
func (s *SimulationProvider) CreateOrder(
	ctx context.Context,
	symbol, orderType, side string,
	amount float64,
	price *float64,
	_ map[string]interface{},
) (ccxt.Order, error) {
	if !isPositiveFinite(amount) {
		return ccxt.Order{}, errors.New("simulation: amount must be a positive, finite number")
	}
	seq := s.orderSeq.Add(1)
	nowMs := processNow().UnixMilli()
	id := fmt.Sprintf("%s%d-%d", PaperOrderIDPrefix, nowMs, seq)

	md, _ := s.inner.(MarketDataProvider)
	marketPx, _, pxErr := resolveOrderPrice(ctx, md, symbol, side, nil)

	var fillPrice float64
	status := "open"
	switch orderType {
	case "market":
		if pxErr != nil {
			return ccxt.Order{}, fmt.Errorf("simulation: cannot price a market order for %s: %w", symbol, pxErr)
		}
		fillPrice, status = marketPx, "closed"
	case "limit":
		if price == nil || !isPositiveFinite(*price) {
			return ccxt.Order{}, errors.New("simulation: a limit order needs a positive, finite price")
		}
		fillPrice = *price
		if pxErr == nil {
			if (side == "buy" && *price >= marketPx) || (side == "sell" && *price <= marketPx) {
				status = "closed"
			}
		}
	default:
		if price != nil {
			fillPrice = *price
		}
	}

	filled, remaining, cost, average := 0.0, amount, 0.0, 0.0
	if status == "closed" {
		filled, remaining, average = amount, 0, fillPrice
		cost = amount * fillPrice
	}
	zeroFee := 0.0

	order := ccxt.Order{
		Id:        &id,
		Symbol:    &symbol,
		Type:      &orderType,
		Side:      &side,
		Amount:    &amount,
		Price:     &fillPrice,
		Average:   &average,
		Filled:    &filled,
		Remaining: &remaining,
		Cost:      &cost,
		Status:    &status,
		Fee:       ccxt.Fee{Cost: &zeroFee},
		Datetime:  ptr(processNow().UTC().Format(time.RFC3339)),
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
	id := fmt.Sprintf("paper-transfer-%d", processNow().UnixMilli())
	return ccxt.TransferEntry{
		Id:          &id,
		Currency:    &asset,
		Amount:      &amount,
		FromAccount: &from,
		ToAccount:   &to,
	}, nil
}

// ptr returns a pointer to v. Duplicated locally to avoid cross-package dep.
func ptr(v string) *string { return &v }
