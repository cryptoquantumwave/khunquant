package settrade

// SettradeFullAdapter is the planned full broker adapter for Phase 5.
// It will implement:
//   - broker.PortfolioProvider  (cash balance + stock holdings)
//   - broker.MarketDataProvider (live prices via MQTT WebSocket, daily OHLCV via REST)
//   - broker.TradingProvider    (limit/ATO/ATC orders with SET lot-size & tick-size enforcement)
//
// TransferProvider is NOT planned — Settrade does not expose inter-account transfers.
//
// Auth: ECDSA P-256 login (POST /api/oam/v1/{broker_id}/broker-apps/ALGO/login)
// Real-time: MQTT over WebSocket (paho.mqtt.golang)
//
// TODO(phase5): implement full adapter; delete this stub file.

import (
	"context"
	"fmt"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// SettradeFullAdapter wraps SettradeExchange stub.
// Replace the stub embedding with real HTTP + MQTT client in Phase 5.
type SettradeFullAdapter struct {
	*SettradeExchange
}

func newFullAdapter(cfg SettradeAccount) (*SettradeFullAdapter, error) {
	ex, err := NewSettradeExchange(cfg)
	if err != nil {
		return nil, err
	}
	return &SettradeFullAdapter{SettradeExchange: ex}, nil
}

// ---- broker.Provider ----

func (a *SettradeFullAdapter) ID() string              { return Name }
func (a *SettradeFullAdapter) Category() broker.AssetCategory { return broker.CategoryStock }
func (a *SettradeFullAdapter) GetMarketStatus(ctx context.Context, symbol string) (broker.MarketStatus, error) {
	return broker.MarketUnknown, ErrNotImplemented
}

// ---- broker.PortfolioProvider ----

func (a *SettradeFullAdapter) GetBalances(_ context.Context) ([]broker.Balance, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/portSummary → cash balance
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) GetWalletBalances(_ context.Context, _ string) ([]broker.WalletBalance, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/portfolio → stock holdings
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchPrice(_ context.Context, asset, quote string) (float64, error) {
	// TODO(phase5): subscribe MQTT topic for {symbol} → last traded price
	// MQTT topic pattern: /market/{symbol}/price
	return 0, ErrNotImplemented
}

func (a *SettradeFullAdapter) SupportedWalletTypes() []string {
	return []string{"stock", "cash"}
}

// ---- broker.MarketDataProvider ----

func (a *SettradeFullAdapter) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	// TODO(phase5): subscribe MQTT WebSocket topic for {symbol} bid/ask/last
	// MQTT broker: wss://open-api.settrade.com/market-data/websocket
	// Topic: /market/{symbol}/quote
	return ccxt.Ticker{}, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	// TODO(phase5): subscribe MQTT for each symbol concurrently then aggregate
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchOHLCV(_ context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	// TODO(phase5): call get_candlestick(symbol, interval, limit) via REST
	// Settrade supports intraday candles down to 1 minute (1m, 5m, 15m, 1h, 1d, etc.)
	// REST path: /api/eq/v1/{broker_id}/market/historical-price/{symbol}?interval={tf}&limit={n}
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchOrderBook(_ context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	// TODO(phase5): subscribe MQTT topic for /market/{symbol}/orderbook → bid/ask queues
	return ccxt.OrderBook{}, ErrNotImplemented
}

func (a *SettradeFullAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/market/symbols — full SET symbol catalogue
	// LotSize = 100 (round lot), TickSize varies by price tier (see SET tick table)
	return nil, ErrNotImplemented
}

// ---- broker.TradingProvider ----

func (a *SettradeFullAdapter) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	// TODO(phase5):
	// 1. POST /api/um/v1/{broker_id}/user/verify-pin  (required before placing orders)
	// 2. POST /api/eq/v1/{broker_id}/accounts/{account_no}/orders
	//    Body: { symbol, side: "BUY"/"SELL", orderType: "LMT"/"ATO"/"ATC", qty, price }
	// Enforce SET lot size: amount must be multiple of 100 (round lot).
	// Enforce tick size per price tier before sending price to API.
	return ccxt.Order{}, ErrNotImplemented
}

func (a *SettradeFullAdapter) CancelOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	// TODO(phase5): DELETE /api/eq/v1/{broker_id}/accounts/{account_no}/orders/{order_id}
	return ccxt.Order{}, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/orders/{order_id}
	return ccxt.Order{}, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchOpenOrders(_ context.Context, symbol string) ([]ccxt.Order, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/orders?status=pending
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchClosedOrders(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/orders?status=matched
	return nil, ErrNotImplemented
}

func (a *SettradeFullAdapter) FetchMyTrades(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	// TODO(phase5): GET /api/eq/v1/{broker_id}/accounts/{account_no}/trades
	return nil, ErrNotImplemented
}

// ---- init ----

func init() {
	// TODO(phase5): un-comment when real implementation is ready.
	// broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
	// 	acc, ok := cfg.Exchanges.Settrade.ResolveAccount(accountName)
	// 	if !ok { return nil, fmt.Errorf("settrade: account %q not found", accountName) }
	// 	return newFullAdapter(acc)
	// })
	_ = fmt.Sprintf // suppress unused import until Phase 5
	_ = config.ExchangeAccount{}
}
