package okx

// OKXBrokerAdapter wraps OKXExchange to implement the full broker provider hierarchy.
// It satisfies: broker.PortfolioProvider, broker.MarketDataProvider,
// broker.TradingProvider, broker.TransferProvider.

import (
	"context"
	"fmt"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OKXBrokerAdapter wraps OKXExchange with the broker.Provider hierarchy.
type OKXBrokerAdapter struct {
	*OKXExchange
}

func newBrokerAdapter(creds config.OKXExchangeAccount, testnet bool) (*OKXBrokerAdapter, error) {
	ex, err := NewOKXExchange(creds, testnet)
	if err != nil {
		return nil, err
	}
	if creds.APIKey.String() != "" {
		logger.RegisterSecret(creds.APIKey.String())
	}
	if creds.Secret.String() != "" {
		logger.RegisterSecret(creds.Secret.String())
	}
	if creds.Passphrase.String() != "" {
		logger.RegisterSecret(creds.Passphrase.String())
	}
	return &OKXBrokerAdapter{OKXExchange: ex}, nil
}

// --- broker.Provider ---

func (a *OKXBrokerAdapter) ID() string { return Name }

func (a *OKXBrokerAdapter) Category() broker.AssetCategory { return broker.CategoryCrypto }

func (a *OKXBrokerAdapter) GetMarketStatus(_ context.Context, symbol string) (broker.MarketStatus, error) {
	ticker, err := a.publicClient.FetchTicker(symbol)
	if err != nil {
		return broker.MarketUnknown, fmt.Errorf("okx: GetMarketStatus: %w", err)
	}
	if ticker.Last != nil {
		return broker.MarketOpen, nil
	}
	return broker.MarketUnknown, nil
}

// --- broker.PortfolioProvider ---

func (a *OKXBrokerAdapter) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	bals, err := a.OKXExchange.GetBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]broker.Balance, len(bals))
	for i, b := range bals {
		out[i] = broker.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked}
	}
	return out, nil
}

func (a *OKXBrokerAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	bals, err := a.OKXExchange.GetWalletBalances(ctx, walletType)
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

func (a *OKXBrokerAdapter) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return a.OKXExchange.FetchPrice(ctx, asset, quote)
}

func (a *OKXBrokerAdapter) SupportedWalletTypes() []string {
	return a.OKXExchange.SupportedWalletTypes()
}

// --- broker.MarketDataProvider ---

func (a *OKXBrokerAdapter) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	return a.publicClient.FetchTicker(symbol)
}

func (a *OKXBrokerAdapter) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	var tickers ccxt.Tickers
	var err error
	if len(symbols) == 0 {
		tickers, err = a.publicClient.FetchTickers()
	} else {
		tickers, err = a.publicClient.FetchTickers(ccxt.WithFetchTickersSymbols(symbols))
	}
	if err != nil {
		return nil, fmt.Errorf("okx: FetchTickers: %w", err)
	}
	return tickers.Tickers, nil
}

func (a *OKXBrokerAdapter) FetchOHLCV(_ context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	opts := []ccxt.FetchOHLCVOptions{ccxt.WithFetchOHLCVTimeframe(timeframe)}
	if since != nil {
		opts = append(opts, ccxt.WithFetchOHLCVSince(*since))
	}
	if limit > 0 {
		opts = append(opts, ccxt.WithFetchOHLCVLimit(int64(limit)))
	}
	return a.publicClient.FetchOHLCV(symbol, opts...)
}

func (a *OKXBrokerAdapter) FetchOrderBook(_ context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	if depth > 0 {
		return a.publicClient.FetchOrderBook(symbol, ccxt.WithFetchOrderBookLimit(int64(depth)))
	}
	return a.publicClient.FetchOrderBook(symbol)
}

func (a *OKXBrokerAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return a.publicClient.LoadMarkets()
}

// --- broker.TradingProvider ---

func (a *OKXBrokerAdapter) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	opts := []ccxt.CreateOrderOptions{}
	if price != nil {
		opts = append(opts, ccxt.WithCreateOrderPrice(*price))
	}
	if len(params) > 0 {
		opts = append(opts, ccxt.WithCreateOrderParams(params))
	}
	return a.client.CreateOrder(symbol, orderType, side, amount, opts...)
}

func (a *OKXBrokerAdapter) CancelOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	return a.client.CancelOrder(id, ccxt.WithCancelOrderSymbol(symbol))
}

func (a *OKXBrokerAdapter) FetchOrder(_ context.Context, id, symbol string) (ccxt.Order, error) {
	return a.client.FetchOrder(id, ccxt.WithFetchOrderSymbol(symbol))
}

func (a *OKXBrokerAdapter) FetchOpenOrders(_ context.Context, symbol string) ([]ccxt.Order, error) {
	if symbol != "" {
		return a.client.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol(symbol))
	}
	return a.client.FetchOpenOrders()
}

func (a *OKXBrokerAdapter) FetchClosedOrders(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	opts := []ccxt.FetchClosedOrdersOptions{}
	if symbol != "" {
		opts = append(opts, ccxt.WithFetchClosedOrdersSymbol(symbol))
	}
	if since != nil {
		opts = append(opts, ccxt.WithFetchClosedOrdersSince(*since))
	}
	if limit > 0 {
		opts = append(opts, ccxt.WithFetchClosedOrdersLimit(int64(limit)))
	}
	return a.client.FetchClosedOrders(opts...)
}

func (a *OKXBrokerAdapter) FetchMyTrades(_ context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	opts := []ccxt.FetchMyTradesOptions{}
	if symbol != "" {
		opts = append(opts, ccxt.WithFetchMyTradesSymbol(symbol))
	}
	if since != nil {
		opts = append(opts, ccxt.WithFetchMyTradesSince(*since))
	}
	if limit > 0 {
		opts = append(opts, ccxt.WithFetchMyTradesLimit(int64(limit)))
	}
	return a.client.FetchMyTrades(opts...)
}

// --- broker.TransferProvider ---

func (a *OKXBrokerAdapter) Transfer(_ context.Context, asset string, amount float64, fromAccount, toAccount string) (ccxt.TransferEntry, error) {
	return a.client.Transfer(asset, amount, fromAccount, toAccount)
}

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.OKX.ResolveAccount("")
		if !ok {
			return newBrokerAdapter(config.OKXExchangeAccount{}, cfg.Exchanges.OKX.Testnet)
		}
		return newBrokerAdapter(acc, cfg.Exchanges.OKX.Testnet)
	})
	broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.OKX.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.OKX.Accounts {
				n := a.Name
				if n == "" {
					n = fmt.Sprintf("%d", i+1)
				}
				names = append(names, n)
			}
			return nil, fmt.Errorf("%s: account %q not found (available: %v)", Name, accountName, names)
		}
		return newBrokerAdapter(acc, cfg.Exchanges.OKX.Testnet)
	})
}
