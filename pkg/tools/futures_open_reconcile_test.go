package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// lostFut is a futures provider whose entry call can book the order and then lose the
// answer, the case futures_open_position used to report as "entry order failed".
// Nothing here talks to an exchange.
type lostFut struct {
	*mockFuturesProvider
	mu sync.Mutex

	// behaviour
	lost      bool  // the entry call returns lostErr instead of the order
	booked    bool  // the exchange created the order even though the answer is lost
	hidden    bool  // a lookup does not show a booked order (lag)
	rejectErr error // the entry call is refused with this and books nothing
	lookupErr error
	posErr    error
	fillFrac  float64

	// state and record
	orders       []*lostOrder
	position     float64
	log          []string
	entries      int
	entryParams  []map[string]interface{}
	protect      []broker.FuturesOrderRequest
	exits        []broker.FuturesOrderRequest
	lookupCIDs   []string
	lookups      int
	entryReturn  time.Time
	lookupAt     time.Time
	positionRead int
}

type lostOrder struct {
	id, cid string
	amount  float64
}

// lostFutCap is lostFut for an adapter that takes a client order ID and looks it up.
type lostFutCap struct{ *lostFut }

func (lostFutCap) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (c lostFutCap) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	f := c.lostFut
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	f.lookupAt = time.Now()
	f.lookupCIDs = append(f.lookupCIDs, cid)
	f.log = append(f.log, "lookup")
	if f.lookupErr != nil {
		return ccxt.Order{}, false, f.lookupErr
	}
	if f.hidden {
		return ccxt.Order{}, false, nil
	}
	for _, o := range f.orders {
		if o.cid == cid {
			id, sym, side, amt := o.id, "BTC/USDT:USDT", "buy", o.amount
			return ccxt.Order{Id: &id, Symbol: &sym, Side: &side, Amount: &amt}, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

func (f *lostFut) SetFuturesLeverage(_ context.Context, _ string, _ int64, _, _ string) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "leverage")
	return nil, nil
}

func (f *lostFut) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	f.mu.Lock()
	defer f.mu.Unlock()
	_, sl := req.Params["stopLossPrice"]
	_, tp := req.Params["takeProfitPrice"]
	if req.ReduceOnly {
		id := fmt.Sprintf("x-%d", len(f.protect)+len(f.exits)+1)
		if sl || tp {
			f.log = append(f.log, "protect")
			f.protect = append(f.protect, req)
		} else {
			f.log = append(f.log, "exit")
			f.exits = append(f.exits, req)
			f.position -= req.Amount
		}
		return ccxt.Order{Id: &id}, nil
	}
	f.log = append(f.log, "entry")
	f.entries++
	cp := map[string]interface{}{}
	for k, v := range req.Params {
		cp[k] = v
	}
	f.entryParams = append(f.entryParams, cp)
	if f.rejectErr != nil {
		return ccxt.Order{}, f.rejectErr
	}
	id := fmt.Sprintf("ord-%d", f.entries)
	cid, _ := req.Params["clientOrderId"].(string)
	if !f.lost || f.booked {
		f.orders = append(f.orders, &lostOrder{id: id, cid: cid, amount: req.Amount})
		f.position += req.Amount
	}
	if f.lost {
		f.entryReturn = time.Now()
		return ccxt.Order{}, &ccxt.Error{Type: "RequestTimeout", Message: "read tcp: i/o timeout SECRET-BODY-9f3"}
	}
	return ccxt.Order{Id: &id}, nil
}

func (f *lostFut) FetchFuturesOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orders {
		if o.id == id {
			filled, status := o.amount*f.fillFrac, "closed"
			return ccxt.Order{Id: &id, Filled: &filled, Status: &status}, nil
		}
	}
	return ccxt.Order{}, errors.New("no such order")
}

func (f *lostFut) FetchFuturesPositions(_ context.Context, _ []string) ([]ccxt.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.positionRead++
	f.log = append(f.log, "positions")
	if f.posErr != nil {
		return nil, f.posErr
	}
	if f.position <= 0 {
		return nil, nil
	}
	sym, side, c := "BTC/USDT:USDT", "long", f.position
	return []ccxt.Position{{Symbol: &sym, Side: &side, Contracts: &c}}, nil
}

