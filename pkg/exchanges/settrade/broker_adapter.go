package settrade

// SettradeFullAdapter implements broker.PortfolioProvider, broker.MarketDataProvider,
// and broker.TradingProvider using the SETTRADE Open API (SDK v2 endpoints).

import (
	"context"
	"fmt"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// SettradeFullAdapter wraps SettradeClient with the broker.Provider hierarchy.
type SettradeFullAdapter struct {
	client *SettradeClient
	cfg    config.SettradeExchangeAccount
}

func newBrokerAdapter(cfg config.SettradeExchangeAccount) (*SettradeFullAdapter, error) {
	client, err := NewSettradeClient(cfg)
	if err != nil {
		return nil, err
	}
	logger.RegisterSecret(cfg.APIKey.String())
	logger.RegisterSecret(cfg.Secret.String())
	logger.RegisterSecret(cfg.PIN.String())
	return &SettradeFullAdapter{client: client, cfg: cfg}, nil
}

// --- broker.Provider ---

func (a *SettradeFullAdapter) ID() string { return Name }

func (a *SettradeFullAdapter) Category() broker.AssetCategory { return broker.CategoryStock }

// GetMarketStatus checks SET market sessions in ICT (UTC+7).
// Morning session: 10:00–12:30; Afternoon session: 14:00–16:30 (Mon–Fri).
func (a *SettradeFullAdapter) GetMarketStatus(_ context.Context, _ string) (broker.MarketStatus, error) {
	ict := time.FixedZone("ICT", 7*3600)
	now := time.Now().In(ict)

	if now.Weekday() == time.Saturday || now.Weekday() == time.Sunday {
		return broker.MarketClosed, nil
	}

	h, m := now.Hour(), now.Minute()
	totalMin := h*60 + m

	morning := totalMin >= 10*60 && totalMin < 12*60+30
	afternoon := totalMin >= 14*60 && totalMin < 16*60+30
	if morning || afternoon {
		return broker.MarketOpen, nil
	}
	return broker.MarketClosed, nil
}

// --- broker.PortfolioProvider ---

func (a *SettradeFullAdapter) GetBalances(ctx context.Context) ([]broker.Balance, error) {
	info, err := a.client.FetchAccountInfo(ctx)
	if err != nil {
		return nil, err
	}
	return []broker.Balance{
		{Asset: "THB", Free: info.CashBalance, Locked: 0},
	}, nil
}

func (a *SettradeFullAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	switch strings.ToLower(walletType) {
	case "cash", "":
		info, err := a.client.FetchAccountInfo(ctx)
		if err != nil {
			return nil, err
		}
		return []broker.WalletBalance{
			{
				Balance:    broker.Balance{Asset: "THB", Free: info.CashBalance, Locked: 0},
				WalletType: "cash",
				Extra: map[string]string{
					"buying_power": fmt.Sprintf("%g", info.LineAvailable),
					"credit_limit": fmt.Sprintf("%g", info.CreditLimit),
				},
			},
		}, nil

	case "stock":
		portfolio, err := a.client.FetchPortfolio(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]broker.WalletBalance, len(portfolio.PortfolioList))
		for i, item := range portfolio.PortfolioList {
			// Volumes are as returned by the API.
			out[i] = broker.WalletBalance{
				Balance:    broker.Balance{Asset: item.Symbol, Free: item.ActualVolume, Locked: item.CurrentVolume - item.ActualVolume},
				WalletType: "stock",
				Extra: map[string]string{
					"avg_cost":       fmt.Sprintf("%g", item.AveragePrice),
					"market_price":   fmt.Sprintf("%g", item.MarketPrice),
					"market_value":   fmt.Sprintf("%g", item.MarketValue),
					"unrealized_pl":  fmt.Sprintf("%g", item.Profit),
					"percent_profit": fmt.Sprintf("%g", item.PercentProfit),
				},
			}
		}
		return out, nil

	case "all":
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
		return nil, fmt.Errorf("settrade: unsupported wallet type %q (use \"cash\", \"stock\", or \"all\")", walletType)
	}
}

func (a *SettradeFullAdapter) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	if !strings.EqualFold(quote, "THB") && quote != "" {
		return 0, fmt.Errorf("settrade: only THB quotes are supported (got %q)", quote)
	}
	// THB is the quote currency itself — return (0, nil) to signal 1:1.
	if strings.EqualFold(asset, "THB") {
		return 0, nil
	}
	resp, err := a.client.FetchQuote(ctx, strings.ToUpper(asset))
	if err != nil {
		return 0, err
	}
	return resp.Last, nil
}

