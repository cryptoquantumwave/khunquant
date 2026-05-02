package binanceth

// BinanceTHBrokerAdapter implements broker.PortfolioProvider, broker.MarketDataProvider,
// and broker.TradingProvider using the Binance Thailand REST API.
// TransferProvider is not implemented (BinanceTH has no internal transfer API).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// normalizeSymbol converts CCXT-format symbol (e.g. "BTC/THB") to Binance TH
// format (e.g. "BTCTHB"). Already-normalized symbols are returned unchanged.
func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.ReplaceAll(symbol, "/", ""))
}

// BinanceTHBrokerAdapter wraps BinanceTHExchange with the broker.Provider hierarchy.
type BinanceTHBrokerAdapter struct {
	*BinanceTHExchange
}

func newBrokerAdapter(creds config.ExchangeAccount) (*BinanceTHBrokerAdapter, error) {
	ex, err := NewBinanceTHExchange(creds)
	if err != nil {
		return nil, err
	}
	if creds.APIKey.String() != "" {
		logger.RegisterSecret(creds.APIKey.String())
	}
	if creds.Secret.String() != "" {
		logger.RegisterSecret(creds.Secret.String())
	}
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

// FetchOHLCV fetches OHLCV bars from GET /api/v1/klines.
// Timeframe uses Binance interval notation (e.g. "1h", "1d") — CCXT format matches directly.
func (a *BinanceTHBrokerAdapter) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if timeframe == "" {
		timeframe = "1h"
	}
	bars, err := a.BinanceTHExchange.fetchKlines(ctx, normalizeSymbol(symbol), timeframe, since, limit)
	if err != nil {
		return nil, fmt.Errorf("binanceth: FetchOHLCV %s: %w", symbol, err)
	}
	return bars, nil
}

// FetchOrderBook fetches the order book from GET /api/v1/depth.
func (a *BinanceTHBrokerAdapter) FetchOrderBook(ctx context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	resp, err := a.BinanceTHExchange.fetchDepth(ctx, normalizeSymbol(symbol), depth)
	if err != nil {
		return ccxt.OrderBook{}, fmt.Errorf("binanceth: FetchOrderBook %s: %w", symbol, err)
	}
	now := time.Now().UnixMilli()
	sym := symbol
	return ccxt.OrderBook{
		Bids:      parseOrderBookSide(resp.Bids),
		Asks:      parseOrderBookSide(resp.Asks),
		Symbol:    &sym,
		Timestamp: &now,
	}, nil
}

// LoadMarkets fetches all listed trading pairs from GET /api/v1/exchangeInfo.
func (a *BinanceTHBrokerAdapter) LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	info, err := a.BinanceTHExchange.fetchExchangeInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("binanceth: LoadMarkets: %w", err)
	}
	out := make(map[string]ccxt.MarketInterface, len(info.Symbols))
	for _, s := range info.Symbols {
		ccxtSym := s.BaseAsset + "/" + s.QuoteAsset
		id := s.Symbol
		base := s.BaseAsset
		quote := s.QuoteAsset
		active := s.Status == "TRADING"
		spotTrue := true
		out[ccxtSym] = ccxt.MarketInterface{
			Info:          map[string]interface{}{"symbol": s.Symbol, "status": s.Status},
			Id:            &id,
			Symbol:        &ccxtSym,
			BaseCurrency:  &base,
			QuoteCurrency: &quote,
			Active:        &active,
			Spot:          &spotTrue,
		}
	}
	return out, nil
}

// --- broker.TradingProvider ---

func (a *BinanceTHBrokerAdapter) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	o, err := a.BinanceTHExchange.createOrder(ctx, sym, side, orderType, amount, price)
	if err != nil {
		return ccxt.Order{}, err
	}
	return a.orderToCCXT(o), nil
}

func (a *BinanceTHBrokerAdapter) CancelOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	o, err := a.BinanceTHExchange.cancelOrder(ctx, sym, id)
	if err != nil {
		return ccxt.Order{}, err
	}
	return a.orderToCCXT(o), nil
}

