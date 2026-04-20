// Package mt5 provides a broker adapter stub for MetaTrader 5 (MT5) Forex/CFD trading.
//
// # Status: STUB — not yet implemented
//
// This package defines the structure and interface contracts for a future MT5
// integration. All methods return ErrNotImplemented until the adapter is
// built (Phase 6).
//
// # Connectivity: MetaApi Cloud (recommended)
//
// MT5 does not expose a native HTTP REST API. The recommended Go integration
// path is MetaApi Cloud — a managed gateway that works with any MT5 broker.
//
//	Docs:     https://metaapi.cloud/docs/client/
//	REST:     https://mt-client-api-v1.new-york.agiliumtrade.ai   (US East)
//	          https://mt-client-api-v1.london.agiliumtrade.ai      (EU)
//	Auth:     Static header  auth-token: <API_TOKEN>
//	          Token is generated once from the MetaApi web dashboard.
//	Cost:     Free tier (1 MT5 account, production use)
//	Setup:    Register at metaapi.cloud, add MT5 login/password/server.
//
//	Key REST endpoints (MetaApi v1):
//	  GET  /users/current/accounts/{id}/account-information         — balance, equity, margin
//	  GET  /users/current/accounts/{id}/positions                   — open positions
//	  GET  /users/current/accounts/{id}/orders                      — pending orders
//	  POST /users/current/accounts/{id}/trade                       — place/cancel order
//	  GET  /users/current/accounts/{id}/history-orders/time/{f}/{t} — order history
//	  GET  /users/current/accounts/{id}/history-deals/time/{f}/{t}  — trade history
//	  GET  /users/current/accounts/{id}/symbols/{symbol}/current-price — live bid/ask
//	  GET  /users/current/accounts/{id}/symbols/{symbol}/current-candles/{tf} — OHLCV
//
//	Place order body:
//	  { "actionType": "ORDER_TYPE_BUY", "symbol": "EURUSD", "volume": 0.1,
//	    "price": 1.12345, "stopLoss": 1.1200, "takeProfit": 1.1300 }
//
// NOTE: The community Go SDK (github.com/metaapi/metaapi-go-client) is
// unmaintained. Use plain net/http REST calls instead.
//
// # MT5-Specific Constraints
//
//   - Symbols: broker-specific (e.g. "EURUSD", "XAUUSD", "US30.cash", "GER40")
//   - Volume: MetaApi uses lots (1 lot = 100,000 units for FX majors; varies for CFDs)
//   - Tool layer converts units ↔ lots using contractSize from symbol spec
//   - Leverage: 1:30 (retail EU/TH) to 1:500 (offshore); TradingRiskConfig.AllowLeverage must be true
//   - Order types: ORDER_TYPE_BUY, ORDER_TYPE_SELL, ORDER_TYPE_BUY_LIMIT, ORDER_TYPE_SELL_LIMIT,
//     ORDER_TYPE_BUY_STOP, ORDER_TYPE_SELL_STOP, ORDER_CANCEL, POSITION_CLOSE_ID, POSITION_MODIFY
//   - Position model: netting (default, 1 position per symbol) or hedging (multiple)
//   - Account currency: single deposit currency (USD, EUR, THB depending on broker)
//   - Market hours: 24/5 for FX majors (Mon 00:00 – Fri 22:00 UTC); varies for metals/indices
//   - OHLCV timeframes: 1m 2m 3m 4m 5m 6m 10m 12m 15m 20m 30m 1h 2h 3h 4h 6h 8h 12h 1d 1w 1mn
//
// # MetaApi Rate Limits
//
//	Per application: 1,000 credits/s, 6,000/min, 18,000/hr
//	Per account:     5,000 credits per 10 seconds
//	Credit costs:    trading ops = 10, account/position reads = 50, symbol list = 500
//
// # TODO (Phase 6)
//
//  1. Register MetaApi account; add MT5 broker credentials (login, password, server)
//  2. Implement HTTP client with auth-token header for all MetaApi REST calls
//  3. Implement PortfolioProvider: GetBalances (equity/balance/margin from account-information)
//  4. Implement MarketDataProvider: FetchTicker (current-price), FetchOHLCV (current-candles)
//  5. Implement TradingProvider: CreateOrder (lots conversion), CancelOrder, FetchOpenOrders
//  6. Implement FX session calendar for GetMarketStatus
//  7. Add MT5ExchangeConfig to pkg/config/config.go
//  8. Guard leveraged order types behind TradingRiskConfig.AllowLeverage check
package mt5

