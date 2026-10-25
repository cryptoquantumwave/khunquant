package broker_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// secretMarker is planted in the fakes' config and in the text of their errors;
// no error PlaceOrderIdempotent builds may contain it.
const secretMarker = "SECRET-MARKER-7f3a"

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: t0} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func ptr[T any](v T) *T { return &v }

// ccxtErr builds the text a ccxt error has after an adapter flattened it.
func ccxtErr(typ, msg string) error {
	return fmt.Errorf("[ccxtError]::[%s]::[%s]", typ, msg)
}

// exch is the fake exchange: a book of accepted orders and a record of every call.
type exch struct {
	mu      sync.Mutex
	clk     *clock
	seq     int
	orders  []ccxt.Order
	trades  []ccxt.Trade
	calls   map[string]int
	secret  string // "config" of the fake; must never reach an error
	closedN int    // cap on the closed-orders listing (0 = unlimited)

	// Behaviour.
	createErr    func(n int) error               // called before the order is accepted
	afterAccept  func(n int, o ccxt.Order) error // called once accepted; its error is the "lost response"
	listErr      map[string]error
	positions    []ccxt.Position
	lookupErr    error
	lookupHidden int // number of by-ID lookups that answer "not found" even though the order exists
	uniqueCID    bool
	fillStatus   string // status of an accepted order ("closed" default)
	hideListings bool   // the listings never show an accepted order (a type or price the match cannot recognise)
}

func newExch(clk *clock) *exch {
	return &exch{clk: clk, calls: map[string]int{}, listErr: map[string]error{}, secret: secretMarker, fillStatus: "closed"}
}

func (e *exch) count(name string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[name]
}

func (e *exch) total(names ...string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, k := range names {
		n += e.calls[k]
	}
	return n
}

func (e *exch) placed() int { return e.count("create") }

// book returns a copy of the accepted orders.
func (e *exch) book() []ccxt.Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]ccxt.Order(nil), e.orders...)
}

func (e *exch) accept(symbol, typ, side string, amount float64, price *float64, cid string) (ccxt.Order, error) {
	e.mu.Lock()
	e.calls["create"]++
	n := e.calls["create"]
	if e.createErr != nil {
		if err := e.createErr(n); err != nil {
			e.mu.Unlock()
			return ccxt.Order{}, err
		}
	}
	if e.uniqueCID && cid != "" {
		for _, o := range e.orders {
			if o.ClientOrderId != nil && *o.ClientOrderId == cid {
				e.mu.Unlock()
				return ccxt.Order{}, ccxtErr("InvalidOrder", "exchange {\"code\":-2010,\"msg\":\"Duplicate order sent.\"} "+e.secret)
			}
		}
	}
	e.seq++
	id := fmt.Sprintf("ord-%d", e.seq)
	ts := e.clk.Now().UnixMilli()
	o := ccxt.Order{
		Id: &id, Symbol: ptr(symbol), Type: ptr(typ), Side: ptr(side), Amount: ptr(amount), Price: price,
		Status: ptr(e.fillStatus), Timestamp: &ts,
	}
	if cid != "" {
		o.ClientOrderId = ptr(cid)
	}
	e.orders = append(e.orders, o)
	hook := e.afterAccept
	e.mu.Unlock()
	if hook != nil {
		if err := hook(n, o); err != nil {
			return ccxt.Order{}, err
		}
	}
	return o, nil
}

func (e *exch) open() []ccxt.Order {
	var out []ccxt.Order
	for _, o := range e.orders {
		if *o.Status == "open" {
			out = append(out, o)
		}
	}
	return out
}

