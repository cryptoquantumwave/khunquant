package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// pricedVenue is a fake venue that prices (per-symbol tickers), holds balances
// and trades, and records every ticker fetch and every CreateOrder. It is the
// base for the order-value cap tests.
//
// Each test registers its own provider ID: the ID is the broker registry key and
// part of the USD-rate cache key, so a shared one would leak a cached THB rate
// from one test into the next. Unknown IDs also take the permissive permission
// branch (see permission_gate_test.go), which keeps these tests about the cap.
type pricedVenue struct {
	id       string
	category broker.AssetCategory

	mu          sync.Mutex
	tickers     map[string]ccxt.Ticker
	balances    []broker.Balance
	tickerCalls []string
	orders      int
}

func newPricedVenue(t *testing.T, id string) *pricedVenue {
	t.Helper()
	v := &pricedVenue{
		id:       id,
		category: broker.CategoryCrypto,
		tickers:  map[string]ccxt.Ticker{},
		// Ample funds, so the balance preflight is not what a test trips over.
		balances: []broker.Balance{{Asset: "THB", Free: 1e9}, {Asset: "USDT", Free: 1e9}},
	}
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return v, nil })
	// The registry has no unregister: leave a factory that fails like an unregistered
	// provider, and drop the USD rates this venue may have cached.
	t.Cleanup(func() {
		broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) {
			return nil, errors.New("broker: provider " + id + " not registered")
		})
		broker.ResetUSDRateCache()
	})
	return v
}

// setTicker scripts a ticker. A zero ask or bid is left unset, as Binance TH
// does (it reports only a last).
func (v *pricedVenue) setTicker(symbol string, ask, bid, last float64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	tk := ccxt.Ticker{Last: &last}
	if ask > 0 {
		tk.Ask = &ask
	}
	if bid > 0 {
		tk.Bid = &bid
	}
	v.tickers[symbol] = tk
}

func (v *pricedVenue) fetched() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.tickerCalls...)
}

func (v *pricedVenue) orderCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.orders
}

func (v *pricedVenue) ID() string                     { return v.id }
func (v *pricedVenue) Category() broker.AssetCategory { return v.category }
func (v *pricedVenue) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (v *pricedVenue) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.tickerCalls = append(v.tickerCalls, symbol)
	tk, ok := v.tickers[symbol]
	if !ok {
		return ccxt.Ticker{}, errors.New("symbol not found: " + symbol)
	}
	return tk, nil
}

func (v *pricedVenue) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}

func (v *pricedVenue) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}

func (v *pricedVenue) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

func (v *pricedVenue) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

func (v *pricedVenue) GetBalances(context.Context) ([]broker.Balance, error) {
	return v.balances, nil
}

func (v *pricedVenue) GetWalletBalances(context.Context, string) ([]broker.WalletBalance, error) {
	return nil, nil
}

func (v *pricedVenue) FetchPrice(context.Context, string, string) (float64, error) { return 0, nil }
func (v *pricedVenue) SupportedWalletTypes() []string                              { return nil }

func (v *pricedVenue) CreateOrder(context.Context, string, string, string, float64, *float64, map[string]interface{}) (ccxt.Order, error) {
	v.mu.Lock()
	v.orders++
	v.mu.Unlock()
	id := "fake-order"
	return ccxt.Order{Id: &id}, nil
}

func (v *pricedVenue) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (v *pricedVenue) FetchOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}

func (v *pricedVenue) FetchOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	return nil, nil
}

func (v *pricedVenue) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	return nil, nil
}

func (v *pricedVenue) FetchMyTrades(context.Context, string, *int64, int) ([]ccxt.Trade, error) {
	return nil, nil
}

func capConfig(maxUSD float64) *config.Config {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.MaxOrderValueUSD = maxUSD
	return cfg
}

func placeOrder(cfg *config.Config, provider, symbol, orderType, side string, amount float64, price *float64, confirm bool) *ToolResult {
	args := map[string]any{
		"provider": provider, "symbol": symbol, "type": orderType, "side": side,
		"amount": amount, "confirm": confirm,
	}
	if price != nil {
		args["price"] = *price
	}
	return NewCreateOrderTool(cfg).Execute(context.Background(), args)
}

func capPrice(f float64) *float64 { return &f }

