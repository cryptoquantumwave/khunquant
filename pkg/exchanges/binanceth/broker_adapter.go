package binanceth

// BinanceTHBrokerAdapter implements broker.PortfolioProvider and broker.MarketDataProvider.
// BinanceTH does not provide order execution APIs, so TradingProvider and
// TransferProvider are not implemented.

import (
	"context"
	"fmt"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/logger"
	"github.com/khunquant/khunquant/pkg/providers/broker"
)

// BinanceTHBrokerAdapter wraps BinanceTHExchange with the broker.Provider hierarchy.
type BinanceTHBrokerAdapter struct {
	*BinanceTHExchange
}

func newBrokerAdapter(creds config.ExchangeAccount) (*BinanceTHBrokerAdapter, error) {
	ex, err := NewBinanceTHExchange(creds)
	if err != nil {
		return nil, err
	}
	logger.RegisterSecret(creds.APIKey)
	logger.RegisterSecret(creds.Secret)
	return &BinanceTHBrokerAdapter{BinanceTHExchange: ex}, nil
}

// --- broker.Provider ---

func (a *BinanceTHBrokerAdapter) ID() string { return Name }

func (a *BinanceTHBrokerAdapter) Category() broker.AssetCategory { return broker.CategoryCrypto }

func (a *BinanceTHBrokerAdapter) GetMarketStatus(ctx context.Context, symbol string) (broker.MarketStatus, error) {
	// Use fetchTickerPrice to check market status — if it returns a price, market is open.
	// BinanceTH symbols have no separator (e.g. "BTCTHB").
	_, err := a.fetchTickerPrice(ctx, symbol)
	if err != nil {
		return broker.MarketUnknown, nil
	}
	return broker.MarketOpen, nil
}

// --- broker.PortfolioProvider ---

func (a *BinanceTHBrokerAdapter) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	bals, err := a.BinanceTHExchange.GetBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]broker.Balance, len(bals))
	for i, b := range bals {
		out[i] = broker.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked}
	}
	return out, nil
}

func (a *BinanceTHBrokerAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	bals, err := a.BinanceTHExchange.GetWalletBalances(ctx, walletType)
	if err != nil {
		return nil, err
	}
	out := make([]broker.WalletBalance, len(bals))
	for i, b := range bals {
		out[i] = broker.WalletBalance{
			Balance:    broker.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked},
			WalletType: b.WalletType,
			Extra:      b.Extra,
		}
	}
	return out, nil
}

func (a *BinanceTHBrokerAdapter) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return a.BinanceTHExchange.FetchPrice(ctx, asset, quote)
}

func (a *BinanceTHBrokerAdapter) SupportedWalletTypes() []string {
	return a.BinanceTHExchange.SupportedWalletTypes()
}

// --- broker.MarketDataProvider ---
// BinanceTH exposes a simple price endpoint. FetchTicker wraps it to return a partial Ticker.

func (a *BinanceTHBrokerAdapter) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	price, err := a.fetchTickerPrice(ctx, symbol)
	if err != nil {
		return ccxt.Ticker{}, fmt.Errorf("binanceth: FetchTicker %s: %w", symbol, err)
	}
	sym := symbol
	return ccxt.Ticker{Symbol: &sym, Last: &price}, nil
}

func (a *BinanceTHBrokerAdapter) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
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

// FetchOHLCV is not supported by the BinanceTH public API.
func (a *BinanceTHBrokerAdapter) FetchOHLCV(_ context.Context, symbol, _ string, _ *int64, _ int) ([]ccxt.OHLCV, error) {
	return nil, fmt.Errorf("binanceth: FetchOHLCV not supported for symbol %s", symbol)
}

// FetchOrderBook is not supported by the BinanceTH public API.
func (a *BinanceTHBrokerAdapter) FetchOrderBook(_ context.Context, symbol string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, fmt.Errorf("binanceth: FetchOrderBook not supported for symbol %s", symbol)
}

// LoadMarkets is not supported by the BinanceTH public API.
func (a *BinanceTHBrokerAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, fmt.Errorf("binanceth: LoadMarkets not supported")
}

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.BinanceTH.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return newBrokerAdapter(acc)
	})
	broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.BinanceTH.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.BinanceTH.Accounts {
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
