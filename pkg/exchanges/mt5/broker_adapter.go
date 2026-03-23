package mt5

// MT5FullAdapter is the planned full broker adapter for Phase 6.
// It will implement:
//   - broker.PortfolioProvider  (account equity, balance, open positions)
//   - broker.MarketDataProvider (real-time quotes, OHLCV candles, symbol catalogue)
//   - broker.TradingProvider    (market/limit/stop orders, lot-size handling)
//
// TransferProvider is NOT planned — MT5 does not expose inter-account fund transfers.
//
// Integration path: MetaApi Cloud REST API (https://metaapi.cloud/docs/client/)
//
// TODO(phase6): implement full adapter; delete this stub file.

import (
	"context"
	"fmt"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// MT5FullAdapter wraps MT5Exchange stub.
type MT5FullAdapter struct {
	*MT5Exchange
}

func newFullAdapter(cfg MT5Account) (*MT5FullAdapter, error) {
	ex, err := NewMT5Exchange(cfg)
	if err != nil {
		return nil, err
	}
	return &MT5FullAdapter{MT5Exchange: ex}, nil
}

// ---- broker.Provider ----

func (a *MT5FullAdapter) ID() string              { return Name }
func (a *MT5FullAdapter) Category() broker.AssetCategory { return broker.CategoryFX }
func (a *MT5FullAdapter) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	// TODO(phase6): implement FX session calendar (24/5 for majors, specific hours for metals/indices)
	return broker.MarketUnknown, ErrNotImplemented
}

// ---- broker.PortfolioProvider ----

func (a *MT5FullAdapter) GetBalances(_ context.Context) ([]broker.Balance, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/account-information
	// Map equity, balance, margin fields to broker.Balance
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) GetWalletBalances(_ context.Context, walletType string) ([]broker.WalletBalance, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/positions
	// Map open positions to WalletBalance{WalletType:"positions"}
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchPrice(_ context.Context, asset, quote string) (float64, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/symbols/{symbol}/current-price
	// MT5 symbol: asset+quote (e.g. FetchPrice("EUR","USD") → "EURUSD")
	return 0, ErrNotImplemented
}

func (a *MT5FullAdapter) SupportedWalletTypes() []string {
	return []string{"balance", "positions", "margin"}
}

// ---- broker.MarketDataProvider ----

func (a *MT5FullAdapter) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/symbols/{symbol}/current-price
	return ccxt.Ticker{}, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	// TODO(phase6): loop FetchTicker calls (MetaApi has no batch endpoint)
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchOHLCV(_ context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/symbols/{symbol}/candles/{timeframe}?limit={n}
	// MetaApi timeframe map: 1m→"1m", 5m→"5m", 1h→"1h", 1d→"1d", etc.
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchOrderBook(_ context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/symbols/{symbol}/current-price
	// MT5/MetaApi does not provide full order book; return best bid/ask only.
	return ccxt.OrderBook{}, ErrNotImplemented
}

func (a *MT5FullAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/symbols — full symbol catalogue
	// Map MT5 contract specs to broker.Market (lotSize, tickSize, category=fx/index/metal)
	return nil, ErrNotImplemented
}

// ---- broker.TradingProvider ----

func (a *MT5FullAdapter) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	// TODO(phase6): POST /users/current/accounts/{id}/trade
	// Convert amount (units) to lots: lots = amount / contractSize
	// Enforce TradingRiskConfig.AllowLeverage check before executing
	// Supported actionTypes: ORDER_TYPE_BUY, ORDER_TYPE_SELL, ORDER_TYPE_BUY_LIMIT, etc.
	return ccxt.Order{}, ErrNotImplemented
}

func (a *MT5FullAdapter) CancelOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	// TODO(phase6): POST /users/current/accounts/{id}/trade with actionType=ORDER_CANCEL
	return ccxt.Order{}, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/history-orders/ticket/{ticket}
	return ccxt.Order{}, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchOpenOrders(_ context.Context, symbol string) ([]ccxt.Order, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/orders
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchClosedOrders(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/history-orders/time/{from}/{to}
	return nil, ErrNotImplemented
}

func (a *MT5FullAdapter) FetchMyTrades(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	// TODO(phase6): GET /users/current/accounts/{id}/history-deals/time/{from}/{to}
	return nil, ErrNotImplemented
}

// ---- init ----

func init() {
	// TODO(phase6): un-comment when real implementation is ready.
	// broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
	// 	acc, ok := cfg.Exchanges.MT5.ResolveAccount(accountName)
	// 	if !ok { return nil, fmt.Errorf("mt5: account %q not found", accountName) }
	// 	return newFullAdapter(acc)
	// })
	_ = fmt.Sprintf // suppress unused import until Phase 6
	_ = config.ExchangeAccount{}
}
