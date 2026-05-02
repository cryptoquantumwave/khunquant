package bitkub

// BitkubBrokerAdapter implements broker.PortfolioProvider, broker.MarketDataProvider,
// and broker.TradingProvider using the Bitkub v3 REST API.
// TransferProvider is not implemented (Bitkub has no internal transfer API).

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

// normalizeSymbol converts unified CCXT format (e.g. "BTC/THB") to the
// Bitkub API format (e.g. "BTC_THB"). Already-normalized symbols are
// returned unchanged, so callers can safely pass either form.
func normalizeSymbol(symbol string) string {
	return strings.ReplaceAll(symbol, "/", "_")
}

// BitkubBrokerAdapter wraps BitkubExchange with the broker.Provider hierarchy.
type BitkubBrokerAdapter struct {
	*BitkubExchange
}

func newBrokerAdapter(creds config.ExchangeAccount) (*BitkubBrokerAdapter, error) {
	ex, err := NewBitkubExchange(creds)
	if err != nil {
		return nil, err
	}
	if creds.APIKey.String() != "" {
		logger.RegisterSecret(creds.APIKey.String())
	}
	if creds.Secret.String() != "" {
		logger.RegisterSecret(creds.Secret.String())
	}
	return &BitkubBrokerAdapter{BitkubExchange: ex}, nil
}

// --- broker.Provider ---

func (a *BitkubBrokerAdapter) ID() string { return Name }

func (a *BitkubBrokerAdapter) Category() broker.AssetCategory { return broker.CategoryCrypto }

func (a *BitkubBrokerAdapter) GetMarketStatus(ctx context.Context, symbol string) (broker.MarketStatus, error) {
	tickers, err := a.fetchTickers(ctx)
	if err != nil {
		return broker.MarketUnknown, nil
	}
	if _, ok := tickers[normalizeSymbol(symbol)]; ok {
		return broker.MarketOpen, nil
	}
	return broker.MarketUnknown, nil
}

// --- broker.PortfolioProvider ---

func (a *BitkubBrokerAdapter) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	bals, err := a.BitkubExchange.GetBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]broker.Balance, len(bals))
	for i, b := range bals {
		out[i] = broker.Balance{Asset: b.Asset, Free: b.Free, Locked: b.Locked}
	}
	return out, nil
}

func (a *BitkubBrokerAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	bals, err := a.BitkubExchange.GetWalletBalances(ctx, walletType)
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

func (a *BitkubBrokerAdapter) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	return a.BitkubExchange.FetchPrice(ctx, asset, quote)
}

func (a *BitkubBrokerAdapter) SupportedWalletTypes() []string {
	return a.BitkubExchange.SupportedWalletTypes()
}

// --- broker.MarketDataProvider ---

func (a *BitkubBrokerAdapter) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	rich, err := a.fetchRichTickers(ctx)
	if err != nil {
		return ccxt.Ticker{}, fmt.Errorf("bitkub: FetchTicker: %w", err)
	}
	e, ok := rich[normalizeSymbol(symbol)]
	if !ok {
		return ccxt.Ticker{}, fmt.Errorf("bitkub: symbol %q not found", symbol)
	}
	return tickerEntryToCCXT(symbol, e), nil
}

func (a *BitkubBrokerAdapter) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	rich, err := a.fetchRichTickers(ctx)
	if err != nil {
		return nil, fmt.Errorf("bitkub: FetchTickers: %w", err)
	}

	// Build normalized filter set so callers can pass either "BTC/THB" or "BTC_THB".
	filterNorm := make(map[string]string, len(symbols)) // normalized → original
	for _, s := range symbols {
		filterNorm[normalizeSymbol(s)] = s
	}

	out := make(map[string]ccxt.Ticker, len(rich))
	for sym, e := range rich {
		if len(symbols) > 0 && filterNorm[sym] == "" {
			continue
		}
		// Use the caller's original key if available, otherwise keep API key.
		outKey := sym
		if orig, ok := filterNorm[sym]; ok && orig != "" {
			outKey = orig
		}
		out[outKey] = tickerEntryToCCXT(outKey, e)
	}
	return out, nil
}

// FetchOHLCV fetches OHLCV bars via the Bitkub TradingView history endpoint.
func (a *BitkubBrokerAdapter) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	tv, err := a.BitkubExchange.fetchTradingViewHistory(ctx, symbol, timeframe, since, limit)
	if err != nil {
		return nil, fmt.Errorf("bitkub: FetchOHLCV: %w", err)
	}
	if tv.Status != "ok" || len(tv.T) == 0 {
		return nil, fmt.Errorf("bitkub: FetchOHLCV: no data returned for %s", symbol)
	}
	out := make([]ccxt.OHLCV, len(tv.T))
	for i, ts := range tv.T {
		tsMs := ts * 1000
		out[i] = ccxt.OHLCV{
			Timestamp: tsMs,
			Open:      tv.O[i],
			High:      tv.H[i],
			Low:       tv.L[i],
			Close:     tv.C[i],
			Volume:    tv.V[i],
		}
	}
	return out, nil
}

