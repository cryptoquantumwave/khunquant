package webull

import (
	"context"
	"fmt"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// webullAdapter wraps Client with broker.Provider hierarchy.
type webullAdapter struct {
	client *Client
	cfg    config.WebullExchangeAccount
}

func newBrokerAdapter(cfg config.WebullExchangeAccount) (*webullAdapter, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &webullAdapter{client: client, cfg: cfg}, nil
}

// --- broker.Provider ---

func (a *webullAdapter) ID() string { return Name }

func (a *webullAdapter) Category() broker.AssetCategory { return broker.CategoryStock }

// GetMarketStatus returns whether the US equity market is currently open.
// Regular market hours: 09:30–16:00 America/New_York, Monday–Friday.
// TODO: no US market holiday calendar — this will incorrectly report MarketOpen
// on holidays/half-days. Harmless while trading is deferred (webullAdapter does
// not implement broker.TradingProvider), but must be addressed before order
// placement gates on this check. See docs/webull-integration.md.
func (a *webullAdapter) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	// Load US Eastern timezone
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		return broker.MarketUnknown, nil
	}

	now := time.Now().In(eastern)

	// Check if weekend
	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return broker.MarketClosed, nil
	}

	// Check regular hours: 09:30–16:00
	h, m := now.Hour(), now.Minute()
	totalMin := h*60 + m

	regularOpen := 9*60 + 30 // 09:30
	regularClose := 16 * 60  // 16:00

	if totalMin >= regularOpen && totalMin < regularClose {
		return broker.MarketOpen, nil
	}

	return broker.MarketClosed, nil
}

// --- broker.PortfolioProvider ---

func (a *webullAdapter) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	balances, err := a.client.FetchBalances(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]broker.Balance, 0, len(balances.Balances))
	for _, b := range balances.Balances {
		if b.Free > 0 || b.Locked > 0 {
			result = append(result, broker.Balance{
				Asset:  b.Asset,
				Free:   b.Free,
				Locked: b.Locked,
			})
		}
	}
	return result, nil
}

func (a *webullAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	switch strings.ToLower(walletType) {
	case "cash", "":
		// Cash wallet includes USD and other liquid currency balances
		balances, err := a.client.FetchBalances(ctx)
		if err != nil {
			return nil, err
		}

		result := make([]broker.WalletBalance, 0)
		for _, b := range balances.Balances {
			// Treat USD and cash-like assets as "cash" wallet
			if strings.EqualFold(b.Asset, "USD") || strings.EqualFold(b.Asset, "CASH") {
				result = append(result, broker.WalletBalance{
					Balance:    broker.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked},
					WalletType: "cash",
					Extra:      map[string]string{"market_value": fmt.Sprintf("%g", b.MarketValue)},
				})
			}
		}
		return result, nil

	case "stock":
		// Stock wallet includes equity holdings
		positions, err := a.client.FetchPositions(ctx)
		if err != nil {
			return nil, err
		}

		result := make([]broker.WalletBalance, 0, len(positions.Positions))
		for _, p := range positions.Positions {
			result = append(result, broker.WalletBalance{
				Balance: broker.Balance{
					Asset:  p.Symbol,
					Free:   p.Quantity,
					Locked: 0,
				},
				WalletType: "stock",
				Extra: map[string]string{
					"avg_cost":      fmt.Sprintf("%g", p.AvgPrice),
					"current_price": fmt.Sprintf("%g", p.CurrentPrice),
					"market_value":  fmt.Sprintf("%g", p.MarketValue),
					"unrealized_pl": fmt.Sprintf("%g", p.UnrealizedPnL),
					"percent_pnl":   fmt.Sprintf("%g", p.PercentPnL),
				},
			})
		}
		return result, nil

	case "all":
		// Aggregate cash + stock
		cash, err := a.GetWalletBalances(ctx, "cash")
		if err != nil {
			return nil, err
		}
		stocks, err := a.GetWalletBalances(ctx, "stock")
		if err != nil {
			return nil, err
		}
		return append(cash, stocks...), nil

	default:
		return nil, fmt.Errorf("webull: unsupported wallet type %q (use \"cash\", \"stock\", or \"all\")", walletType)
	}
}