func (a *BinanceTHBrokerAdapter) FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	o, err := a.BinanceTHExchange.fetchOrder(ctx, sym, id)
	if err != nil {
		return ccxt.Order{}, err
	}
	return a.orderToCCXT(o), nil
}

func (a *BinanceTHBrokerAdapter) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	orders, err := a.BinanceTHExchange.fetchOpenOrders(ctx, sym)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, len(orders))
	for i, o := range orders {
		out[i] = a.orderToCCXT(o)
	}
	return out, nil
}

func (a *BinanceTHBrokerAdapter) FetchClosedOrders(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	orders, err := a.BinanceTHExchange.fetchAllOrders(ctx, sym, since, limit)
	if err != nil {
		return nil, err
	}
	// Filter to closed/canceled orders only.
	out := make([]ccxt.Order, 0, len(orders))
	for _, o := range orders {
		if o.Status == "FILLED" || o.Status == "CANCELED" || o.Status == "EXPIRED" || o.Status == "REJECTED" {
			out = append(out, a.orderToCCXT(o))
		}
	}
	return out, nil
}

func (a *BinanceTHBrokerAdapter) FetchMyTrades(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Trade, error) {
	sym := normalizeSymbol(symbol)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	trades, err := a.BinanceTHExchange.fetchUserTrades(ctx, sym, since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Trade, len(trades))
	for i, t := range trades {
		out[i] = a.tradeToCCXT(t)
	}
	return out, nil
}

// ---- Conversion helpers ----

func (a *BinanceTHBrokerAdapter) orderToCCXT(o orderResponse) ccxt.Order {
	id := strconv.FormatInt(o.OrderID, 10)
	sym := o.Symbol
	price, _ := strconv.ParseFloat(o.Price, 64)
	amount, _ := strconv.ParseFloat(o.OrigQty, 64)
	filled, _ := strconv.ParseFloat(o.ExecutedQty, 64)
	remaining := amount - filled
	cost, _ := strconv.ParseFloat(o.CummulativeQuoteQty, 64)
	side := strings.ToLower(o.Side)
	typ := strings.ToLower(o.Type)
	status := mapOrderStatus(o.Status)
	ts := o.Time

	return ccxt.Order{
		Id:        &id,
		Symbol:    &sym,
		Price:     &price,
		Amount:    &amount,
		Filled:    &filled,
		Remaining: &remaining,
		Cost:      &cost,
		Side:      &side,
		Type:      &typ,
		Status:    &status,
		Timestamp: &ts,
	}
}

func (a *BinanceTHBrokerAdapter) tradeToCCXT(t tradeResponse) ccxt.Trade {
	id := strconv.FormatInt(t.ID, 10)
	orderID := strconv.FormatInt(t.OrderID, 10)
	sym := t.Symbol
	price, _ := strconv.ParseFloat(t.Price, 64)
	amount, _ := strconv.ParseFloat(t.Qty, 64)
	cost := price * amount
	fee, _ := strconv.ParseFloat(t.Commission, 64)
	feeAsset := t.CommissionAsset
	var side string
	if t.IsBuyer {
		side = "buy"
	} else {
		side = "sell"
	}
	ts := t.Time

	return ccxt.Trade{
		Id:        &id,
		Order:     &orderID,
		Symbol:    &sym,
		Side:      &side,
		Price:     &price,
		Amount:    &amount,
		Cost:      &cost,
		Timestamp: &ts,
		Fee:       ccxt.Fee{Cost: &fee},
		Info:      map[string]interface{}{"commissionAsset": feeAsset},
	}
}

func mapOrderStatus(s string) string {
	switch s {
	case "NEW", "PARTIALLY_FILLED":
		return "open"
	case "FILLED":
		return "closed"
	case "CANCELED", "EXPIRED", "REJECTED":
		return "canceled"
	default:
		return strings.ToLower(s)
	}
}

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.BinanceTH.ResolveAccount("")
		if !ok {
			return newBrokerAdapter(config.ExchangeAccount{})
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