// FetchOrderBook returns the order book using GET /api/v3/market/depth.
func (a *BitkubBrokerAdapter) FetchOrderBook(ctx context.Context, symbol string, depth int) (ccxt.OrderBook, error) {
	if depth <= 0 {
		depth = 10
	}
	sym := normalizeSymbol(symbol)
	resp, err := a.fetchDepth(ctx, sym, depth)
	if err != nil {
		return ccxt.OrderBook{}, fmt.Errorf("bitkub: FetchOrderBook %s: %w", symbol, err)
	}
	if resp.Error != 0 {
		return ccxt.OrderBook{}, fmt.Errorf("bitkub: FetchOrderBook error code %d", resp.Error)
	}
	now := time.Now().UnixMilli()
	return ccxt.OrderBook{
		Asks:      resp.Result.Asks,
		Bids:      resp.Result.Bids,
		Symbol:    &symbol,
		Timestamp: &now,
	}, nil
}

// LoadMarkets returns all listed trading pairs using GET /api/v3/market/symbols.
func (a *BitkubBrokerAdapter) LoadMarkets(ctx context.Context) (map[string]ccxt.MarketInterface, error) {
	symbols, err := a.fetchSymbols(ctx)
	if err != nil {
		return nil, fmt.Errorf("bitkub: LoadMarkets: %w", err)
	}

	out := make(map[string]ccxt.MarketInterface, len(symbols))
	for _, s := range symbols {
		ccxtSym := s.BaseAsset + "/" + s.QuoteAsset
		id := s.Symbol
		base := s.BaseAsset
		quote := s.QuoteAsset
		active := s.Status == "active"
		spotTrue := true
		out[ccxtSym] = ccxt.MarketInterface{
			Info:          map[string]interface{}{"symbol": s.Symbol, "name": s.Name, "status": s.Status},
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

// CreateOrder places a limit or market order on Bitkub.
// amount is always in base currency (e.g. BTC). For buy orders, it is converted
// to THB using the provided price, or the current market price for market orders.
func (a *BitkubBrokerAdapter) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)

	var rat float64
	if price != nil {
		rat = *price
	}

	var o bitkubOrder
	var err error

	switch strings.ToLower(side) {
	case "buy":
		// Bitkub place-bid: amt = THB to spend, rat = price per unit
		var thbAmount float64
		if price != nil {
			// Limit buy: convert base amount to THB
			thbAmount = amount * rat
		} else {
			// Market buy: fetch current price to convert base amount → THB
			parts := strings.SplitN(symbol, "/", 2)
			base, quote := parts[0], "THB"
			if len(parts) == 2 {
				quote = parts[1]
			}
			currentPrice, tickErr := a.FetchPrice(ctx, base, quote)
			if tickErr != nil || currentPrice <= 0 {
				return ccxt.Order{}, fmt.Errorf("bitkub: market buy: could not fetch price for %s: %w", symbol, tickErr)
			}
			thbAmount = amount * currentPrice
			// rat stays 0 for market order (as Bitkub requires)
		}
		o, err = a.placeBid(ctx, sym, thbAmount, rat, orderType)
	case "sell":
		// Bitkub place-ask: amt = base currency to sell
		o, err = a.placeAsk(ctx, sym, amount, rat, orderType)
	default:
		return ccxt.Order{}, fmt.Errorf("bitkub: unknown order side %q (must be buy or sell)", side)
	}
	if err != nil {
		return ccxt.Order{}, err
	}
	return a.orderToCCXT(o), nil
}

// CancelOrder cancels an open order. The symbol is required.
// Bitkub requires the order side (buy/sell) to cancel; we try "buy" first,
// then "sell" if that fails with an API error.
func (a *BitkubBrokerAdapter) CancelOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)

	for _, side := range []string{"buy", "sell"} {
		err := a.cancelBitkubOrder(ctx, sym, id, side)
		if err == nil {
			// Fetch the order state to return accurate info.
			orderInfo, _, fetchErr := a.fetchOrderInfo(ctx, sym, id, side)
			if fetchErr != nil {
				// Return a minimal order on fetch failure — cancel succeeded.
				return ccxt.Order{Id: &id}, nil
			}
			orderInfo.St = "cancelled"
			return a.orderToCCXT(orderInfo), nil
		}
	}

	return ccxt.Order{}, fmt.Errorf("bitkub: CancelOrder: failed to cancel order %s on %s", id, sym)
}