func TestCreateOrder_THBMarketOrderOverCap_Rejected(t *testing.T) {
	v := newPricedVenue(t, "mr2-thb-cap")
	v.setTicker("USDT/THB", 0, 0, 36)
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	cfg := capConfig(100)

	// 5,000 THB is about 139 USD: over the 100 USD cap, refused before the venue.
	res := placeOrder(cfg, v.id, "BTC/THB", "market", "buy", 0.05, nil, true)
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Fatalf("5,000 THB market order was not refused on the USD cap: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for an order over the cap")
	}

	// The same order with an explicit limit price (the other call site).
	res = placeOrder(cfg, v.id, "BTC/THB", "limit", "buy", 0.05, capPrice(100_000), true)
	if !res.IsError || !strings.Contains(res.ForLLM, "max_order_value_usd") {
		t.Fatalf("5,000 THB limit order was not refused on the USD cap: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for a limit order over the cap")
	}

	// 3,000 THB is about 83 USD: under the cap, so it goes through.
	res = placeOrder(cfg, v.id, "BTC/THB", "market", "buy", 0.03, nil, true)
	if res.IsError {
		t.Fatalf("3,000 THB market order was refused: %s", res.ForLLM)
	}
	if v.orderCount() != 1 {
		t.Errorf("the permitted order did not reach the venue (orders=%d)", v.orderCount())
	}
}

func TestCreateOrder_MarketBuyOverCapUSDT_Rejected(t *testing.T) {
	v := newPricedVenue(t, "mr2-usdt-cap")
	v.setTicker("BTC/USDT", 60_000, 59_990, 59_995)
	cfg := capConfig(100)

	// 0.01 BTC at 60,000 is 600 USD.
	res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if !res.IsError || !strings.Contains(res.ForLLM, "exceeds configured max_order_value_usd") {
		t.Fatalf("market buy over the cap was not refused: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for an order over the cap")
	}
	fetchesAfterMarket := len(v.fetched())

	// An explicit price in a USD-like quote is refused up front, with no
	// further ticker fetch at all.
	res = placeOrder(cfg, v.id, "BTC/USDT", "limit", "buy", 0.01, capPrice(60_000), true)
	if !res.IsError || !strings.Contains(res.ForLLM, "exceeds configured max_order_value_usd") {
		t.Fatalf("limit buy over the cap was not refused: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for a limit order over the cap")
	}
	if got := len(v.fetched()); got != fetchesAfterMarket {
		t.Errorf("a priced USDT limit order made %d extra ticker fetches", got-fetchesAfterMarket)
	}
}

func TestCreateOrder_CapSetNoRate_FailsClosed(t *testing.T) {
	cfg := capConfig(100)

	// A THB market whose USDT/THB rate cannot be fetched.
	v := newPricedVenue(t, "mr2-no-rate")
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	for name, px := range map[string]*float64{"market": nil, "limit": capPrice(100_000)} {
		res := placeOrder(cfg, v.id, "BTC/THB", name, "buy", 0.001, px, true)
		if !res.IsError || !strings.Contains(res.ForLLM, "USD conversion rate") {
			t.Errorf("%s: order with no USD rate was not refused for that cause: %+v", name, res)
		}
	}
	if v.orderCount() != 0 {
		t.Fatalf("CreateOrder was reached although the USD value was unknown (orders=%d)", v.orderCount())
	}

	// A market order whose price cannot be resolved.
	v2 := newPricedVenue(t, "mr2-no-price")
	res := placeOrder(cfg, v2.id, "BTC/USDT", "market", "buy", 0.0001, nil, true)
	if !res.IsError || !strings.Contains(res.ForLLM, "cannot determine the price") {
		t.Errorf("unpriceable market order was not refused for that cause: %+v", res)
	}
	if v2.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for an unpriceable order")
	}

	// A quote-less symbol on a broker whose currency is unknown.
	v3 := newPricedVenue(t, "mr2-no-quote")
	v3.setTicker("XYZ", 10, 9, 9.5)
	res = placeOrder(cfg, v3.id, "XYZ", "market", "buy", 1, nil, true)
	if !res.IsError || !strings.Contains(res.ForLLM, "USD conversion rate") {
		t.Errorf("order with an unknown quote currency was not refused: %+v", res)
	}
	if v3.orderCount() != 0 {
		t.Fatal("CreateOrder was reached for an order of unknown currency")
	}
}