func (e *exch) closed(since *int64, limit int) []ccxt.Order {
	var out []ccxt.Order
	for _, o := range e.orders {
		if *o.Status != "closed" {
			continue
		}
		if since != nil && o.Timestamp != nil && *o.Timestamp < *since {
			continue
		}
		out = append(out, o)
	}
	if e.closedN > 0 && len(out) > e.closedN {
		out = out[:e.closedN]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---- providers ----

// plainProv is a spot provider with no client order ID support.
type plainProv struct {
	broker.TradingProvider // nil: a call to an unimplemented method panics
	ex                     *exch
	params                 *[]map[string]any
}

func newPlain(ex *exch) *plainProv {
	return &plainProv{ex: ex, params: new([]map[string]any)}
}

func (p *plainProv) ID() string                     { return "fake" }
func (p *plainProv) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (p *plainProv) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (p *plainProv) cidOf(params map[string]any) string {
	if v, ok := params["clientOrderId"].(string); ok {
		return v
	}
	return ""
}

func (p *plainProv) CreateOrder(_ context.Context, symbol, typ, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	cp := map[string]any{}
	for k, v := range params {
		cp[k] = v
	}
	p.ex.mu.Lock()
	*p.params = append(*p.params, cp)
	p.ex.mu.Unlock()
	return p.ex.accept(symbol, typ, side, amount, price, "")
}

func (p *plainProv) FetchOpenOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["open"]++
	if err := p.ex.listErr["open"]; err != nil {
		return nil, err
	}
	if p.ex.hideListings {
		return nil, nil
	}
	return p.ex.open(), nil
}

func (p *plainProv) FetchClosedOrders(_ context.Context, _ string, since *int64, limit int) ([]ccxt.Order, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["closed"]++
	if err := p.ex.listErr["closed"]; err != nil {
		return nil, err
	}
	if p.ex.hideListings {
		return nil, nil
	}
	return p.ex.closed(since, limit), nil
}

func (p *plainProv) FetchMyTrades(_ context.Context, _ string, _ *int64, limit int) ([]ccxt.Trade, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["trades"]++
	if err := p.ex.listErr["trades"]; err != nil {
		return nil, err
	}
	out := p.ex.trades
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (p *plainProv) FetchOrder(context.Context, string, string) (ccxt.Order, error) {
	p.ex.mu.Lock()
	p.ex.calls["fetch"]++
	p.ex.mu.Unlock()
	return ccxt.Order{}, errors.New("FetchOrder by id must not be used by the reconcile")
}

// capNoLookup carries the client order ID but has no by-ID lookup (the listings are searched).
type capNoLookup struct{ *plainProv }

func (capNoLookup) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (p capNoLookup) CreateOrder(ctx context.Context, symbol, typ, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error) {
	cp := map[string]any{}
	for k, v := range params {
		cp[k] = v
	}
	p.ex.mu.Lock()
	*p.params = append(*p.params, cp)
	p.ex.mu.Unlock()
	return p.ex.accept(symbol, typ, side, amount, price, p.cidOf(params))
}

// capProv is capable with a by-ID lookup.
type capProv struct{ capNoLookup }

func newCap(ex *exch) capProv { return capProv{capNoLookup{newPlain(ex)}} }

func (p capProv) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["lookup"]++
	if p.ex.lookupErr != nil {
		return ccxt.Order{}, false, p.ex.lookupErr
	}
	if p.ex.lookupHidden > 0 {
		p.ex.lookupHidden--
		return ccxt.Order{}, false, nil
	}
	for _, o := range p.ex.orders {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

// futProv is a futures provider (spot methods absent). capFut adds the client ID.
type futProv struct {
	broker.FuturesProvider // nil
	ex                     *exch
	reqs                   *[]broker.FuturesOrderRequest
}

func newFut(ex *exch) *futProv { return &futProv{ex: ex, reqs: new([]broker.FuturesOrderRequest)} }

func (p *futProv) ID() string                     { return "fakefut" }
func (p *futProv) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (p *futProv) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}

func (p *futProv) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) {
	p.ex.mu.Lock()
	*p.reqs = append(*p.reqs, req)
	p.ex.mu.Unlock()
	return p.ex.accept(req.Symbol, req.OrderType, req.Side, req.Amount, req.Price, "")
}

func (p *futProv) FetchFuturesOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["fopen"]++
	if err := p.ex.listErr["fopen"]; err != nil {
		return nil, err
	}
	return p.ex.open(), nil
}

func (p *futProv) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["positions"]++
	if err := p.ex.listErr["positions"]; err != nil {
		return nil, err
	}
	return p.ex.positions, nil
}

type capFut struct{ *futProv }

func (capFut) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (p capFut) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) {
	p.ex.mu.Lock()
	*p.reqs = append(*p.reqs, req)
	p.ex.mu.Unlock()
	cid, _ := req.Params["clientOrderId"].(string)
	return p.ex.accept(req.Symbol, req.OrderType, req.Side, req.Amount, req.Price, cid)
}

func (p capFut) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	p.ex.mu.Lock()
	defer p.ex.mu.Unlock()
	p.ex.calls["lookup"]++
	if p.ex.lookupErr != nil {
		return ccxt.Order{}, false, p.ex.lookupErr
	}
	for _, o := range p.ex.orders {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

// ---- helpers ----

func spotReq(amount float64) broker.OrderRequest {
	return broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: "BTC/USDT", Type: "market", Side: "buy", Amount: amount}
}

func limitReq(amount, price float64) broker.OrderRequest {
	return broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: "BTC/USDT", Type: "limit", Side: "buy", Amount: amount, Price: ptr(price)}
}

func futReq(amount float64) broker.OrderRequest {
	return broker.OrderRequest{Market: broker.OrderMarketFutures, Symbol: "BTC/USDT:USDT", Type: "market", Side: "buy", Amount: amount, PositionSide: "long"}
}

// placeOpts returns the options a test uses: the fake clock and the default delay.
func placeOpts(clk *clock) []broker.PlaceOption {
	return []broker.PlaceOption{broker.WithClock(clk.Now)}
}

func contains(err error, s string) bool { return err != nil && strings.Contains(err.Error(), s) }
