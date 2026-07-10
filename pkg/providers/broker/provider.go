// Package broker defines the unified provider hierarchy for all asset classes.
// It mirrors the pkg/exchanges interface hierarchy but extends it with
// market-data, trading, and transfer capabilities.
package broker

import (
	"context"
	"fmt"
	"strings"

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

// FuturesOrderRequest is the exchange-neutral futures order input.
// Symbols should use CCXT contract notation, e.g. "BTC/USDT:USDT" for USDT-settled
// perpetual swaps.
type FuturesOrderRequest struct {
	Symbol       string
	OrderType    string
	Side         string
	Amount       float64
	Price        *float64
	MarginMode   string
	PositionSide string
	ReduceOnly   bool
	Params       map[string]interface{}
}

// FuturesProvider extends Provider with perpetual/futures trading and account data.
// Binance TH and Bitkub intentionally do not implement this interface because they
// do not offer futures trading through this app.
type FuturesProvider interface {
	Provider

	SetFuturesLeverage(ctx context.Context, symbol string, leverage int64, marginMode, positionSide string) (map[string]interface{}, error)
	CreateFuturesOrder(ctx context.Context, req FuturesOrderRequest) (ccxt.Order, error)
	FetchFuturesOrder(ctx context.Context, id, symbol string) (ccxt.Order, error)
	FetchFuturesOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error)
	FetchFuturesPositions(ctx context.Context, symbols []string) ([]ccxt.Position, error)
	FetchFuturesFundingRate(ctx context.Context, symbol string) (ccxt.FundingRate, error)
	FetchFuturesFundingRates(ctx context.Context, symbols []string) (map[string]ccxt.FundingRate, error)
	FetchFuturesFundingHistory(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingHistory, error)
	FetchPublicFundingRateHistory(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.FundingRateHistory, error)
	LoadFuturesMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error)
	FetchFuturesMarkPrice(ctx context.Context, symbol string) (float64, error)
	CancelFuturesOrder(ctx context.Context, id, symbol string) (ccxt.Order, error)
	CancelAllFuturesOrders(ctx context.Context, symbol string) ([]ccxt.Order, error)
}

// EarnRatePoint represents a single historical earn rate data point.
// Rate is an annualized fraction (0.05 == 5% APY).
// Timestamp is in milliseconds.
type EarnRatePoint struct {
	Rate      float64
	Timestamp int64
}

// EarnProduct describes a flexible savings/earn product offered for an asset.
// APY is a fraction (0.05 == 5%).
type EarnProduct struct {
	Exchange      string
	Asset         string
	ProductID     string
	APY           float64
	CanSubscribe  bool
	AutoSubscribe bool
	MinSubscribe  float64
	// Type distinguishes product categories: "savings" (lending pool) or "staking-defi" (on-chain earn).
	// Empty string is treated as "savings" for backward compatibility.
	Type     string
	Protocol string // human-readable protocol name for staking-defi (e.g. "Chiliz")
}

// EarnPosition describes a currently held flexible earn position.
// APY is a fraction (0.05 == 5%).
type EarnPosition struct {
	Exchange      string
	Asset         string
	ProductID     string
	Amount        float64
	APY           float64
	AutoSubscribe bool
}