func TestCreateOrder_CapZero_NoExtraNetwork(t *testing.T) {
	v := newPricedVenue(t, "mr2-cap-zero")
	v.setTicker("USDT/THB", 0, 0, 36)
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	v.setTicker("BTC/USDT", 60_000, 59_990, 59_995)
	cfg := capConfig(0)

	// A THB market order costs exactly the one ticker fetch it always did, and
	// no USD rate is fetched for it.
	if res := placeOrder(cfg, v.id, "BTC/THB", "market", "buy", 0.05, nil, true); res.IsError {
		t.Fatalf("market order refused with the cap at 0: %s", res.ForLLM)
	}
	if got := v.fetched(); len(got) != 1 || got[0] != "BTC/THB" {
		t.Errorf("market order fetches = %v; want exactly [BTC/THB]", got)
	}

	// A THB limit order needs no ticker at all.
	if res := placeOrder(cfg, v.id, "BTC/THB", "limit", "buy", 0.05, capPrice(100_000), true); res.IsError {
		t.Fatalf("limit order refused with the cap at 0: %s", res.ForLLM)
	}
	if got := v.fetched(); len(got) != 1 {
		t.Errorf("limit order added fetches: %v", got)
	}

	// A USDT market order: one fetch, for its own symbol.
	if res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true); res.IsError {
		t.Fatalf("USDT market order refused with the cap at 0: %s", res.ForLLM)
	}
	if got := v.fetched(); len(got) != 2 || got[1] != "BTC/USDT" {
		t.Errorf("fetches after the USDT order = %v; want [BTC/THB BTC/USDT]", got)
	}
	if v.orderCount() != 3 {
		t.Errorf("orders = %d; want 3", v.orderCount())
	}
}

func TestCreateOrder_PositionWarningUsesUSD(t *testing.T) {
	v := newPricedVenue(t, "mr2-warn-usd")
	v.setTicker("USDT/THB", 0, 0, 36)
	v.setTicker("BTC/THB", 100_000, 99_900, 99_950)
	cfg := capConfig(50_000)

	// 300,000 THB is about 8,333 USD: under the 10,000 USD warning threshold
	// although the raw THB figure is far over 10,000.
	res := placeOrder(cfg, v.id, "BTC/THB", "market", "buy", 3, nil, false)
	if res.IsError {
		t.Fatalf("an 8,333 USD order tripped the 10,000 USD warning: %s", res.ForLLM)
	}
	// The dry-run says what the order is worth in USD.
	if !strings.Contains(res.ForUser, "≈ $8333.33 USD at 36 THB per USD") {
		t.Errorf("dry-run does not show the USD estimate: %s", res.ForUser)
	}

	// 400,000 THB is about 11,111 USD: over, so it needs confirm.
	res = placeOrder(cfg, v.id, "BTC/THB", "market", "buy", 4, nil, false)
	if !res.IsError || !strings.Contains(res.ForLLM, "large order warning: notional value 11111.11 exceeds 10000 USD") {
		t.Errorf("an 11,111 USD order did not trigger the warning: %+v", res)
	}
}

func TestCreateOrder_MarketBuyBalancePreflightQuote(t *testing.T) {
	v := newPricedVenue(t, "mr2-preflight")
	v.setTicker("BTC/USDT", 60_000, 59_990, 59_995)
	cfg := capConfig(0)

	// 0.01 BTC at 60,000 needs 600 USDT. The 500 USDT balance is short even
	// though the base amount (0.01) would have passed a base-vs-quote compare.
	v.balances = []broker.Balance{{Asset: "USDT", Free: 500}, {Asset: "BTC", Free: 10}}
	res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true)
	if !res.IsError || !errors.Is(res.Err, broker.ErrInsufficientFunds) ||
		!strings.Contains(res.ForLLM, "insufficient USDT balance: have 500, need 600") {
		t.Fatalf("a short quote balance was not caught with the quote notional: %+v", res)
	}
	if v.orderCount() != 0 {
		t.Fatal("CreateOrder was reached despite insufficient funds")
	}

	// Enough quote balance: the order goes through.
	v.balances = []broker.Balance{{Asset: "USDT", Free: 1_000}}
	if res := placeOrder(cfg, v.id, "BTC/USDT", "market", "buy", 0.01, nil, true); res.IsError {
		t.Fatalf("a funded order was refused: %s", res.ForLLM)
	}

	// No price could be resolved (cap 0): the preflight is skipped, rather than
	// comparing 5 BTC against a 0.001 USDT balance.
	v2 := newPricedVenue(t, "mr2-preflight-noprice")
	v2.balances = []broker.Balance{{Asset: "USDT", Free: 0.001}}
	if res := placeOrder(cfg, v2.id, "BTC/USDT", "market", "buy", 5, nil, true); res.IsError {
		t.Fatalf("an unpriced order was refused by a base-vs-quote balance compare: %s", res.ForLLM)
	}
	if v2.orderCount() != 1 {
		t.Errorf("the unpriced order did not reach the venue (orders=%d)", v2.orderCount())
	}
}