func (f *lostFut) snapshot() (entries, lookups int, protect, exits []broker.FuturesOrderRequest, log []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.entries, f.lookups, append([]broker.FuturesOrderRequest(nil), f.protect...), append([]broker.FuturesOrderRequest(nil), f.exits...), append([]string(nil), f.log...)
}

// logLimiter counts the rate limit tokens spent, and logs each into the provider's log.
type logLimiter struct {
	mu    sync.Mutex
	f     *lostFut
	spent int
	deny  bool
}

func (l *logLimiter) Allow(string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.f.mu.Lock()
	l.f.log = append(l.f.log, "allow")
	l.f.mu.Unlock()
	if l.deny {
		return false
	}
	l.spent++
	return true
}
func (l *logLimiter) Status() map[string]broker.RateLimitStatus { return nil }
func (l *logLimiter) tokens() int                               { l.mu.Lock(); defer l.mu.Unlock(); return l.spent }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const testSettle = 40 * time.Millisecond

type lostWorld struct {
	f       *lostFut
	limiter *logLimiter
	clock   *fakeClock
}

// newLostWorld installs the fake as the futures provider, a counting limiter, a short
// settle delay and a clock the test moves. capable picks an adapter with client order IDs.
func newLostWorld(t *testing.T, capable bool) *lostWorld {
	t.Helper()
	sym := "BTC/USDT:USDT"
	active, swap := true, true
	minAmt, maxLev := 0.001, 100.0
	f := &lostFut{fillFrac: 1, booked: true, mockFuturesProvider: &mockFuturesProvider{
		markPriceFn: func(context.Context, string) (float64, error) { return 50000, nil },
		loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{sym: {
				Active: &active, Swap: &swap,
				Limits: ccxt.Limits{Amount: ccxt.MinMax{Min: &minAmt}, Leverage: ccxt.MinMax{Max: &maxLev}},
			}}, nil
		},
	}}
	w := &lostWorld{f: f, limiter: &logLimiter{f: f}, clock: &fakeClock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}}
	origFn, origLimiter, origSettle, origNow := futuresProviderFn, broker.DefaultLimiter, futuresOpenSettle, futuresOpenNow
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		if capable {
			return lostFutCap{f}, nil
		}
		return f, nil
	}
	broker.DefaultLimiter = w.limiter
	futuresOpenSettle, futuresOpenNow = testSettle, w.clock.now
	resetUnknownEntries()
	t.Cleanup(func() {
		futuresProviderFn, broker.DefaultLimiter, futuresOpenSettle, futuresOpenNow = origFn, origLimiter, origSettle, origNow
		resetUnknownEntries()
	})
	return w
}

func (w *lostWorld) open(t *testing.T, cfg *config.Config, over map[string]any) *ToolResult {
	t.Helper()
	if cfg == nil {
		cfg = seqCfg()
	}
	args := seqOpenArgs()
	for k, v := range over {
		args[k] = v
	}
	return NewFuturesOpenPositionTool(cfg).Execute(context.Background(), args)
}

func plainWords(t *testing.T, res *ToolResult) {
	t.Helper()
	low := strings.ToLower(res.ForLLM)
	for _, bad := range []string{"failed", "retry", "try again"} {
		if strings.Contains(low, bad) {
			t.Errorf("an unknown outcome must not be called a failure or invite a retry (%q): %s", bad, res.ForLLM)
		}
	}
	if strings.Contains(res.ForLLM, "SECRET-BODY") {
		t.Errorf("the result carries raw exchange text: %s", res.ForLLM)
	}
}