func (a *SettradeFullAdapter) SupportedWalletTypes() []string {
	return []string{"cash", "stock"}
}

// --- broker.MarketDataProvider ---

func (a *SettradeFullAdapter) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	sym := toSetSymbol(symbol)
	resp, err := a.client.FetchQuote(ctx, sym)
	if err != nil {
		return ccxt.Ticker{}, fmt.Errorf("settrade: FetchTicker %s: %w", symbol, err)
	}
	return quoteToCCXT(symbol, resp), nil
}

func (a *SettradeFullAdapter) FetchTickers(ctx context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
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

// settradeTimeframe maps CCXT unified timeframes to Settrade interval strings.
var settradeTimeframe = map[string]string{
	"1m": "1m", "5m": "5m", "15m": "15m",
	"1h": "60m", "4h": "240m",
	"1d": "1d", "1w": "1w",
}

// FetchOHLCV returns candlestick data using the techchart market data endpoint.
func (a *SettradeFullAdapter) FetchOHLCV(ctx context.Context, symbol, timeframe string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	interval, ok := settradeTimeframe[timeframe]
	if !ok {
		interval = "1d"
	}

	sym := toSetSymbol(symbol)

	var start string
	if since != nil {
		start = time.UnixMilli(*since).UTC().Format("2006-01-02T15:04")
	}

	bars, err := a.client.FetchCandlestick(ctx, sym, interval, limit, start, "", false)
	if err != nil {
		return nil, fmt.Errorf("settrade: FetchOHLCV %s: %w", symbol, err)
	}

	out := make([]ccxt.OHLCV, len(bars))
	for i, b := range bars {
		ts := b.Time * 1000 // seconds → milliseconds
		out[i] = ccxt.OHLCV{
			Timestamp: ts,
			Open:      b.Open,
			High:      b.High,
			Low:       b.Low,
			Close:     b.Close,
			Volume:    b.Volume,
		}
	}
	return out, nil
}

// FetchOrderBook is not supported via the SETTRADE Open API.
func (a *SettradeFullAdapter) FetchOrderBook(_ context.Context, symbol string, _ int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, fmt.Errorf("settrade: order book is not available via the Open API (symbol: %s)", symbol)
}

// LoadMarkets is not supported via the SETTRADE Open API.
func (a *SettradeFullAdapter) LoadMarkets(_ context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, fmt.Errorf("settrade: LoadMarkets is not supported via the Open API")
}

// --- broker.TradingProvider (Equity) ---

// CreateOrder places a limit or market order on SET.
// symbol: CCXT format "PTT/THB" or raw "PTT".
// orderType: "limit" → Limit, "market" → ATO.
// amount: number of shares/units to trade.
// price: required for limit orders; ignored for market orders.
func (a *SettradeFullAdapter) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, _ map[string]interface{}) (ccxt.Order, error) {
	sym := toSetSymbol(symbol)

	var priceType string
	var orderPrice float64
	switch strings.ToLower(orderType) {
	case "limit":
		priceType = "Limit"
		if price == nil {
			return ccxt.Order{}, fmt.Errorf("settrade: price is required for limit orders")
		}
		orderPrice = *price
	case "market":
		priceType = "ATO"
	default:
		return ccxt.Order{}, fmt.Errorf("settrade: unsupported order type %q (use \"limit\" or \"market\")", orderType)
	}

	var setSide string
	switch strings.ToLower(side) {
	case "buy":
		setSide = "Buy"
	case "sell":
		setSide = "Sell"
	default:
		return ccxt.Order{}, fmt.Errorf("settrade: unknown order side %q (must be buy or sell)", side)
	}

	o, err := a.client.CreateEQOrder(ctx, sym, setSide, priceType, int(amount), orderPrice)
	if err != nil {
		return ccxt.Order{}, err
	}
	return settradeOrderToCCXT(o), nil
}

func (a *SettradeFullAdapter) CancelOrder(ctx context.Context, id, _ string) (ccxt.Order, error) {
	o, err := a.client.CancelEQOrder(ctx, id)
	if err != nil {
		return ccxt.Order{}, fmt.Errorf("settrade: CancelOrder %s: %w", id, err)
	}
	return settradeOrderToCCXT(o), nil
}