func (a *webullAdapter) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	// Only USD quotes are supported
	if !strings.EqualFold(quote, "USD") && quote != "" {
		return 0, fmt.Errorf("webull: only USD quotes are supported (got %q)", quote)
	}

	// USD is the quote currency itself — return (0, nil) to signal 1:1
	if strings.EqualFold(asset, "USD") {
		return 0, nil
	}

	resp, err := a.client.FetchQuote(ctx, strings.ToUpper(asset))
	if err != nil {
		return 0, err
	}
	// A Webull stock quote is never a 1:1 self-pair, so a non-positive Last
	// (halted symbol, unavailable data, or a response-schema mismatch) must
	// not collide with the codebase's price==0 "asset IS the quote" sentinel.
	if resp.Last <= 0 {
		return 0, fmt.Errorf("webull: invalid price for %s (got %v)", asset, resp.Last)
	}
	return resp.Last, nil
}

func (a *webullAdapter) SupportedWalletTypes() []string {
	return []string{"cash", "stock"}
}

// --- broker.MarketDataProvider ---

// webullTimeframe maps CCXT unified timeframes to Webull interval strings.
var webullTimeframe = map[string]string{
	"1m":  "1m",
	"5m":  "5m",
	"15m": "15m",
	"1h":  "1h",
	"4h":  "4h",
	"1d":  "1d",
	"1w":  "1w",
}

func (a *webullAdapter) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	sym := toWebullSymbol(symbol)
	resp, err := a.client.FetchQuote(ctx, sym)
	if err != nil {
		return ccxt.Ticker{}, fmt.Errorf("webull: FetchTicker %s: %w", symbol, err)
	}
	return quoteToTicker(symbol, resp), nil
}

func (a *webullAdapter) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	out := make(map[string]ccxt.Ticker, len(symbols))
	for _, sym := range symbols {
		t, err := a.FetchTicker(ctx, sym)
		if err != nil {
			return nil, err
		}
		out[sym] = t
	}
	return out, nil
}

func (a *webullAdapter) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if since != nil {
		return nil, fmt.Errorf("webull: FetchOHLCV does not support a since parameter (only limit is supported)")
	}

	interval, ok := webullTimeframe[timeframe]
	if !ok {
		interval = "1d"
	}

	sym := toWebullSymbol(symbol)

	bars, err := a.client.FetchBars(ctx, sym, interval, limit)
	if err != nil {
		return nil, fmt.Errorf("webull: FetchOHLCV %s: %w", symbol, err)
	}

	out := make([]ccxt.OHLCV, len(bars.Bars))
	for i, b := range bars.Bars {
		out[i] = ccxt.OHLCV{
			Timestamp: b.Timestamp,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    b.Volume,
		}
	}
	return out, nil
}

// FetchOrderBook is not supported via Webull OpenAPI.
func (a *webullAdapter) FetchOrderBook(_ context.Context, symbol string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, fmt.Errorf("webull: order book is not available via the OpenAPI (symbol: %s)", symbol)
}

// LoadMarkets is not supported via Webull OpenAPI.
func (a *webullAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, fmt.Errorf("webull: LoadMarkets is not supported via the OpenAPI")
}

// --- Helpers ---

func toWebullSymbol(symbol string) string {
	if idx := strings.Index(symbol, "/"); idx != -1 {
		return strings.ToUpper(symbol[:idx])
	}
	return strings.ToUpper(symbol)
}

func quoteToTicker(symbol string, resp *QuoteResponse) ccxt.Ticker {
	sym := symbol
	now := time.Now().UnixMilli()
	return ccxt.Ticker{
		Symbol:     &sym,
		Last:       &resp.Last,
		High:       &resp.High,
		Low:        &resp.Low,
		Open:       &resp.Open,
		Close:      &resp.Close,
		BaseVolume: &resp.Volume,
		Percentage: &resp.ChangePercent,
		Timestamp:  &now,
	}
}

// --- init ---

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Webull.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return newBrokerAdapter(acc)
	})
	broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Webull.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.Webull.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, fmt.Errorf("%s: account %q not found (available: %v)", Name, accountName, names)
		}
		return newBrokerAdapter(acc)
	})
}
