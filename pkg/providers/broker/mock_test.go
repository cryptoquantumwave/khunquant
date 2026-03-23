package broker_test

import (
	"context"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// MockProvider implements all four broker interfaces for testing.
type MockProvider struct {
	id       string
	category broker.AssetCategory

	// Configurable responses
	MarketStatusFn     func(symbol string) (broker.MarketStatus, error)
	GetBalancesFn      func() ([]broker.Balance, error)
	GetWalletBalsFn    func(walletType string) ([]broker.WalletBalance, error)
	FetchPriceFn       func(asset, quote string) (float64, error)
	FetchTickerFn      func(symbol string) (ccxt.Ticker, error)
	FetchTickersFn     func(symbols []string) (map[string]ccxt.Ticker, error)
	FetchOHLCVFn       func(symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error)
	FetchOrderBookFn   func(symbol string, depth int) (ccxt.OrderBook, error)
	LoadMarketsFn      func() (map[string]ccxt.MarketInterface, error)
	CreateOrderFn      func(symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error)
	CancelOrderFn      func(id, symbol string) (ccxt.Order, error)
	FetchOrderFn       func(id, symbol string) (ccxt.Order, error)
	FetchOpenOrdersFn  func(symbol string) ([]ccxt.Order, error)
	FetchClosedOrdersFn func(symbol string, since *int64, limit int) ([]ccxt.Order, error)
	FetchMyTradesFn    func(symbol string, since *int64, limit int) ([]ccxt.Trade, error)
	TransferFn         func(asset string, amount float64, fromAccount, toAccount string) (ccxt.TransferEntry, error)
}

func NewMockProvider(id string) *MockProvider {
	return &MockProvider{id: id, category: broker.CategoryCrypto}
}

// --- broker.Provider ---
func (m *MockProvider) ID() string                    { return m.id }
func (m *MockProvider) Category() broker.AssetCategory { return m.category }
func (m *MockProvider) GetMarketStatus(_ context.Context, symbol string) (broker.MarketStatus, error) {
	if m.MarketStatusFn != nil {
		return m.MarketStatusFn(symbol)
	}
	return broker.MarketOpen, nil
}

// --- broker.PortfolioProvider ---
func (m *MockProvider) GetBalances(_ context.Context) ([]broker.Balance, error) {
	if m.GetBalancesFn != nil {
		return m.GetBalancesFn()
	}
	return nil, nil
}
func (m *MockProvider) GetWalletBalances(_ context.Context, walletType string) ([]broker.WalletBalance, error) {
	if m.GetWalletBalsFn != nil {
		return m.GetWalletBalsFn(walletType)
	}
	return nil, nil
}
func (m *MockProvider) FetchPrice(_ context.Context, asset, quote string) (float64, error) {
	if m.FetchPriceFn != nil {
		return m.FetchPriceFn(asset, quote)
	}
	return 1.0, nil
}
func (m *MockProvider) SupportedWalletTypes() []string { return []string{"spot", "all"} }

// --- broker.MarketDataProvider ---
func (m *MockProvider) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	if m.FetchTickerFn != nil {
		return m.FetchTickerFn(symbol)
	}
	last := 50000.0
	return ccxt.Ticker{Symbol: &symbol, Last: &last}, nil
}
func (m *MockProvider) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	if m.FetchTickersFn != nil {
		return m.FetchTickersFn(symbols)
	}
	return map[string]ccxt.Ticker{}, nil
}
func (m *MockProvider) FetchOHLCV(_ context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if m.FetchOHLCVFn != nil {
		return m.FetchOHLCVFn(symbol, timeframe, since, limit)
	}
	return nil, nil
}
func (m *MockProvider) FetchOrderBook(_ context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	if m.FetchOrderBookFn != nil {
		return m.FetchOrderBookFn(symbol, depth)
	}
	return ccxt.OrderBook{}, nil
}
func (m *MockProvider) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	if m.LoadMarketsFn != nil {
		return m.LoadMarketsFn()
	}
	return nil, nil
}

// --- broker.TradingProvider ---
func (m *MockProvider) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	if m.CreateOrderFn != nil {
		return m.CreateOrderFn(symbol, orderType, side, amount, price, params)
	}
	id := "mock-order-1"
	return ccxt.Order{Id: &id}, nil
}
func (m *MockProvider) CancelOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	if m.CancelOrderFn != nil {
		return m.CancelOrderFn(id, symbol)
	}
	return ccxt.Order{Id: &id}, nil
}
func (m *MockProvider) FetchOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	if m.FetchOrderFn != nil {
		return m.FetchOrderFn(id, symbol)
	}
	return ccxt.Order{Id: &id}, nil
}
func (m *MockProvider) FetchOpenOrders(_ context.Context, symbol string) ([]ccxt.Order, error) {
	if m.FetchOpenOrdersFn != nil {
		return m.FetchOpenOrdersFn(symbol)
	}
	return nil, nil
}
func (m *MockProvider) FetchClosedOrders(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	if m.FetchClosedOrdersFn != nil {
		return m.FetchClosedOrdersFn(symbol, since, limit)
	}
	return nil, nil
}
func (m *MockProvider) FetchMyTrades(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	if m.FetchMyTradesFn != nil {
		return m.FetchMyTradesFn(symbol, since, limit)
	}
	return nil, nil
}

// --- broker.TransferProvider ---
func (m *MockProvider) Transfer(_ context.Context, asset string, amount float64, fromAccount, toAccount string) (ccxt.TransferEntry, error) {
	if m.TransferFn != nil {
		return m.TransferFn(asset, amount, fromAccount, toAccount)
	}
	return ccxt.TransferEntry{}, nil
}
