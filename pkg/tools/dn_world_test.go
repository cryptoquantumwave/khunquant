package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dnWorld is one simulated exchange for the delta-neutral workflow tests: a spot
// market and a perpetual swap that share state, so what the recovery reads off the
// "exchange" is what the legs really did. It dedups on a client order ID when it is
// capable (as Binance and OKX are), and ignores it when it is not.
//
// Numbers: a contract is 10 base units, the mark and the spot price are 100.
type dnWorld struct {
	mu sync.Mutex

	id           string
	capable      bool // adapters take and look up a client order ID
	listClosed   bool // a not-capable spot adapter's closed-orders listing shows our orders
	contractSize float64
	position     float64 // short contracts held
	spotBal      float64 // base-asset balance
	seq          int
	orders       map[string]*dnOrder
	byCID        map[string]string
	created      map[string]int // orders created per leg: "futures" / "spot"
	fillFrac     map[string]float64
	reject       map[string]error
	dupRejected  int
	onCreate     func(leg string) // called, unlocked, after an order is booked
	// closedFut and soldSpot add up what the exits took off the exchange: contracts
	// bought back (reduce-only or not) and base sold. reduceOnly counts the futures
	// orders that carried the flag; overClose the contracts a reduce-only order asked
	// for beyond the position (an exchange caps them; the test must never rely on it).
	ownerLong           float64 // contracts of the owner's own LONG on the symbol (hedge mode), listed first
	closedFut, soldSpot float64
	reduceOnly          int
	overClose           float64
}

type dnOrder struct {
	id, leg, side, cid, status string
	amount, filled             float64
}

func newDNWorld(t *testing.T, id string, capable bool) *dnWorld {
	t.Helper()
	w := &dnWorld{
		id: id, capable: capable, contractSize: 10, spotBal: 5,
		orders: map[string]*dnOrder{}, byCID: map[string]string{}, created: map[string]int{},
		fillFrac: map[string]float64{}, reject: map[string]error{},
	}
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) {
		if capable {
			return &dnSpotCap{dnSpot{w}}, nil
		}
		return &dnSpot{w}, nil
	})
	orig := futuresProviderFn
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		if capable {
			return &dnFutCap{dnFut{mockFuturesProvider: &mockFuturesProvider{}, w: w}}, nil
		}
		return &dnFut{mockFuturesProvider: &mockFuturesProvider{}, w: w}, nil
	}
	t.Cleanup(func() {
		futuresProviderFn = orig
		broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) {
			return nil, errors.New("broker: provider " + id + " not registered")
		})
	})
	return w
}

func (w *dnWorld) setFill(leg string, frac float64) {
	w.mu.Lock()
	w.fillFrac[leg] = frac
	w.mu.Unlock()
}
func (w *dnWorld) setReject(leg string, err error) { w.mu.Lock(); w.reject[leg] = err; w.mu.Unlock() }
func (w *dnWorld) placed(leg string) int           { w.mu.Lock(); defer w.mu.Unlock(); return w.created[leg] }
func (w *dnWorld) pos() (contracts, spot float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.position, w.spotBal
}

// create books an order and applies its fill. cid is "" for a not-capable adapter.
func (w *dnWorld) create(leg, side string, amount float64, params map[string]any, key string, reduceOnly bool) (ccxt.Order, error) {
	o, err := w.book(leg, side, amount, params, key, reduceOnly)
	if err == nil && w.onCreate != nil {
		w.onCreate(leg)
	}
	return o, err
}

func (w *dnWorld) book(leg, side string, amount float64, params map[string]any, key string, reduceOnly bool) (ccxt.Order, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.reject[leg]; err != nil {
		return ccxt.Order{}, err
	}
	cid := ""
	if v, ok := params[key]; ok && key != "" {
		cid, _ = v.(string)
	}
	if cid != "" {
		if _, dup := w.byCID[cid]; dup {
			w.dupRejected++
			return ccxt.Order{}, &ccxt.Error{Type: "InvalidOrder", Message: "duplicate client order id"}
		}
	}
	w.seq++
	o := &dnOrder{id: fmt.Sprintf("%s-%d", leg, w.seq), leg: leg, side: side, cid: cid, amount: amount, status: "closed"}
	frac, ok := w.fillFrac[leg]
	if !ok {
		frac = 1
	}
	o.filled = amount * frac
	if reduceOnly {
		w.reduceOnly++
		if o.filled > w.position {
			w.overClose += o.filled - w.position
			o.filled = w.position
		}
	}
	if frac < 1 {
		o.status = "canceled"
	}
	w.orders[o.id] = o
	if cid != "" {
		w.byCID[cid] = o.id
	}
	w.created[leg]++
	switch {
	case leg == "futures" && side == "sell":
		w.position += o.filled
	case leg == "futures":
		w.position -= o.filled
		w.closedFut += o.filled
	case side == "buy":
		w.spotBal += o.filled * (1 - 0.001) // the fee is taken from the base received
	default:
		w.spotBal -= o.filled
		w.soldSpot += o.filled
	}
	id := o.id
	return ccxt.Order{Id: &id}, nil // like OKX: the answer to a placement has no fill
}

func (w *dnWorld) get(id string) (ccxt.Order, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.orders[id]
	if !ok {
		return ccxt.Order{}, errors.New("order not found")
	}
	return toCCXT(o), nil
}

