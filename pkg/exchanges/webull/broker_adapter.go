package webull

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
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
	balResp, err := a.client.FetchBalance(ctx)
	if err != nil {
		return nil, err
	}

	// Extract USD cash from the first currency asset (typically USD)
	result := make([]broker.Balance, 0)
	if len(balResp.AccountCurrencyAssets) > 0 {
		// Emit one entry per currency with cash balance
		for _, asset := range balResp.AccountCurrencyAssets {
			cashBal := parseFloat(asset.CashBalance)
			if cashBal > 0 {
				result = append(result, broker.Balance{
					Asset:  asset.Currency,
					Free:   cashBal,
					Locked: 0,
				})
			}
		}
	}
	return result, nil
}

func (a *webullAdapter) GetWalletBalances(ctx context.Context, walletType string) ([]broker.WalletBalance, error) {
	switch strings.ToLower(walletType) {
	case "cash", "":
		// Cash wallet includes USD and other liquid currency balances
		balResp, err := a.client.FetchBalance(ctx)
		if err != nil {
			return nil, err
		}

		result := make([]broker.WalletBalance, 0)
		if len(balResp.AccountCurrencyAssets) > 0 {
			for _, asset := range balResp.AccountCurrencyAssets {
				cashBal := parseFloat(asset.CashBalance)
				if cashBal > 0 {
					extra := map[string]string{
						"buying_power":          asset.BuyingPower,
						"market_value":          asset.MarketValue,
						"net_liquidation_value": asset.NetLiquidationValue,
					}
					result = append(result, broker.WalletBalance{
						Balance: broker.Balance{
							Asset:  asset.Currency,
							Free:   cashBal,
							Locked: 0,
						},
						WalletType: "cash",
						Extra:      extra,
					})
				}
			}
		}
		return result, nil

	case "stock":
		// Stock wallet includes equity holdings
		positions, err := a.client.FetchPositions(ctx)
		if err != nil {
			return nil, err
		}

		result := make([]broker.WalletBalance, 0, len(positions))
		for _, p := range positions {
			qty := parseFloat(p.Quantity)
			if qty > 0 {
				extra := map[string]string{
					"avg_cost":      p.CostPrice,
					"current_price": p.LastPrice,
					"market_value":  p.MarketValue,
					"unrealized_pl": p.UnrealizedProfitLoss,
					"percent_pnl":   p.UnrealizedProfitLossRate,
				}
				result = append(result, broker.WalletBalance{
					Balance: broker.Balance{
						Asset:  p.Symbol,
						Free:   qty,
						Locked: 0,
					},
					WalletType: "stock",
					Extra:      extra,
				})
			}
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

	snapshots, err := a.client.FetchSnapshot(ctx, []string{strings.ToUpper(asset)})
	if err != nil {
		return 0, err
	}

	if len(snapshots) == 0 {
		return 0, fmt.Errorf("webull: no snapshot data for %s", asset)
	}

	price := parseFloat(snapshots[0].Price)
	// A Webull stock price is never 1:1 self-pair, so non-positive price
	// must be an error (halted symbol, unavailable data, response mismatch).
	if price <= 0 {
		return 0, fmt.Errorf("webull: invalid price for %s (got %v)", asset, price)
	}
	return price, nil
}

func (a *webullAdapter) SupportedWalletTypes() []string {
	// TODO(webull-multiasset): add a "crypto" wallet when Webull crypto support lands.
	return []string{"cash", "stock"}
}

// --- broker.MarketDataProvider ---

// webullTimeframe maps CCXT unified timeframes to Webull timespan strings.
// Webull uses: M1, M5, M15, M30, M60, M120, M240, D, W, M, Y
var webullTimeframe = map[string]string{
	"1m":  "M1",
	"5m":  "M5",
	"15m": "M15",
	"30m": "M30",
	"1h":  "M60",
	"2h":  "M120",
	"4h":  "M240",
	"1d":  "D",
	"1w":  "W",
	"1M":  "M",
}

func (a *webullAdapter) FetchTicker(ctx context.Context, symbol string) (ccxt.Ticker, error) {
	sym := toWebullSymbol(symbol)
	snapshots, err := a.client.FetchSnapshot(ctx, []string{sym})
	if err != nil {
		return ccxt.Ticker{}, fmt.Errorf("webull: FetchTicker %s: %w", symbol, err)
	}

	if len(snapshots) == 0 {
		return ccxt.Ticker{}, fmt.Errorf("webull: no snapshot for %s", symbol)
	}

	return snapshotToTicker(symbol, &snapshots[0]), nil
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

	timespan, ok := webullTimeframe[timeframe]
	if !ok {
		timespan = "D"
	}

	sym := toWebullSymbol(symbol)

	bars, err := a.client.FetchBars(ctx, sym, timespan, limit)
	if err != nil {
		return nil, fmt.Errorf("webull: FetchOHLCV %s: %w", symbol, err)
	}

	// Bars come newest-first from Webull; reverse to oldest-first for CCXT convention
	out := make([]ccxt.OHLCV, len(bars))
	for i, b := range bars {
		// Parse bar time (ISO8601 format: "2026-07-09T04:00:00.000+0000")
		barTime, err := time.Parse("2006-01-02T15:04:05.000-0700", b.Time)
		if err != nil {
			// If parsing fails, use a fallback (current time)
			barTime = time.Now()
		}

		o := parseFloat(b.Open)
		h := parseFloat(b.High)
		l := parseFloat(b.Low)
		c := parseFloat(b.Close)
		v := parseFloat(b.Volume)

		out[i] = ccxt.OHLCV{
			Timestamp: barTime.UnixMilli(),
			Open:      o,
			High:      h,
			Low:       l,
			Close:     c,
			Volume:    v,
		}
	}

	// Reverse to oldest-first
	for i := 0; i < len(out)/2; i++ {
		out[i], out[len(out)-1-i] = out[len(out)-1-i], out[i]
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

// --- broker.TradingProvider (Equity) ---
//
// TODO(webull-multiasset): trading is intentionally scoped to US equities for now.
// Webull's OpenAPI also supports CRYPTO, OPTION, FUTURES, and EVENT contracts
// (see docs/webull-api-spec.md). To extend:
//   - crypto:  reuse this order path with instrument_type=CRYPTO; add a crypto
//     wallet type + market-data category, and confirm the order_type/entrust rules.
//   - options: instrument_type=OPTION requires the `legs` array (strike, expiry,
//     option_type, option_category) and option_strategy on the order.
//   - futures: instrument_type=FUTURES uses contract symbols + margin; wire a
//     FuturesProvider rather than overloading equity CreateOrder.
// Each asset class has distinct order_type/time_in_force enums — parameterize
// the hardcoded EQUITY/US/QTY/CORE fields below rather than branching inline.

// CreateOrder submits a new equity order (EQUITIES ONLY).
// symbol: CCXT format "AAPL/USD" or raw "AAPL".
// orderType: "limit" (requires price), "market", "stop_loss" (requires price as stop_price).
//
//	"take_profit" returns a clear error (not supported for equities).
//
// side: "buy" or "sell".
// amount: number of shares (supports decimals for fractional shares).
//
//	Fractional + LIMIT returns error (Webull rejects fractional limit orders).
//	Fractional + STOP_LOSS returns error (undefined behavior).
//
// price: required for limit and stop_loss; used as limit_price or stop_price respectively.
// params: optional overrides for time_in_force (DAY, GTC only for stocks).
func (a *webullAdapter) CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	sym := toWebullSymbol(symbol)

	// Validate and map orderType
	var webullOrderType string
	switch strings.ToLower(orderType) {
	case "market":
		webullOrderType = "MARKET"
	case "limit":
		if price == nil {
			return ccxt.Order{}, fmt.Errorf("webull: price is required for limit orders")
		}
		webullOrderType = "LIMIT"
	case "stop_loss":
		if price == nil {
			return ccxt.Order{}, fmt.Errorf("webull: price (stop_price) is required for stop_loss orders")
		}
		webullOrderType = "STOP_LOSS"
	case "take_profit":
		return ccxt.Order{}, fmt.Errorf("webull: take_profit orders are not supported for equities")
	default:
		return ccxt.Order{}, fmt.Errorf("webull: unsupported order type %q (use \"market\", \"limit\", or \"stop_loss\")", orderType)
	}

	// Validate side
	var webullSide string
	switch strings.ToLower(side) {
	case "buy":
		webullSide = "BUY"
	case "sell":
		webullSide = "SELL"
	default:
		return ccxt.Order{}, fmt.Errorf("webull: unknown order side %q (must be buy or sell)", side)
	}

	// Check for fractional orders
	isFractional := amount != float64(int64(amount))
	if isFractional {
		if webullOrderType == "LIMIT" {
			return ccxt.Order{}, fmt.Errorf("webull: fractional shares are not supported for LIMIT orders (amount: %g)", amount)
		}
		if webullOrderType == "STOP_LOSS" {
			return ccxt.Order{}, fmt.Errorf("webull: fractional shares are not supported for STOP_LOSS orders (amount: %g)", amount)
		}
		// Fractional MARKET is OK; don't force conversion here.
	}

	// Extract time_in_force from params (default: DAY)
	timeInForce := "DAY"
	if tifVal, ok := params["time_in_force"]; ok {
		if tifStr, ok := tifVal.(string); ok {
			timeInForce = tifStr
		}
	}
	// Validate TIF for stocks (only DAY or GTC allowed)
	timeInForceUpper := strings.ToUpper(timeInForce)
	if timeInForceUpper != "DAY" && timeInForceUpper != "GTC" {
		return ccxt.Order{}, fmt.Errorf("webull: unsupported time_in_force %q for equities (use \"DAY\" or \"GTC\")", timeInForce)
	}

	// Generate unique client_order_id (≤32 chars)
	clientOrderID, err := generateClientOrderID()
	if err != nil {
		return ccxt.Order{}, fmt.Errorf("webull: generate client_order_id: %w", err)
	}

	// Build NewOrder
	// TODO(webull-multiasset): InstrumentType/Market/EntryType are pinned to
	// equities. Parameterize these (and the order_type mapping above) when adding
	// crypto/option/futures support — see the roadmap note on TradingProvider.
	order := NewOrder{
		ClientOrderID:         clientOrderID,
		ComboType:             "NORMAL",
		EntryType:             "QTY",
		InstrumentType:        "EQUITY",
		Market:                "US",
		OrderType:             webullOrderType,
		Side:                  webullSide,
		Symbol:                sym,
		TimeInForce:           timeInForceUpper,
		Quantity:              strconv.FormatFloat(amount, 'f', -1, 64),
		SupportTradingSession: "CORE", // REQUIRED for equity orders
	}

	// Set price fields based on order type
	if webullOrderType == "LIMIT" && price != nil {
		order.LimitPrice = strconv.FormatFloat(*price, 'f', -1, 64)
	} else if webullOrderType == "STOP_LOSS" && price != nil {
		order.StopPrice = strconv.FormatFloat(*price, 'f', -1, 64)
	}

	// Execute PlaceOrder
	req := PlaceOrderRequest{
		AccountID: a.client.accountID,
		NewOrders: []NewOrder{order},
	}
	resp, err := a.client.PlaceOrder(ctx, req)
	if err != nil {
		return ccxt.Order{}, err
	}

	// Return ccxt.Order with Id = client_order_id, order_id in Info
	ccxtSym := symbol
	if !strings.Contains(ccxtSym, "/") {
		ccxtSym = sym + "/USD"
	}
	ccxtSide := strings.ToLower(side)
	ccxtType := strings.ToLower(orderType)
	ccxtStatus := "open"

	return ccxt.Order{
		Id:     &resp.ClientOrderID,
		Symbol: &ccxtSym,
		Side:   &ccxtSide,
		Type:   &ccxtType,
		Amount: &amount,
		Price:  price,
		Status: &ccxtStatus,
		Info: map[string]interface{}{
			"order_id": resp.OrderID,
		},
	}, nil
}

// CancelOrder cancels an open order by ID (client_order_id).
func (a *webullAdapter) CancelOrder(ctx context.Context, id, _ string) (ccxt.Order, error) {
	req := CancelOrderRequest{
		AccountID:     a.client.accountID,
		ClientOrderID: id,
	}
	_, err := a.client.CancelOrder(ctx, req)
	if err != nil {
		return ccxt.Order{}, fmt.Errorf("webull: CancelOrder %s: %w", id, err)
	}

	// Return basic order with Id and status=canceled
	status := "canceled"
	return ccxt.Order{
		Id:     &id,
		Status: &status,
	}, nil
}

// FetchOrder retrieves a single order by client_order_id.
func (a *webullAdapter) FetchOrder(ctx context.Context, id, symbol string) (ccxt.Order, error) {
	combo, err := a.client.FetchOrderDetail(ctx, id)
	if err != nil {
		return ccxt.Order{}, fmt.Errorf("webull: FetchOrder %s: %w", id, err)
	}

	// Guard against empty orders array
	if len(combo.Orders) == 0 {
		return ccxt.Order{}, fmt.Errorf("webull: FetchOrder %s: no orders in response", id)
	}

	// Flatten first order from combo
	return orderItemToCCXT(symbol, &combo.Orders[0]), nil
}

// FetchOpenOrders returns all open orders, optionally filtered by symbol.
func (a *webullAdapter) FetchOpenOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	combos, err := a.client.FetchOpenOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("webull: FetchOpenOrders: %w", err)
	}

	var result []ccxt.Order
	symbolFilter := toWebullSymbol(symbol) // "" → "", "AAPL/USD" → "AAPL"

	for _, combo := range combos {
		for _, item := range combo.Orders {
			// Filter by symbol if specified
			if symbolFilter != "" && item.Symbol != symbolFilter {
				continue
			}
			result = append(result, orderItemToCCXT(symbol, &item))
		}
	}

	return result, nil
}

// FetchClosedOrders returns closed/filled orders, optionally filtered by symbol.
// since: optional Unix milliseconds timestamp; if provided, derives start_date.
// limit: max number of orders to return (0 = no limit).
func (a *webullAdapter) FetchClosedOrders(ctx context.Context, symbol string, since *int64, limit int) ([]ccxt.Order, error) {
	// Derive start_date from since if provided (format yyyy-MM-dd)
	var startDate string
	if since != nil {
		sinceTime := time.UnixMilli(*since).UTC()
		startDate = sinceTime.Format("2006-01-02")
	}

	combos, err := a.client.FetchOrderHistory(ctx, startDate, "")
	if err != nil {
		return nil, fmt.Errorf("webull: FetchClosedOrders: %w", err)
	}

	var result []ccxt.Order
	symbolFilter := toWebullSymbol(symbol)

	for _, combo := range combos {
		for _, item := range combo.Orders {
			// Only include FILLED or CANCELLED orders
			status := strings.ToUpper(item.Status)
			if status != "FILLED" && status != "CANCELLED" {
				continue
			}

			// Filter by symbol if specified
			if symbolFilter != "" && item.Symbol != symbolFilter {
				continue
			}

			result = append(result, orderItemToCCXT(symbol, &item))

			// Respect limit
			if limit > 0 && len(result) >= limit {
				return result, nil
			}
		}
	}

	return result, nil
}

// FetchMyTrades returns personal trade history.
// NOT SUPPORTED via Webull OpenAPI for equities.
func (a *webullAdapter) FetchMyTrades(_ context.Context, _ string, _ *int64, _ int) ([]ccxt.Trade, error) {
	return nil, fmt.Errorf("webull: FetchMyTrades is not available via the OpenAPI (equities v1)")
}

// --- Helpers ---

// generateClientOrderID generates a unique client_order_id (≤32 chars).
// Format: "kq" + 30 hex chars from crypto/rand = 32 chars total.
func generateClientOrderID() (string, error) {
	buf := make([]byte, 15) // 15 bytes → 30 hex chars
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "kq" + fmt.Sprintf("%x", buf), nil
}

// orderItemToCCXT converts a Webull OrderItem to ccxt.Order.
func orderItemToCCXT(symbol string, item *OrderItem) ccxt.Order {
	id := item.ClientOrderID
	sym := symbol
	if !strings.Contains(sym, "/") {
		sym = item.Symbol + "/USD"
	}
	side := strings.ToLower(item.Side)

	// Map order type
	var typ string
	switch item.OrderType {
	case "MARKET", "MARKET_ON_OPEN", "MARKET_ON_CLOSE":
		typ = "market"
	case "LIMIT", "LIMIT_ON_OPEN":
		typ = "limit"
	case "STOP_LOSS", "STOP_LOSS_LIMIT", "TRAILING_STOP_LOSS":
		typ = "stop_loss"
	default:
		typ = strings.ToLower(item.OrderType)
	}

	// Map status
	var status string
	statusUpper := strings.ToUpper(item.Status)
	switch statusUpper {
	case "FILLED":
		status = "closed"
	case "CANCELLED":
		status = "canceled"
	case "PENDING", "SUBMITTED", "PARTIAL_FILLED":
		status = "open"
	case "FAILED", "REJECTED":
		status = "rejected"
	default:
		status = strings.ToLower(item.Status)
	}

	// Parse numeric fields
	totalQty := parseFloat(item.TotalQuantity)
	filledQty := parseFloat(item.FilledQuantity)
	limitPrice := parseFloat(item.LimitPrice)

	// Remaining = total - filled
	remaining := totalQty - filledQty

	return ccxt.Order{
		Id:        &id,
		Symbol:    &sym,
		Side:      &side,
		Type:      &typ,
		Status:    &status,
		Amount:    &totalQty,
		Filled:    &filledQty,
		Remaining: &remaining,
		Price:     &limitPrice,
		Info: map[string]interface{}{
			"order_id":        item.OrderID,
			"client_order_id": item.ClientOrderID,
			"stop_price":      item.StopPrice,
			"filled_price":    item.FilledPrice,
			"place_time_at":   item.PlaceTimeAt,
			"filled_time_at":  item.FilledTimeAt,
		},
	}
}

// --- init ---

func toWebullSymbol(symbol string) string {
	if idx := strings.Index(symbol, "/"); idx != -1 {
		return strings.ToUpper(symbol[:idx])
	}
	return strings.ToUpper(symbol)
}

func snapshotToTicker(symbol string, snap *Snapshot) ccxt.Ticker {
	sym := symbol
	now := time.Now().UnixMilli()

	price := parseFloat(snap.Price)
	prevClose := parseFloat(snap.PreClose)
	open := parseFloat(snap.Open)
	high := parseFloat(snap.High)
	low := parseFloat(snap.Low)
	close := parseFloat(snap.Close)
	volume := parseFloat(snap.Volume)
	change := parseFloat(snap.Change)
	changeRatio := parseFloat(snap.ChangeRatio)
	bid := parseFloat(snap.Bid)
	ask := parseFloat(snap.Ask)

	return ccxt.Ticker{
		Symbol:        &sym,
		Timestamp:     &now,
		Last:          &price,
		High:          &high,
		Low:           &low,
		Bid:           &bid,
		Ask:           &ask,
		Open:          &open,
		Close:         &close,
		PreviousClose: &prevClose,
		Change:        &change,
		Percentage:    &changeRatio,
		BaseVolume:    &volume,
	}
}

// --- Interface Compliance ---

// Compile-time assertion that webullAdapter implements broker.TradingProvider.
var _ broker.TradingProvider = (*webullAdapter)(nil)

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
