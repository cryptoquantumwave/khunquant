// Package broker defines the unified provider hierarchy for all asset classes.
// It mirrors the pkg/exchanges interface hierarchy but extends it with
// market-data, trading, and transfer capabilities.
package broker

import (
	"context"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// AssetCategory identifies the class of instruments a provider handles.
type AssetCategory string

const (
	CategoryCrypto AssetCategory = "crypto"
	CategoryStock  AssetCategory = "stock"
	CategoryFX     AssetCategory = "fx"
)

// MarketStatus indicates whether a market is currently tradeable.
type MarketStatus string

const (
	MarketOpen    MarketStatus = "open"
	MarketClosed  MarketStatus = "closed"
	MarketUnknown MarketStatus = "unknown"
)

// AccountRef is a resolved (provider-id, account-name) pair returned by
// ListConfiguredAccounts.
type AccountRef struct {
	ProviderID string
	Account    string
}

// Provider is the root interface every broker provider must satisfy.
type Provider interface {
	// ID returns the canonical provider identifier (e.g. "binance", "okx").
	ID() string

	// Category returns the asset class this provider covers.
	Category() AssetCategory

	// GetMarketStatus returns whether the given symbol is currently open for trading.
	GetMarketStatus(ctx context.Context, symbol string) (MarketStatus, error)
}

// PortfolioProvider extends Provider with balance and pricing capabilities.
// It mirrors the existing pkg/exchanges.PricedExchange surface.
type PortfolioProvider interface {
	Provider

	// GetBalances returns a flat list of non-zero balances (spot / default wallet).
	GetBalances(ctx context.Context) ([]Balance, error)

	// GetWalletBalances returns balances for a specific wallet type.
	// Pass "all" to aggregate across all supported wallet types.
	GetWalletBalances(ctx context.Context, walletType string) ([]WalletBalance, error)

	// FetchPrice returns the last-traded price of asset in terms of quote (e.g. "USDT").
	// Returns (0, nil) when asset IS quote or a recognised stablecoin equivalent.
	FetchPrice(ctx context.Context, asset, quote string) (float64, error)

	// SupportedWalletTypes returns the wallet-type keys accepted by GetWalletBalances.
	SupportedWalletTypes() []string
}

// MarketDataProvider extends Provider with read-only market-data feeds.
type MarketDataProvider interface {
	Provider

	// FetchTicker returns the latest ticker for symbol (e.g. "BTC/USDT").
	FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error)

	// FetchTickers returns tickers for a set of symbols (max 20 recommended).
	// Pass nil or empty slice to fetch all available tickers.
	FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error)

	// FetchOHLCV returns candlestick data.
	// timeframe is one of: 1m 5m 15m 1h 4h 1d 1w
	// limit is capped at 500 by callers.
	FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error)

	// FetchOrderBook returns the current order book.
	// depth is capped at 50 by callers.
	FetchOrderBook(ctx context.Context, symbol string, depth int) (ccxt.OrderBook, error)

	// LoadMarkets refreshes the cached market catalogue and returns the map.
	LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error)
}

// TradingProvider extends Provider with order management.
type TradingProvider interface {
	Provider

	// CreateOrder submits a new order.
	// orderType: "limit" | "market" | "stop_loss" | "take_profit"
	// side: "buy" | "sell"
	CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error)

	// CancelOrder cancels an open order by ID.
	CancelOrder(ctx context.Context, id, symbol string) (ccxt.Order, error)

	// FetchOrder retrieves a single order by ID.
	FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error)

	// FetchOpenOrders returns all open orders, optionally filtered by symbol.
	FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error)

	// FetchClosedOrders returns closed/filled orders.
	FetchClosedOrders(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error)

	// FetchMyTrades returns the personal trade history.
	FetchMyTrades(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error)
}

// TransferProvider extends Provider with internal fund-transfer capability.
type TransferProvider interface {
	Provider

	// Transfer moves funds between internal sub-accounts (e.g. spot → futures).
	Transfer(ctx context.Context, asset string, amount float64, fromAccount, toAccount string) (ccxt.TransferEntry, error)
}

// Balance mirrors pkg/exchanges.Balance so callers don't need to import both.
type Balance struct {
	Asset  string
	Free   float64
	Locked float64
}

// WalletBalance extends Balance with wallet-type metadata.
type WalletBalance struct {
	Balance
	WalletType string
	Extra      map[string]string
}