import (
	"context"
	"errors"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// Name is the canonical provider identifier.
const Name = "mt5"

// ErrNotImplemented is returned by all stub methods.
var ErrNotImplemented = errors.New("mt5: adapter not yet implemented (Phase 6)")

// MT5Exchange is a placeholder that satisfies exchanges.Exchange.
// Replace with real implementation in Phase 6.
//
// TODO(phase6): replace stub with MetaApi client or broker REST client.
type MT5Exchange struct {
	cfg MT5Account
}

// NewMT5Exchange creates a stub exchange.
func NewMT5Exchange(cfg MT5Account) (*MT5Exchange, error) {
	return &MT5Exchange{cfg: cfg}, nil
}

func (e *MT5Exchange) Name() string { return Name }

func (e *MT5Exchange) GetBalances(_ context.Context) ([]exchanges.Balance, error) {
	return nil, ErrNotImplemented
}

func (e *MT5Exchange) FetchPrice(_ context.Context, _, _ string) (float64, error) {
	return 0, ErrNotImplemented
}

// MT5Account holds connection details for a single MT5 account via MetaApi Cloud.
//
// TODO(phase6): add to pkg/config/config.go under ExchangesConfig.
type MT5Account struct {
	Name string `json:"name,omitempty"`

	// MetaAPIToken is the static API token from https://app.metaapi.cloud.
	// Header: auth-token: <MetaAPIToken>
	MetaAPIToken string `json:"metaapi_token"`

	// MetaAPIAccountID is the MetaApi account UUID (not the broker login number).
	// Found in the MetaApi dashboard after adding your MT5 account.
	MetaAPIAccountID string `json:"metaapi_account_id"`

	// Region selects the MetaApi endpoint region: "new-york" (default) or "london".
	// Default: new-york (lower latency for Asia/Pacific via US East)
	Region string `json:"region,omitempty"`

	// MT5 login credentials — used when registering the account with MetaApi.
	// Not used directly in API calls once registered.
	MT5Login    string `json:"mt5_login,omitempty"`
	MT5Password string `json:"mt5_password,omitempty"`
	MT5Server   string `json:"mt5_server,omitempty"` // e.g. "ICMarketsSC-Demo02"
}

// MT5ExchangeConfig is the top-level config block for MT5.
//
// TODO(phase6): add as a field to pkg/config.ExchangesConfig.
type MT5ExchangeConfig struct {
	Enabled  bool         `json:"enabled"`
	Sandbox  bool         `json:"sandbox"` // use MetaApi demo environment
	Accounts []MT5Account `json:"accounts,omitempty"`
}

// SettradeMarketDataProvider is a placeholder for the MarketDataProvider category marker.
type MT5MarketDataProvider struct {
	*MT5Exchange
}

func (p *MT5MarketDataProvider) ID() string                     { return Name }
func (p *MT5MarketDataProvider) Category() broker.AssetCategory { return broker.CategoryFX }
func (p *MT5MarketDataProvider) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	// TODO(phase6): FX is open 24/5 (Mon 00:00 UTC – Fri 22:00 UTC)
	// Indices/metals have specific session hours
	return broker.MarketUnknown, ErrNotImplemented
}

// Ensure MT5Exchange satisfies exchanges.Exchange at compile time.
var _ exchanges.Exchange = (*MT5Exchange)(nil)

// Ensure MT5MarketDataProvider partially satisfies broker.Provider at compile time.
var _ broker.Provider = (*MT5MarketDataProvider)(nil)

// suppress unused import
var _ = config.ExchangeAccount{}