func (a *SettradeFullAdapter) FetchOrder(ctx context.Context, id, _ string) (ccxt.Order, error) {
	o, err := a.client.FetchEQOrder(ctx, id)
	if err != nil {
		return ccxt.Order{}, fmt.Errorf("settrade: FetchOrder %s: %w", id, err)
	}
	return settradeOrderToCCXT(o), nil
}

func (a *SettradeFullAdapter) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	orders, err := a.client.FetchOpenEQOrders(ctx, toSetSymbol(symbol))
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, len(orders))
	for i, o := range orders {
		out[i] = settradeOrderToCCXT(o)
	}
	return out, nil
}

func (a *SettradeFullAdapter) FetchClosedOrders(ctx context.Context, symbol string, _ *int64, limit int) ([]ccxt.Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	orders, err := a.client.FetchClosedEQOrders(ctx, toSetSymbol(symbol), limit)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Order, len(orders))
	for i, o := range orders {
		out[i] = settradeOrderToCCXT(o)
	}
	return out, nil
}

func (a *SettradeFullAdapter) FetchMyTrades(ctx context.Context, symbol string, _ *int64, limit int) ([]ccxt.Trade, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	trades, err := a.client.FetchTrades(ctx, toSetSymbol(symbol), limit)
	if err != nil {
		return nil, err
	}
	out := make([]ccxt.Trade, len(trades))
	for i, tr := range trades {
		id := tr.TradeID
		ccxtSym := tr.Symbol + "/THB"
		side := strings.ToLower(tr.Side)
		typ := "limit"
		shares := tr.Volume
		out[i] = ccxt.Trade{
			Id:     &id,
			Symbol: &ccxtSym,
			Side:   &side,
			Type:   &typ,
			Price:  &tr.Price,
			Amount: &shares,
			Info:   map[string]interface{}{"order_no": tr.OrderNo, "trade_date": tr.TradeDate},
		}
	}
	return out, nil
}

// --- Helpers ---

func toSetSymbol(symbol string) string {
	if idx := strings.Index(symbol, "/"); idx != -1 {
		return strings.ToUpper(symbol[:idx])
	}
	return strings.ToUpper(symbol)
}

func quoteToCCXT(symbol string, resp quoteResponse) ccxt.Ticker {
	sym := symbol
	now := time.Now().UnixMilli()
	return ccxt.Ticker{
		Symbol:     &sym,
		Last:       &resp.Last,
		High:       &resp.High,
		Low:        &resp.Low,
		BaseVolume: &resp.TotalVolume,
		Percentage: &resp.PercentChange,
		Timestamp:  &now,
	}
}

func settradeOrderToCCXT(o settradeOrder) ccxt.Order {
	id := o.OrderNo
	sym := o.Symbol + "/THB"
	side := strings.ToLower(o.Side)

	var typ string
	switch o.PriceType {
	case "Limit":
		typ = "limit"
	case "ATO", "ATC":
		typ = "market"
	default:
		typ = strings.ToLower(o.PriceType)
	}

	var status string
	switch strings.ToUpper(o.Status) {
	// Settrade SEOS open/queuing states
	case "A", "P", "SC", "S", "SX", "WC", "OPEN", "PENDING":
		status = "open"
	// Fully matched
	case "M", "MATCHED", "FILLED":
		status = "closed"
	// Cancelled states
	case "C", "CM", "CS", "CX", "CANCELLED", "CANCELED":
		status = "canceled"
	case "R", "REJECTED":
		status = "rejected"
	default:
		status = strings.ToLower(o.Status)
	}

	// API returns vol/matched/balance in shares (same convention as portfolioItem).
	totalShares := o.Volume
	filledShares := o.FilledVol
	remaining := o.Balance
	return ccxt.Order{
		Id:        &id,
		Symbol:    &sym,
		Side:      &side,
		Type:      &typ,
		Status:    &status,
		Price:     &o.Price,
		Amount:    &totalShares,
		Filled:    &filledShares,
		Remaining: &remaining,
		Datetime:  &o.EntryDate,
		Info:      map[string]interface{}{"order_no": o.OrderNo, "symbol": o.Symbol},
	}
}

// --- init ---

func init() {
	broker.RegisterFactory(Name, func(cfg *config.Config) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Settrade.ResolveAccount("")
		if !ok {
			return nil, fmt.Errorf("%s: no accounts configured", Name)
		}
		return newBrokerAdapter(acc)
	})
	broker.RegisterAccountFactory(Name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		acc, ok := cfg.Exchanges.Settrade.ResolveAccount(accountName)
		if !ok {
			var names []string
			for i, a := range cfg.Exchanges.Settrade.Accounts {
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