func assertUnknownOutcome(t *testing.T, res *ToolResult) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("an entry whose outcome is unknown must be an error result: %s", res.ForLLM)
	}
	if !errors.Is(res.Err, broker.ErrOutcomeUnknown) {
		t.Errorf("the result must carry broker.ErrOutcomeUnknown, got %v", res.Err)
	}
	for _, want := range []string{"OUTCOME UNKNOWN", "may exist", "NO stop-loss or take-profit was placed", "futures_get_positions"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the result must say %q: %s", want, res.ForLLM)
		}
	}
	plainWords(t, res)
}

// The answer is lost, the order is on the exchange, and the lookup by client order ID
// finds it: one order, the fill verified, SL/TP on the FILLED amount, and the result
// says the answer was lost.
func TestFuturesOpen_LostAnswer_FoundByClientID(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.fillFrac = true, 0.4 // 0.01 asked, 0.004 filled

	res := w.open(t, nil, nil)

	if res.IsError {
		t.Fatalf("a found order carries on as a success, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "answer to the entry order was lost") || !strings.Contains(res.ForLLM, "found it by its client order ID") {
		t.Errorf("the result must name that the answer was lost and how it was found: %s", res.ForLLM)
	}
	entries, lookups, protect, _, _ := w.f.snapshot()
	if entries != 1 || lookups != 1 {
		t.Fatalf("entries=%d lookups=%d, want one entry and one look", entries, lookups)
	}
	if len(protect) != 2 {
		t.Fatalf("want a stop-loss and a take-profit, got %d protection orders", len(protect))
	}
	for _, p := range protect {
		if p.Amount != 0.004 {
			t.Errorf("protection amount = %v, want the filled 0.004", p.Amount)
		}
	}
	cid, _ := w.f.entryParams[0]["clientOrderId"].(string)
	if cid == "" || len(w.f.lookupCIDs) != 1 || w.f.lookupCIDs[0] != cid {
		t.Errorf("the look must use the entry's client order ID %q, looked up %v", cid, w.f.lookupCIDs)
	}
	if gap := w.f.lookupAt.Sub(w.f.entryReturn); gap < testSettle {
		t.Errorf("the look came %s after the lost answer, want at least the settle delay %s", gap, testSettle)
	}
	if got := w.limiter.tokens(); got != 1 {
		t.Errorf("rate limit tokens spent = %d, want 1 (a look spends none)", got)
	}
	if n := len(openUnknowns.m); n != 0 {
		t.Errorf("a found order leaves no marker, have %d", n)
	}
}

// Not found: at most one order, no second create, no protection, and the result says so.
func TestFuturesOpen_LostAnswer_NotFound_IsUnknownWithNoSecondCreate(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.hidden = true, true // the order exists but the lookup does not show it

	res := w.open(t, nil, nil)

	assertUnknownOutcome(t, res)
	entries, lookups, protect, _, _ := w.f.snapshot()
	if entries != 1 || lookups != 1 || len(protect) != 0 {
		t.Errorf("entries=%d lookups=%d protect=%d, want 1, 1, 0", entries, lookups, len(protect))
	}
	if n := len(openUnknowns.m); n != 1 {
		t.Errorf("an unresolved entry must leave a marker, have %d", n)
	}
}

func TestFuturesOpen_LostAnswer_LookFailed_IsUnknown(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost = true
	w.f.lookupErr = &ccxt.Error{Type: "ExchangeNotAvailable", Message: "503 SECRET-BODY-9f3"}

	res := w.open(t, nil, nil)

	assertUnknownOutcome(t, res)
	entries, lookups, protect, _, _ := w.f.snapshot()
	if entries != 1 || lookups != 1 || len(protect) != 0 {
		t.Errorf("entries=%d lookups=%d protect=%d, want 1, 1, 0", entries, lookups, len(protect))
	}
	if !strings.Contains(res.ForLLM, "could not be completed") {
		t.Errorf("the result must say the look could not be completed: %s", res.ForLLM)
	}
}

// An adapter with no client order ID: the position is the only evidence. A position that
// moved by about the order is NOT treated as a found order (there is no order to verify
// or to size protection from): it is an unknown outcome, said to have most likely gone through.
func TestFuturesOpen_LostAnswer_NotCapable_PositionMoved_IsUnknownNotFound(t *testing.T) {
	w := newLostWorld(t, false)
	w.f.lost = true

	res := w.open(t, nil, nil)

	assertUnknownOutcome(t, res)
	if !strings.Contains(res.ForLLM, "most likely went through") {
		t.Errorf("a moved position must be said to most likely have gone through: %s", res.ForLLM)
	}
	entries, _, protect, _, _ := w.f.snapshot()
	if entries != 1 || len(protect) != 0 {
		t.Errorf("entries=%d protect=%d, want 1 and 0", entries, len(protect))
	}
	if _, ok := w.f.entryParams[0]["clientOrderId"]; ok {
		t.Error("an adapter that cannot take a client order ID must not be sent one")
	}
}

func TestFuturesOpen_LostAnswer_NotCapable_PositionUnchanged_IsUnknown(t *testing.T) {
	w := newLostWorld(t, false)
	w.f.lost, w.f.booked = true, false // the order never reached the exchange

	res := w.open(t, nil, nil)

	assertUnknownOutcome(t, res)
	if entries, _, _, _, _ := w.f.snapshot(); entries != 1 {
		t.Errorf("entries = %d, want 1", entries)
	}
}

// Without a client order ID the position before the order is the only evidence, so an
// unreadable position stops the order before anything is sent. With one, it does not.
func TestFuturesOpen_PositionUnreadable(t *testing.T) {
	w := newLostWorld(t, false)
	w.f.posErr = errors.New("positions down SECRET-BODY-9f3")
	res := w.open(t, nil, nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "Nothing was sent") || strings.Contains(res.ForLLM, "SECRET-BODY") {
		t.Fatalf("an unreadable position on a not-capable adapter must refuse cleanly: %s", res.ForLLM)
	}
	if entries, _, _, _, log := w.f.snapshot(); entries != 0 || logHas(log, "leverage") || w.limiter.tokens() != 0 {
		t.Errorf("nothing may be sent or spent: entries=%d log=%v tokens=%d", entries, log, w.limiter.tokens())
	}

	w = newLostWorld(t, true)
	w.f.posErr = errors.New("positions down")
	if res := w.open(t, nil, nil); res.IsError {
		t.Fatalf("a capable adapter does not need the baseline: %s", res.ForLLM)
	}
}

func logHas(log []string, op string) bool {
	for _, l := range log {
		if l == op {
			return true
		}
	}
	return false
}

// The exchange's own refusal stays a plain failure: no look, no wait, no marker, and
// the next open is not held back.
func TestFuturesOpen_DefiniteRejection_IsPlainFailure(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.rejectErr = &ccxt.Error{Type: "InsufficientFunds", Message: "margin is insufficient"}

	start := time.Now()
	res := w.open(t, nil, nil)

	if !res.IsError || !strings.Contains(res.ForLLM, "entry order failed") {
		t.Fatalf("a definite rejection is a plain failure: %s", res.ForLLM)
	}
	if errors.Is(res.Err, broker.ErrOutcomeUnknown) {
		t.Error("a definite rejection is not an unknown outcome")
	}
	if _, lookups, _, _, _ := w.f.snapshot(); lookups != 0 {
		t.Errorf("a definite rejection must not be looked for, %d lookups", lookups)
	}
	if elapsed := time.Since(start); elapsed >= testSettle {
		t.Errorf("a definite rejection must not wait out the settle delay, took %s", elapsed)
	}
	if n := len(openUnknowns.m); n != 0 {
		t.Errorf("a definite rejection leaves no marker, have %d", n)
	}
	w.f.rejectErr = nil
	if res := w.open(t, nil, nil); res.IsError {
		t.Errorf("the next open must not be held back: %s", res.ForLLM)
	}
}

// The next call on the same account, symbol and side.
func TestFuturesOpen_NextCall_RefusedWhileTheEarlierOrderIsThere(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.hidden = true, true
	assertUnknownOutcome(t, w.open(t, nil, nil))
	spent := w.limiter.tokens()

	w.f.hidden = false // the lookup now shows the order
	w.clock.advance(2 * futuresUnknownEntryClearAfter)
	res := w.open(t, nil, nil)

	if !res.IsError || !strings.Contains(res.ForLLM, "refused") || !strings.Contains(res.ForLLM, "that order is now on the exchange") {
		t.Fatalf("the next open must be refused while the earlier order is there: %s", res.ForLLM)
	}
	if !errors.Is(res.Err, broker.ErrOutcomeUnknown) {
		t.Errorf("the refusal carries the unknown outcome, got %v", res.Err)
	}
	if !strings.Contains(res.ForLLM, "no stop-loss or take-profit was placed") {
		t.Errorf("the refusal must say the position is unprotected, for a reader who missed the first result: %s", res.ForLLM)
	}
	plainWords(t, res)
	if entries, _, _, _, _ := w.f.snapshot(); entries != 1 {
		t.Errorf("entries = %d, want no second create", entries)
	}
	if got := w.limiter.tokens(); got != spent {
		t.Errorf("a refused call spent a token: %d -> %d", spent, got)
	}
}

func TestFuturesOpen_NextCall_RefusedWhenThePositionMoved(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.hidden = true, true
	assertUnknownOutcome(t, w.open(t, nil, nil)) // booked, so the position is now 0.01 up
	w.clock.advance(2 * futuresUnknownEntryClearAfter)

	res := w.open(t, nil, nil)

	if !res.IsError || !strings.Contains(res.ForLLM, "the position has changed since") || !strings.Contains(res.ForLLM, "no stop-loss or take-profit was placed") {
		t.Fatalf("a moved position must hold the next open back even when the lookup shows nothing: %s", res.ForLLM)
	}
	if entries, _, _, _, _ := w.f.snapshot(); entries != 1 {
		t.Errorf("entries = %d, want no second create", entries)
	}
}

// A look that proves nothing changed clears the marker, but not before the clear-after age
// (an immediate retry comes seconds after the first call), and a marker dies with its window.
func TestFuturesOpen_NextCall_AllowedOnlyAfterALookProvesNothingChanged(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.booked = true, false // the order never reached the exchange
	assertUnknownOutcome(t, w.open(t, nil, nil))

	w.clock.advance(5 * time.Second)
	res := w.open(t, nil, nil)
	if !res.IsError || !strings.Contains(res.ForLLM, "only 5 seconds have passed") {
		t.Fatalf("a call seconds later must be refused: %s", res.ForLLM)
	}
	if entries, _, _, _, _ := w.f.snapshot(); entries != 1 {
		t.Fatalf("entries = %d, want no second create in the refused case", entries)
	}

	w.clock.advance(futuresUnknownEntryClearAfter)
	w.f.lost = false
	res = w.open(t, nil, nil)
	if res.IsError {
		t.Fatalf("a look that shows nothing changed must let the open through: %s", res.ForLLM)
	}
	if entries, _, _, _, _ := w.f.snapshot(); entries != 2 {
		t.Errorf("entries = %d, want the second open placed", entries)
	}
	if n := len(openUnknowns.m); n != 0 {
		t.Errorf("the marker must be cleared, have %d", n)
	}
}

func TestFuturesOpen_NextCall_NoBaselineCannotClear_ButTheWindowEnds(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.posErr = errors.New("positions down") // no baseline can be taken
	w.f.lost, w.f.booked = true, false
	assertUnknownOutcome(t, w.open(t, nil, nil))
	w.f.posErr = nil

	w.clock.advance(2 * futuresUnknownEntryClearAfter)
	if res := w.open(t, nil, nil); !res.IsError || !strings.Contains(res.ForLLM, "nothing can show that it did not go through") {
		t.Fatalf("with no baseline the marker cannot be cleared by a look: %s", res.ForLLM)
	}
	w.clock.advance(futuresUnknownEntryWindow)
	w.f.lost = false
	if res := w.open(t, nil, nil); res.IsError {
		t.Fatalf("after the window the marker is gone: %s", res.ForLLM)
	}
}

// The marker is per account, symbol and side.
func TestFuturesOpen_Marker_DoesNotHoldBackOtherSides(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.hidden = true, true
	assertUnknownOutcome(t, w.open(t, nil, nil))
	w.f.lost = false

	if res := w.open(t, nil, map[string]any{"side": "short"}); res.IsError {
		t.Fatalf("the other side of the symbol is not held back: %s", res.ForLLM)
	}
}

// Exits are never gated by the marker.
func TestFuturesOpen_Marker_NeverGatesAnExit(t *testing.T) {
	w := newLostWorld(t, true)
	w.f.lost, w.f.hidden = true, true
	assertUnknownOutcome(t, w.open(t, nil, nil))
	w.f.lost = false
	if n := len(openUnknowns.m); n != 1 {
		t.Fatalf("setup: want a marker, have %d", n)
	}

	cfg := seqCfg()
	res := NewFuturesClosePositionTool(cfg).Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "confirm": true,
	})
	if res.IsError {
		t.Fatalf("a close must go through while the marker is set: %s", res.ForLLM)
	}
	if _, _, _, exits, _ := w.f.snapshot(); len(exits) != 1 || !exits[0].ReduceOnly {
		t.Errorf("want one reduce-only exit, got %+v", exits)
	}
	if n := len(openUnknowns.m); n != 1 {
		t.Errorf("an exit must leave the marker alone, have %d", n)
	}
}