// EarnProvider is implemented by providers that expose flexible savings/earn
// products for the spot leg of a delta-neutral position. All methods require
// authenticated credentials except where the underlying APY endpoint is public.
type EarnProvider interface {
	Provider
	// FetchFlexibleEarnProducts returns flexible earn products. asset == "" returns all.
	FetchFlexibleEarnProducts(ctx context.Context, asset string) ([]EarnProduct, error)
	// FetchFlexibleEarnPositions returns currently held flexible earn positions.
	FetchFlexibleEarnPositions(ctx context.Context) ([]EarnPosition, error)
	// SubscribeFlexibleEarn moves amount of asset into the given flexible product.
	// Returns an exchange transaction/purchase id.
	SubscribeFlexibleEarn(ctx context.Context, productID, asset string, amount float64, autoSubscribe bool) (string, error)
	// RedeemFlexibleEarn redeems amount (or all) of asset from the flexible product.
	// Returns an exchange transaction/redemption id.
	RedeemFlexibleEarn(ctx context.Context, productID, asset string, amount float64, redeemAll bool) (string, error)
	// SetFlexibleAutoSubscribe enables/disables auto-subscribe for the product/asset.
	SetFlexibleAutoSubscribe(ctx context.Context, productID, asset string, enable bool) error
	// FetchFlexibleEarnRateHistory returns historical rate data points.
	// productID is required; since is optional. limit capped at 100 by callers.
	FetchFlexibleEarnRateHistory(ctx context.Context, productID, asset string, since *int64, limit int) ([]EarnRatePoint, error)
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

// --- Options (Market Data + Trading) ---

// OptionContract represents a single options contract specification.
type OptionContract struct {
	Underlying string // e.g. "AAPL"
	Expiry     string // yyyy-MM-dd format
	Strike     float64
	OptionType string // CALL or PUT
}

// OptionQuote represents the quote data for an options contract.
// Includes price, greeks, and open interest.
type OptionQuote struct {
	Contract    OptionContract
	Symbol      string // Encoded symbol (e.g. AAPL260821C00320000)
	Price       float64
	Bid         float64
	Ask         float64
	BidSize     float64
	AskSize     float64
	Open        float64
	High        float64
	Low         float64
	PreClose    float64
	Change      float64
	ChangeRatio float64
	// Greeks
	Delta        float64
	Gamma        float64
	Theta        float64
	Vega         float64
	Rho          float64
	ImpVol       float64 // Implied Volatility
	Volume       float64
	OpenInterest float64
	StrikePrice  float64
	Timestamp    int64 // Unix milliseconds
}

// OptionLeg represents a single leg in an options order.
type OptionLeg struct {
	Side       string // buy or sell
	Quantity   float64
	Underlying string
	Strike     float64
	Expiry     string // yyyy-MM-dd
	OptionType string // CALL or PUT
}

// OptionOrderRequest represents an options order placement request.
type OptionOrderRequest struct {
	Underlying  string // e.g. "AAPL"
	Strategy    string // SINGLE for now (VERTICAL, STRADDLE, etc. deferred)
	OrderType   string // limit, stop_loss, stop_loss_limit
	Side        string // buy or sell
	Quantity    float64
	LimitPrice  *float64
	StopPrice   *float64
	TimeInForce string // DAY or GTC
	Legs        []OptionLeg
}

// Validate checks a single-leg option order request for shape correctness:
// strategy, order type + required prices (rejecting MARKET/TAKE_PROFIT), side,
// time-in-force (GTC not allowed on SELL), exactly one leg, and leg side /
// option type. It is the single source of option request-shape validation shared
// by the option tool layer and provider adapters. It does NOT enforce policy
// gates (permissions, risk, confirmation) — those remain in the tool layer.
func (r OptionOrderRequest) Validate() error {
	strategy := r.Strategy
	if strategy == "" {
		strategy = "SINGLE"
	}
	if strategy != "SINGLE" {
		return fmt.Errorf("only single-leg options orders are supported (got strategy %q)", strategy)
	}

	orderType := strings.ToUpper(r.OrderType)
	switch orderType {
	case "LIMIT":
		if r.LimitPrice == nil {
			return fmt.Errorf("limit_price is required for LIMIT option orders")
		}
	case "STOP_LOSS":
		if r.StopPrice == nil {
			return fmt.Errorf("stop_price is required for STOP_LOSS option orders")
		}
	case "STOP_LOSS_LIMIT":
		if r.LimitPrice == nil || r.StopPrice == nil {
			return fmt.Errorf("both limit_price and stop_price are required for STOP_LOSS_LIMIT option orders")
		}
	case "MARKET", "TAKE_PROFIT":
		return fmt.Errorf("order type %q is not supported for options (use LIMIT, STOP_LOSS, or STOP_LOSS_LIMIT)", orderType)
	default:
		return fmt.Errorf("unsupported option order type %q", r.OrderType)
	}

	side := strings.ToUpper(r.Side)
	if side != "BUY" && side != "SELL" {
		return fmt.Errorf("unknown option order side %q (must be BUY or SELL)", r.Side)
	}

	tif := strings.ToUpper(r.TimeInForce)
	if tif != "DAY" && tif != "GTC" {
		return fmt.Errorf("unsupported time_in_force %q for options (use DAY or GTC)", r.TimeInForce)
	}
	if side == "SELL" && tif == "GTC" {
		return fmt.Errorf("GTC (Good-Till-Cancel) is not allowed on SELL orders (use DAY)")
	}

	if len(r.Legs) != 1 {
		return fmt.Errorf("exactly one leg is required for single-leg orders (got %d)", len(r.Legs))
	}
	leg := r.Legs[0]
	if s := strings.ToUpper(leg.Side); s != "BUY" && s != "SELL" {
		return fmt.Errorf("invalid leg side %q", leg.Side)
	}
	if ot := strings.ToUpper(leg.OptionType); ot != "CALL" && ot != "PUT" {
		return fmt.Errorf("invalid option type %q (must be CALL or PUT)", leg.OptionType)
	}
	return nil
}

// OptionMarketDataProvider extends Provider with options market data.
type OptionMarketDataProvider interface {
	Provider

	// FetchOptionSnapshot returns quotes for multiple option contracts.
	FetchOptionSnapshot(ctx context.Context, contracts []OptionContract) ([]OptionQuote, error)

	// FetchOptionOHLCV returns candlestick data for an options contract.
	// timeframe: 1m, 5m, 15m, 30m, 1h, 4h, 1d, 1w (CCXT unified format)
	FetchOptionOHLCV(ctx context.Context, contract OptionContract, timeframe string, limit int) ([]ccxt.OHLCV, error)
}

// OptionTradingProvider extends Provider with options order management.
//
// Symbol convention: every ccxt.Order returned by this interface uses the
// OCC-encoded contract symbol (e.g. "AAPL260821C00320000") as ccxt.Order.Symbol,
// not the bare underlying or a "BASE/USD" pair. Order.Id is the client_order_id.
type OptionTradingProvider interface {
	Provider

	// PlaceOptionOrder submits a new options order.
	PlaceOptionOrder(ctx context.Context, req OptionOrderRequest) (ccxt.Order, error)

	// CancelOptionOrder cancels an open options order by client_order_id.
	CancelOptionOrder(ctx context.Context, clientOrderID string) (ccxt.Order, error)

	// FetchOptionOrder retrieves a single options order by client_order_id.
	FetchOptionOrder(ctx context.Context, clientOrderID string) (ccxt.Order, error)

	// FetchOpenOptionOrders returns all open options orders.
	FetchOpenOptionOrders(ctx context.Context) ([]ccxt.Order, error)
}