func toCCXT(o *dnOrder) ccxt.Order {
	id, st, side, amt, fl, avg := o.id, o.status, o.side, o.amount, o.filled, 100.0
	cost := o.filled * 100
	if o.leg == "futures" {
		cost *= 10
	}
	out := ccxt.Order{Id: &id, Status: &st, Side: &side, Amount: &amt, Filled: &fl, Average: &avg, Cost: &cost}
	if o.cid != "" {
		c := o.cid
		out.ClientOrderId = &c
	}
	return out
}

func (w *dnWorld) byClient(cid string) (ccxt.Order, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	id, ok := w.byCID[cid]
	if !ok {
		return ccxt.Order{}, false
	}
	return toCCXT(w.orders[id]), true
}

// ---- spot ----

type dnSpot struct{ w *dnWorld }

func (s *dnSpot) ID() string                     { return s.w.id }
func (s *dnSpot) Category() broker.AssetCategory { return broker.CategoryCrypto }
func (s *dnSpot) GetMarketStatus(context.Context, string) (broker.MarketStatus, error) {
	return broker.MarketOpen, nil
}
func (s *dnSpot) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	last, ask, bid := 100.0, 100.0, 100.0
	return ccxt.Ticker{Last: &last, Ask: &ask, Bid: &bid}, nil
}
func (s *dnSpot) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}
func (s *dnSpot) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}
func (s *dnSpot) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}
func (s *dnSpot) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}
func (s *dnSpot) GetBalances(context.Context) ([]broker.Balance, error) {
	_, bal := s.w.pos()
	return []broker.Balance{{Asset: "BTC", Free: bal}, {Asset: "USDT", Free: 1e9}}, nil
}
func (s *dnSpot) GetWalletBalances(context.Context, string) ([]broker.WalletBalance, error) {
	return nil, nil
}
func (s *dnSpot) FetchPrice(context.Context, string, string) (float64, error) { return 100, nil }
func (s *dnSpot) SupportedWalletTypes() []string                              { return nil }
func (s *dnSpot) CreateOrder(_ context.Context, _, _, side string, amount float64, _ *float64, params map[string]any) (ccxt.Order, error) {
	key := ""
	if s.w.capable {
		key = "clientOrderId"
	}
	return s.w.create("spot", side, amount, params, key, false)
}
func (s *dnSpot) CancelOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (s *dnSpot) FetchOrder(_ context.Context, id, _ string) (ccxt.Order, error) { return s.w.get(id) }
func (s *dnSpot) FetchOpenOrders(context.Context, string) ([]ccxt.Order, error)  { return nil, nil }
func (s *dnSpot) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	var out []ccxt.Order
	if s.w.listClosed {
		for _, o := range s.w.orders {
			if o.leg == "spot" {
				out = append(out, toCCXT(o))
			}
		}
	}
	return out, nil
}
func (s *dnSpot) FetchMyTrades(context.Context, string, *int64, int) ([]ccxt.Trade, error) {
	return nil, nil
}

// dnSpotCap is the capable spot adapter: it takes a client order ID and looks it up.
type dnSpotCap struct{ dnSpot }

func (s *dnSpotCap) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }
func (s *dnSpotCap) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	o, ok := s.w.byClient(cid)
	return o, ok, nil
}

// ---- futures ----

type dnFut struct {
	*mockFuturesProvider
	w *dnWorld
}

func (f *dnFut) ID() string { return f.w.id }
func (f *dnFut) FetchFuturesMarkPrice(context.Context, string) (float64, error) {
	return 100, nil
}
func (f *dnFut) SetFuturesLeverage(context.Context, string, int64, string, string) (map[string]any, error) {
	return nil, nil
}
func (f *dnFut) LoadFuturesMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	cs, minAmt, swap, active := f.w.contractSize, 1.0, true, true
	return map[string]ccxt.MarketInterface{"BTC/USDT:USDT": {
		ContractSize: &cs, Active: &active, Swap: &swap, Limits: ccxt.Limits{Amount: ccxt.MinMax{Min: &minAmt}},
	}}, nil
}
func (f *dnFut) CreateFuturesOrder(_ context.Context, r broker.FuturesOrderRequest) (ccxt.Order, error) {
	key := ""
	if f.w.capable {
		key = "clOrdId"
	}
	return f.w.create("futures", r.Side, r.Amount, r.Params, key, r.ReduceOnly)
}
func (f *dnFut) FetchFuturesOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	return f.w.get(id)
}
func (f *dnFut) FetchFuturesOpenOrders(context.Context, string) ([]ccxt.Order, error) {
	return nil, nil
}
func (f *dnFut) CancelFuturesOrder(context.Context, string, string) (ccxt.Order, error) {
	return ccxt.Order{}, nil
}
func (f *dnFut) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	c, _ := f.w.pos()
	f.w.mu.Lock()
	long := f.w.ownerLong
	f.w.mu.Unlock()
	sym, side, lside, margin := "BTC/USDT:USDT", "short", "long", "cross"
	var out []ccxt.Position
	if long > 0 {
		out = append(out, ccxt.Position{Symbol: &sym, Side: &lside, Contracts: &long, MarginMode: &margin})
	}
	if c > 0 {
		out = append(out, ccxt.Position{Symbol: &sym, Side: &side, Contracts: &c, MarginMode: &margin})
	}
	return out, nil
}

type dnFutCap struct{ dnFut }

func (f *dnFutCap) ClientOrderIDKey(string) (string, int, bool) { return "clOrdId", 32, true }
func (f *dnFutCap) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	o, ok := f.w.byClient(cid)
	return o, ok, nil
}