// A client order ID in params is refused before anything is sent: the tool sets its own.
func TestFuturesOpen_ClientOrderIDParamIsRefusedBeforeAnythingIsSent(t *testing.T) {
	w := newLostWorld(t, true)
	for _, key := range []string{"clientOrderId", "clOrdId", "NewClientOrderId"} {
		res := w.open(t, nil, map[string]any{"params": map[string]interface{}{key: "mine"}})
		if !res.IsError || !strings.Contains(res.ForLLM, "client order ID") {
			t.Fatalf("%s: want a refusal, got %s", key, res.ForLLM)
		}
	}
	if entries, _, _, _, log := w.f.snapshot(); entries != 0 || len(log) != 0 || w.limiter.tokens() != 0 {
		t.Errorf("nothing may be sent or spent: entries=%d log=%v tokens=%d", entries, log, w.limiter.tokens())
	}
}

// Every gate still runs, in order, before the first mutation: a refusal at any of them
// sends nothing and spends nothing, and a placed order goes limiter, leverage, entry.
func TestFuturesOpen_EveryGateRunsBeforeTheFirstMutation(t *testing.T) {
	denied := gateConfig(config.ScopeMarketData)
	noLeverage := gateConfig(config.ScopeTrade)
	noLeverage.TradingRisk.AllowLeverage = false
	capped := gateConfig(config.ScopeTrade)
	capped.TradingRisk.MaxOrderValueUSD = 1
	limited := gateConfig(config.ScopeTrade)
	limited.TradingRisk.DailyLossLimitUSD = 10

	cases := []struct {
		name  string
		cfg   *config.Config
		deny  bool
		setup func(t *testing.T)
		want  string
	}{
		{"leverage switch", noLeverage, false, nil, "leverage"},
		{"permission", denied, false, nil, "permission"},
		{"daily loss", limited, false, func(t *testing.T) { swapLossTracker(t).RecordLoss(50) }, "loss"},
		{"max order value", capped, false, nil, "max_order_value_usd"},
		{"rate limit", gateConfig(config.ScopeTrade), true, nil, "rate limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newLostWorld(t, true)
			w.limiter.deny = tc.deny
			if tc.setup != nil {
				tc.setup(t)
			}
			res := w.open(t, tc.cfg, map[string]any{"account": "main"})
			if !res.IsError || !strings.Contains(strings.ToLower(res.ForLLM), tc.want) {
				t.Fatalf("want a refusal naming %q, got error=%v %s", tc.want, res.IsError, res.ForLLM)
			}
			if entries, _, _, _, log := w.f.snapshot(); entries != 0 || logHas(log, "leverage") {
				t.Errorf("a refused call mutated the exchange: entries=%d log=%v", entries, log)
			}
			if tc.name != "rate limit" && w.limiter.tokens() != 0 {
				t.Errorf("a refusal at a gate spent a token")
			}
		})
	}

	w := newLostWorld(t, true)
	if res := w.open(t, gateConfig(config.ScopeTrade), map[string]any{"account": "main"}); res.IsError {
		t.Fatalf("setup: %s", res.ForLLM)
	}
	_, _, _, _, log := w.f.snapshot()
	var mutations []string
	for _, l := range log {
		if l == "allow" || l == "leverage" || l == "entry" {
			mutations = append(mutations, l)
		}
	}
	if got := strings.Join(mutations, ","); got != "allow,leverage,entry" {
		t.Errorf("order of the rate limit and the mutations = %s, want allow,leverage,entry (log %v)", got, log)
	}
	if i := logIndex(log, "allow"); i < 0 || !logHas(log[:i], "positions") {
		t.Errorf("the position is read before the rate limit token is spent, log %v", log)
	}
}

