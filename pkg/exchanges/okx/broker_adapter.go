package okx

// OKXBrokerAdapter wraps OKXExchange to implement the full broker provider hierarchy.
// It satisfies: broker.PortfolioProvider, broker.MarketDataProvider,
// broker.TradingProvider, broker.TransferProvider.

import (
	"context"
	"fmt"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// catchPanic converts a CCXT panic (which the library uses instead of returning errors) into a Go error.
func catchPanic(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}

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

func (a *OKXBrokerAdapter) FetchTicker(_ context.Context, symbol string) (t ccxt.Ticker, err error) {
	err = catchPanic(func() error { t, err = a.publicClient.FetchTicker(symbol); return err })
	return
}

func (a *OKXBrokerAdapter) FetchTickers(_ context.Context, symbols []string) (result map[string]ccxt.Ticker, err error) {
	var tickers ccxt.Tickers
	err = catchPanic(func() error {
		var e error
		if len(symbols) == 0 {
			tickers, e = a.publicClient.FetchTickers()
		} else {
			tickers, e = a.publicClient.FetchTickers(ccxt.WithFetchTickersSymbols(symbols))
		}
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("okx: FetchTickers: %w", err)
	}
	return tickers.Tickers, nil
}

func (a *OKXBrokerAdapter) FetchOHLCV(_ context.Context, symbol, timeframe string, since *int64, limit int) (out []ccxt.OHLCV, err error) {
	opts := []ccxt.FetchOHLCVOptions{ccxt.WithFetchOHLCVTimeframe(timeframe)}
	if since != nil {
		opts = append(opts, ccxt.WithFetchOHLCVSince(*since))
	}
	if limit > 0 {
		opts = append(opts, ccxt.WithFetchOHLCVLimit(int64(limit)))
	}
	err = catchPanic(func() error { out, err = a.publicClient.FetchOHLCV(symbol, opts...); return err })
	return
}

func (a *OKXBrokerAdapter) FetchOrderBook(_ context.Context, symbol string, depth int) (ob ccxt.OrderBook, err error) {
	err = catchPanic(func() error {
		if depth > 0 {
			ob, err = a.publicClient.FetchOrderBook(symbol, ccxt.WithFetchOrderBookLimit(int64(depth)))
		} else {
			ob, err = a.publicClient.FetchOrderBook(symbol)
		}
		return err
	})
	return
}

func (a *OKXBrokerAdapter) LoadMarkets(_ context.Context) (m map[string]ccxt.MarketInterface, err error) {
	err = catchPanic(func() error { m, err = a.publicClient.LoadMarkets(); return err })
	return
}

// --- broker.TradingProvider ---

func (a *OKXBrokerAdapter) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (o ccxt.Order, err error) {
	opts := []ccxt.CreateOrderOptions{}
	if price != nil {
		opts = append(opts, ccxt.WithCreateOrderPrice(*price))
	}
	if len(params) > 0 {
		opts = append(opts, ccxt.WithCreateOrderParams(params))
	}
	err = catchPanic(func() error { o, err = a.client.CreateOrder(symbol, orderType, side, amount, opts...); return err })
	return
}

func (a *OKXBrokerAdapter) CancelOrder(_ context.Context, id, symbol string) (o ccxt.Order, err error) {
	err = catchPanic(func() error { o, err = a.client.CancelOrder(id, ccxt.WithCancelOrderSymbol(symbol)); return err })
	return
}

func (a *OKXBrokerAdapter) FetchOrder(_ context.Context, id, symbol string) (o ccxt.Order, err error) {
	err = catchPanic(func() error { o, err = a.client.FetchOrder(id, ccxt.WithFetchOrderSymbol(symbol)); return err })
	return
}

func (a *OKXBrokerAdapter) FetchOpenOrders(_ context.Context, symbol string) (orders []ccxt.Order, err error) {
	err = catchPanic(func() error {
		if symbol != "" {
			orders, err = a.client.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol(symbol))
		} else {
			orders, err = a.client.FetchOpenOrders()
		}
		return err
	})
	return
}

func (a *OKXBrokerAdapter) FetchClosedOrders(_ context.Context, symbol string, since *int64, limit int) (orders []ccxt.Order, err error) {
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
	err = catchPanic(func() error { orders, err = a.client.FetchClosedOrders(opts...); return err })
	return
}

func (a *OKXBrokerAdapter) FetchMyTrades(_ context.Context, symbol string, since *int64, limit int) (trades []ccxt.Trade, err error) {
	// OKX /api/v5/trade/fills accepts at most 100 records per request.
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	opts := []ccxt.FetchMyTradesOptions{ccxt.WithFetchMyTradesLimit(int64(limit))}
	if symbol != "" {
		opts = append(opts, ccxt.WithFetchMyTradesSymbol(symbol))
	}
	if since != nil {
		opts = append(opts, ccxt.WithFetchMyTradesSince(*since))
	}
	err = catchPanic(func() error { trades, err = a.client.FetchMyTrades(opts...); return err })
	return
}

// --- broker.TransferProvider ---

func (a *OKXBrokerAdapter) Transfer(_ context.Context, asset string, amount float64, fromAccount, toAccount string) (ccxt.TransferEntry, error) {
	return a.client.Transfer(asset, amount, fromAccount, toAccount)
}

// --- broker.FuturesProvider ---

func (a *OKXBrokerAdapter) SetFuturesLeverage(_ context.Context, symbol string, leverage int64, marginMode, positionSide string) (out map[string]interface{}, err error) {
	if err := a.requireAuth(); err != nil {
		return nil, err
	}
	params := map[string]interface{}{}
	if marginMode != "" {
		params["marginMode"] = strings.ToLower(marginMode)
	}
	if positionSide != "" {
		params["posSide"] = strings.ToLower(positionSide)
	}
	err = catchPanic(func() error {
		out, err = a.client.SetLeverage(leverage, ccxt.WithSetLeverageSymbol(symbol), ccxt.WithSetLeverageParams(params))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("okx futures: set leverage: %w", err)
	}
	return out, nil
}

func (a *OKXBrokerAdapter) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (o ccxt.Order, err error) {
	if err := a.requireAuth(); err != nil {
		return o, err
	}
	opts := []ccxt.CreateOrderOptions{}
	if req.Price != nil {
		opts = append(opts, ccxt.WithCreateOrderPrice(*req.Price))
	}
	params := copyParams(req.Params)
	if req.MarginMode != "" {
		params["tdMode"] = strings.ToLower(req.MarginMode)
	}
	if req.PositionSide != "" {
		params["posSide"] = strings.ToLower(req.PositionSide)
	}
	if req.ReduceOnly {
		params["reduceOnly"] = true
	}
	if len(params) > 0 {
		opts = append(opts, ccxt.WithCreateOrderParams(params))
	}
	err = catchPanic(func() error {
		o, err = a.client.CreateOrder(req.Symbol, req.OrderType, req.Side, req.Amount, opts...)
		return err
	})
	return
}

func (a *OKXBrokerAdapter) FetchFuturesOrder(_ context.Context, id, symbol string) (o ccxt.Order, err error) {
	err = catchPanic(func() error { o, err = a.client.FetchOrder(id, ccxt.WithFetchOrderSymbol(symbol)); return err })
	return
}

func (a *OKXBrokerAdapter) FetchFuturesOpenOrders(_ context.Context, symbol string) (orders []ccxt.Order, err error) {
	err = catchPanic(func() error {
		if symbol != "" {
			orders, err = a.client.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol(symbol))
		} else {
			orders, err = a.client.FetchOpenOrders()
		}
		return err
	})
	return
}

func (a *OKXBrokerAdapter) FetchFuturesPositions(_ context.Context, symbols []string) (positions []ccxt.Position, err error) {
	opts := []ccxt.FetchPositionsOptions{}
	if len(symbols) > 0 {
		opts = append(opts, ccxt.WithFetchPositionsSymbols(symbols))
	}
	err = catchPanic(func() error { positions, err = a.client.FetchPositions(opts...); return err })
	return
}

func (a *OKXBrokerAdapter) FetchFuturesFundingRate(_ context.Context, symbol string) (rate ccxt.FundingRate, err error) {
	err = catchPanic(func() error { rate, err = a.client.FetchFundingRate(symbol); return err })
	return
}

func (a *OKXBrokerAdapter) FetchFuturesFundingHistory(_ context.Context, symbol string, since *int64, limit int) (history []ccxt.FundingHistory, err error) {
	opts := []ccxt.FetchFundingHistoryOptions{}
	if symbol != "" {
		opts = append(opts, ccxt.WithFetchFundingHistorySymbol(symbol))
	}
	if since != nil {
		opts = append(opts, ccxt.WithFetchFundingHistorySince(*since))
	}
	if limit > 0 {
		opts = append(opts, ccxt.WithFetchFundingHistoryLimit(int64(limit)))
	}
	err = catchPanic(func() error { history, err = a.client.FetchFundingHistory(opts...); return err })
	return
}

func copyParams(in map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range in {
		out[k] = v
	}
	return out
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