// FetchOrder returns a single order's details. Side must be supplied via
// symbol suffix convention "BTC/THB:buy" or defaults to trying both sides.
func (a *BitkubBrokerAdapter) FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	sym := normalizeSymbol(symbol)

	// Try buy side first, then sell.
	o, _, err := a.fetchOrderInfo(ctx, sym, id, "buy")
	if err != nil {
		o, _, err = a.fetchOrderInfo(ctx, sym, id, "sell")
		if err != nil {
			return ccxt.Order{}, fmt.Errorf("bitkub: FetchOrder %s: %w", id, err)
		}
	}
	return a.orderToCCXT(o), nil
}

// FetchOpenOrders returns all open orders for the symbol.
// Bitkub's API requires a specific trading pair — it does not support fetching
// open orders across all symbols in a single call.
func (a *BitkubBrokerAdapter) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	if symbol == "" {
		return nil, fmt.Errorf("bitkub requires a symbol to list open orders; please specify a trading pair (e.g. BTC/THB, STO/THB, ETH/THB)")
	}
	sym := normalizeSymbol(symbol)
	orders, err := a.fetchMyOpenOrders(ctx, sym)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, len(orders))
	for i, o := range orders {
		out[i] = a.orderToCCXT(o)
	}
	return out, nil
}

// FetchClosedOrders returns the order history for the symbol.
// since and limit are supported; since is converted to a page-1 request.
func (a *BitkubBrokerAdapter) FetchClosedOrders(ctx context.Context, symbol string, _ *int64, limit int) ([]ccxt.Order, error) {
	sym := normalizeSymbol(symbol)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	orders, err := a.fetchOrderHistory(ctx, sym, 1, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, len(orders))
	for i, o := range orders {
		out[i] = a.orderToCCXT(o)
	}
	return out, nil
}

// FetchMyTrades maps completed orders from order history to CCXT Trade objects.
// Bitkub does not have a dedicated trade-history endpoint; each filled order
// is treated as a single trade.
func (a *BitkubBrokerAdapter) FetchMyTrades(ctx context.Context, symbol string, _ *int64, limit int) ([]ccxt.Trade, error) {
	sym := normalizeSymbol(symbol)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	orders, err := a.fetchOrderHistory(ctx, sym, 1, limit)
	if err != nil {
		return nil, err
	}

	out := make([]ccxt.Trade, 0, len(orders))
	for _, o := range orders {
		price, _ := strconv.ParseFloat(string(o.Rat), 64)
		rec, _ := strconv.ParseFloat(string(o.Rec), 64)
		feeAmt, _ := strconv.ParseFloat(string(o.Fee), 64)
		tsMs := int64(o.Ts) // already Unix milliseconds

		id := o.ID
		ccxtSym := strings.ReplaceAll(strings.ToUpper(o.Sym), "_", "/")
		side := o.Sd
		typ := o.Typ

		var amount float64
		if o.Sd == "buy" {
			amount = rec // base received
		} else {
			// sell: rec is THB received; amount in base = rec/price
			if price > 0 {
				amount = rec / price
			}
		}

		out = append(out, ccxt.Trade{
			Id:        &id,
			Symbol:    &ccxtSym,
			Side:      &side,
			Type:      &typ,
			Price:     &price,
			Amount:    &amount,
			Timestamp: &tsMs,
			Fee:       ccxt.Fee{Cost: &feeAmt},
			Info:      map[string]interface{}{"id": o.ID, "sym": o.Sym, "rec": o.Rec},
		})
	}
	return out, nil
}

// ---- Helpers ----

// tickerEntryToCCXT converts a Bitkub tickerEntry to a ccxt.Ticker.
func tickerEntryToCCXT(symbol string, e tickerEntry) ccxt.Ticker {
	sym := symbol

	last, _ := strconv.ParseFloat(string(e.Last), 64)
	ask, _ := strconv.ParseFloat(string(e.LowestAsk), 64)
	bid, _ := strconv.ParseFloat(string(e.HighestBid), 64)
	pct, _ := strconv.ParseFloat(string(e.PercentChange), 64)
	baseVol, _ := strconv.ParseFloat(string(e.BaseVolume), 64)
	quoteVol, _ := strconv.ParseFloat(string(e.QuoteVolume), 64)
	high, _ := strconv.ParseFloat(string(e.High24Hr), 64)
	low, _ := strconv.ParseFloat(string(e.Low24Hr), 64)

	now := time.Now().UnixMilli()

	return ccxt.Ticker{
		Symbol:      &sym,
		Last:        &last,
		Ask:         &ask,
		Bid:         &bid,
		Percentage:  &pct,
		BaseVolume:  &baseVol,
		QuoteVolume: &quoteVol,
		High:        &high,
		Low:         &low,
		Timestamp:   &now,
	}
}

func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Bitkub.ResolveAccount("")
		if !ok {
			return newBrokerAdapter(config.ExchangeAccount{})
		}
		return newBrokerAdapter(acc)
	})
	broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Bitkub.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.Bitkub.Accounts {
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