func logIndex(log []string, op string) int {
	for i, l := range log {
		if l == op {
			return i
		}
	}
	return -1
}

// ---- the marker store ----

func TestUnknownEntries_PruneCapAndCompareDelete(t *testing.T) {
	resetUnknownEntries()
	t.Cleanup(resetUnknownEntries)
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	old := &unknownEntry{key: "old", since: base}
	putUnknownEntry(old, base)
	if getUnknownEntry("old", base.Add(futuresUnknownEntryWindow-time.Second)) == nil {
		t.Error("a marker inside the window must be held")
	}
	if getUnknownEntry("old", base.Add(futuresUnknownEntryWindow)) != nil {
		t.Error("a marker at the window's end must be gone")
	}

	now := base.Add(time.Hour)
	for i := 0; i < futuresUnknownEntryMax+50; i++ {
		putUnknownEntry(&unknownEntry{key: fmt.Sprintf("k%d", i), since: now.Add(time.Duration(i) * time.Millisecond)}, now)
	}
	if n := len(openUnknowns.m); n != futuresUnknownEntryMax {
		t.Errorf("held %d markers, want the cap %d", n, futuresUnknownEntryMax)
	}
	if getUnknownEntry("k0", now) != nil || getUnknownEntry(fmt.Sprintf("k%d", futuresUnknownEntryMax+49), now) == nil {
		t.Error("the cap must drop the oldest, never the newest")
	}

	first := &unknownEntry{key: "same", since: now}
	putUnknownEntry(first, now)
	second := &unknownEntry{key: "same", since: now.Add(time.Second)}
	putUnknownEntry(second, now)
	dropUnknownEntry(first)
	if getUnknownEntry("same", now) != second {
		t.Error("dropping a superseded marker must not remove its replacement")
	}
}

func TestUnknownEntries_ConcurrentUse(t *testing.T) {
	resetUnknownEntries()
	t.Cleanup(resetUnknownEntries)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("k%d", (g*7+i)%40)
				e := &unknownEntry{key: key, since: now}
				putUnknownEntry(e, now)
				getUnknownEntry(key, now)
				dropUnknownEntry(e)
			}
		}(g)
	}
	wg.Wait()
}

func TestPositionSnapshot_UnchangedFrom(t *testing.T) {
	a := positionSnapshot{"long": 1}
	if !a.unchangedFrom(positionSnapshot{"long": 1}) || !(positionSnapshot{}).unchangedFrom(positionSnapshot{}) {
		t.Error("equal snapshots are unchanged")
	}
	if a.unchangedFrom(positionSnapshot{"long": 1.1}) || a.unchangedFrom(positionSnapshot{"long": 1, "short": 2}) || (positionSnapshot{"short": 2}).unchangedFrom(a) {
		t.Error("a differing size, or another side appearing, is a change")
	}
	if !(positionSnapshot{"short": 0}).unchangedFrom(positionSnapshot{}) {
		t.Error("a side at zero is the same as no side")
	}
}
