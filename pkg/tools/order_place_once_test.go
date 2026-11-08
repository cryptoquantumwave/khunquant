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
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ---- a spot venue that can lose the answer to an order ----

// lostSpot is a priced spot venue (dcaGateMock) whose CreateOrder can book the order and
// then lose the answer, the case create_order and the scheduled DCA run used to report as
// a plain failure. Nothing here talks to an exchange.
type lostSpot struct {
	*dcaGateMock
	mu sync.Mutex

	lost      bool  // the call returns a timeout instead of the order
	booked    bool  // the venue created the order even though the answer is lost
	hidden    bool  // neither a lookup nor a listing shows a booked order (lag)
	rejectErr error // the call is refused with this and books nothing

	creates    int
	params     []map[string]interface{}
	book       []ccxt.Order
	lookups    int
	lookupCIDs []string
}

func (s *lostSpot) CreateOrder(_ context.Context, symbol, orderType, side string, amount float64, _ *float64, params map[string]interface{}) (ccxt.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	cp := map[string]interface{}{}
	for k, v := range params {
		cp[k] = v
	}
	s.params = append(s.params, cp)
	if s.rejectErr != nil {
		return ccxt.Order{}, s.rejectErr
	}
	id := fmt.Sprintf("spot-%d", s.creates)
	cid, _ := params["clientOrderId"].(string)
	filled, status := amount, "closed"
	o := ccxt.Order{Id: &id, ClientOrderId: &cid, Symbol: &symbol, Side: &side, Type: &orderType, Amount: &amount, Filled: &filled, Status: &status}
	if !s.lost || s.booked {
		s.book = append(s.book, o)
	}
	if s.lost {
		return ccxt.Order{}, &ccxt.Error{Type: "RequestTimeout", Message: "read tcp: i/o timeout SECRET-BODY-5c1"}
	}
	return o, nil
}

// FetchClosedOrders lists the booked orders, which is where a shape match finds a filled one.
func (s *lostSpot) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hidden {
		return nil, nil
	}
	return append([]ccxt.Order(nil), s.book...), nil
}

func (s *lostSpot) counts() (creates, lookups int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates, s.lookups
}

// lostSpotCap is lostSpot for an adapter that takes a client order ID and looks it up.
type lostSpotCap struct{ *lostSpot }

func (lostSpotCap) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (c lostSpotCap) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	s := c.lostSpot
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups++
	s.lookupCIDs = append(s.lookupCIDs, cid)
	if s.hidden {
		return ccxt.Order{}, false, nil
	}
	for _, o := range s.book {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

// installLostSpot serves s under its mock's provider ID (capable: with client order IDs and
// a lookup by them) until the test ends, with a short settle delay before the look.
func installLostSpot(t *testing.T, s *lostSpot, capable bool) {
	t.Helper()
	broker.RegisterFactory(s.id, func(*config.Config) (broker.Provider, error) {
		if capable {
			return lostSpotCap{s}, nil
		}
		return s, nil
	})
	prev := futuresOpenSettle
	futuresOpenSettle = testSettle
	t.Cleanup(func() {
		futuresOpenSettle = prev
		broker.RegisterFactory(s.id, func(*config.Config) (broker.Provider, error) {
			return nil, errors.New("broker: provider " + s.id + " not registered")
		})
		broker.ResetUSDRateCache()
	})
}

func newLostSpot(t *testing.T, id string, capable bool) *lostSpot {
	t.Helper()
	s := &lostSpot{dcaGateMock: &dcaGateMock{id: id, last: 50000}, booked: true}
	installLostSpot(t, s, capable)
	return s
}

func spotBuy(provider string) map[string]any {
	return map[string]any{"provider": provider, "symbol": "BTC/USDT", "type": "market", "side": "buy", "amount": 0.001, "confirm": true}
}

// assertOrderOutcomeUnknown: an error result that carries broker.ErrOutcomeUnknown, says
// the order may exist and names its client order ID, and is not called a failure.
func assertOrderOutcomeUnknown(t *testing.T, res *ToolResult) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("an order whose outcome is unknown must be an error result: %s", res.ForLLM)
	}
	if !errors.Is(res.Err, broker.ErrOutcomeUnknown) {
		t.Errorf("the result must carry broker.ErrOutcomeUnknown, got %v", res.Err)
	}
	for _, want := range []string{"ORDER OUTCOME UNKNOWN", "may exist", "Client order ID: "} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the result must say %q: %s", want, res.ForLLM)
		}
	}
	plainWords(t, res)
}

// ---- create_order ----

// The answer is lost, the order is on the exchange, and the lookup by client order ID finds
// it: one order, one token, and the result names the order and says the answer was lost.
func TestCreateOrder_LostAnswer_FoundByClientID(t *testing.T) {
	s := newLostSpot(t, "lost-spot-found", true)
	s.lost = true
	lim := broker.NewTokenBucketLimiter(2, time.Hour)
	installDCALimiter(t, lim)

	res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), spotBuy(s.id))

	if res.IsError {
		t.Fatalf("a found order carries on as a success, got: %s", res.ForLLM)
	}
	for _, want := range []string{"spot-1", "answer to this order was lost", "found it by its client order ID kq"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("the result must say %q: %s", want, res.ForLLM)
		}
	}
	creates, lookups := s.counts()
	if creates != 1 || lookups != 1 {
		t.Fatalf("creates=%d lookups=%d, want one order and one look", creates, lookups)
	}
	cid, _ := s.params[0]["clientOrderId"].(string)
	if !strings.HasPrefix(cid, "kq") || len(cid) > 32 || len(s.lookupCIDs) != 1 || s.lookupCIDs[0] != cid {
		t.Errorf("the look must use the order's client order ID %q (kq..., at most 32), looked up %v", cid, s.lookupCIDs)
	}
	if st := lim.Status()[s.id]; st.TokensLeft != 1 {
		t.Errorf("tokens left = %d, want 1 (one order, and a look spends none)", st.TokensLeft)
	}
	if strings.Contains(res.ForLLM, "SECRET-BODY") {
		t.Errorf("the result carries raw exchange text: %s", res.ForLLM)
	}
}

// Not found: one order, no second create, and an unknown outcome that says what to check.
func TestCreateOrder_LostAnswer_NotFound_IsUnknownWithNoSecondCreate(t *testing.T) {
	s := newLostSpot(t, "lost-spot-hidden", true)
	s.lost, s.hidden = true, true

	res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), spotBuy(s.id))

	assertOrderOutcomeUnknown(t, res)
	if !strings.Contains(res.ForLLM, NameGetOpenOrders) || !strings.Contains(res.ForLLM, "Client order ID: kq") {
		t.Errorf("the result must say where to look and name the client order ID: %s", res.ForLLM)
	}
	if creates, lookups := s.counts(); creates != 1 || lookups != 1 {
		t.Errorf("creates=%d lookups=%d, want 1 and 1", creates, lookups)
	}
}

// An adapter with no client order ID: the order is found by its shape among the closed
// orders, and no client order ID is sent to it.
func TestCreateOrder_LostAnswer_NotCapable_FoundByShape(t *testing.T) {
	s := newLostSpot(t, "lost-spot-shape", false)
	s.lost = true

	res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), spotBuy(s.id))

	if res.IsError || !strings.Contains(res.ForLLM, "found it by its shape") {
		t.Fatalf("a booked order must be found by its shape: %s", res.ForLLM)
	}
	if creates, _ := s.counts(); creates != 1 {
		t.Errorf("creates = %d, want 1", creates)
	}
	if _, ok := s.params[0]["clientOrderId"]; ok {
		t.Error("an adapter that cannot take a client order ID must not be sent one")
	}
}

// The exchange's own refusal stays a plain failure: no wait, no look.
func TestCreateOrder_DefiniteRejection_IsPlainFailure(t *testing.T) {
	s := newLostSpot(t, "lost-spot-reject", true)
	s.rejectErr = &ccxt.Error{Type: "InsufficientFunds", Message: "balance too low"}

	start := time.Now()
	res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), spotBuy(s.id))

	if !res.IsError || !strings.Contains(res.ForLLM, "CreateOrder failed") || errors.Is(res.Err, broker.ErrOutcomeUnknown) {
		t.Fatalf("a definite rejection is a plain failure: %s (%v)", res.ForLLM, res.Err)
	}
	if creates, lookups := s.counts(); creates != 1 || lookups != 0 {
		t.Errorf("creates=%d lookups=%d, want 1 and no look", creates, lookups)
	}
	if elapsed := time.Since(start); elapsed >= testSettle {
		t.Errorf("a definite rejection must not wait out the settle delay, took %s", elapsed)
	}
}

// A Webull session that needs a login refuses before it sends (a spot adapter returns
// exchanges.ErrNeedsReauth, here wrapped). That is not a lost answer: the tool gives the
// reconnect instruction at once, with no wait and no look, and reads as no unknown outcome.
func TestCreateOrder_NeedsReauth_IsTheReconnectHintNotUnknown(t *testing.T) {
	s := newLostSpot(t, "lost-spot-reauth", true)
	s.rejectErr = fmt.Errorf("webull: place order: %w", exchanges.ErrNeedsReauth)

	start := time.Now()
	res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), spotBuy(s.id))

	if !res.IsError || !strings.Contains(res.ForLLM, NameWebullReconnect) || strings.Contains(res.ForLLM, "OUTCOME UNKNOWN") {
		t.Fatalf("a re-authentication need must give the reconnect hint, not an unknown outcome: %s", res.ForLLM)
	}
	if creates, lookups := s.counts(); creates != 1 || lookups != 0 {
		t.Errorf("creates=%d lookups=%d, want 1 and no look", creates, lookups)
	}
	if elapsed := time.Since(start); elapsed >= testSettle {
		t.Errorf("a re-authentication need must not wait out the settle delay, took %s", elapsed)
	}
}

// A client order ID in params is refused before anything is sent or spent: the tool sets its own.
func TestCreateOrder_ClientOrderIDParamIsRefusedBeforeAnythingIsSent(t *testing.T) {
	s := newLostSpot(t, "lost-spot-cid", true)
	lim := broker.NewTokenBucketLimiter(5, time.Hour)
	installDCALimiter(t, lim)
	for _, key := range []string{"clientOrderId", "clOrdId", "NewClientOrderId"} {
		args := spotBuy(s.id)
		args["params"] = map[string]any{key: "mine"}
		res := NewCreateOrderTool(config.DefaultConfig()).Execute(context.Background(), args)
		if !res.IsError || !strings.Contains(res.ForLLM, "client order ID") || !strings.Contains(res.ForLLM, "Nothing was sent") {
			t.Fatalf("%s: want a refusal, got %s", key, res.ForLLM)
		}
	}
	if creates, _ := s.counts(); creates != 0 {
		t.Errorf("creates = %d, want nothing sent", creates)
	}
	if st := lim.Status(); len(st) != 0 {
		t.Errorf("a refusal spent a rate limit token: %+v", st)
	}
}

// Paper mode places in the simulation, in this process, as before: the venue's CreateOrder
// is never reached, so its lost answer cannot matter.
func TestCreateOrder_PaperMode_StillSimulates(t *testing.T) {
	s := newLostSpot(t, "lost-spot-paper", true)
	s.lost = true
	cfg := config.DefaultConfig()
	cfg.TradingRisk.PaperTradingMode = true

	res := NewCreateOrderTool(cfg).Execute(context.Background(), spotBuy(s.id))

	if res.IsError || !strings.Contains(res.ForLLM, "[PAPER]") {
		t.Fatalf("paper mode must simulate the order: %s", res.ForLLM)
	}
	if creates, lookups := s.counts(); creates != 0 || lookups != 0 {
		t.Errorf("creates=%d lookups=%d, want the venue untouched", creates, lookups)
	}
}

// ---- the scheduled DCA run ----

// Found: the run records the fill of the order it found, once.
func TestDCAExecute_LostAnswer_FoundByClientID_RecordsTheFill(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "lost-dca-found")
	s := &lostSpot{dcaGateMock: mock, lost: true, booked: true}
	installLostSpot(t, s, true)

	res := runDCA(config.DefaultConfig(), store, planID)

	if res.IsError || !strings.Contains(res.ForLLM, "answer to this order was lost") {
		t.Fatalf("a found order is recorded as the run's order: %s", res.ForLLM)
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != dcaStatusCompleted || rows[0].OrderID != "spot-1" || rows[0].FilledQuantity <= 0 {
		t.Fatalf("rows = %+v, want one completed row for spot-1", rows)
	}
	if creates, lookups := s.counts(); creates != 1 || lookups != 1 {
		t.Errorf("creates=%d lookups=%d, want 1 and 1", creates, lookups)
	}
}

// Not found: an "unknown" row, nothing recorded as bought, and the next run in the same
// period is held back by the period cap, so the plan cannot buy twice.
func TestDCAExecute_LostAnswer_NotFound_IsUnknownAndHoldsThePeriod(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "lost-dca-hidden")
	setDCAPlan(t, store, planID, func(p *dca.Plan) { p.MaxExecPerPeriod = 1; p.ExecPeriod = "day" })
	s := &lostSpot{dcaGateMock: mock, lost: true, booked: true, hidden: true}
	installLostSpot(t, s, true)
	cfg := config.DefaultConfig()

	res := runDCA(cfg, store, planID)

	assertOrderOutcomeUnknown(t, res)
	if !strings.Contains(res.ForLLM, "Do not re-run this plan") {
		t.Errorf("the result must say not to run the plan again for the period: %s", res.ForLLM)
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != dcaStatusUnknown || rows[0].OrderID != "" || rows[0].FilledQuantity != 0 ||
		!strings.Contains(rows[0].ErrorMsg, "ORDER OUTCOME UNKNOWN") {
		t.Fatalf("rows = %+v, want one unknown row with nothing bought", rows)
	}
	if creates, _ := s.counts(); creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}

	s.lost = false
	res = runDCA(cfg, store, planID)
	if res.IsError || !strings.Contains(res.ForLLM, "1/1") {
		t.Fatalf("the next run in the period must be held by the cap: %s", res.ForLLM)
	}
	if creates, _ := s.counts(); creates != 1 {
		t.Errorf("creates = %d, want no second order in the period", creates)
	}
}

// ---- the futures exits ----

// lostExit is a futures venue holding long positions whose reduce-only exits can be booked
// and then lose the answer, the case the close, reduce and flatten tools used to report as
// a plain failure.
type lostExit struct {
	*mockFuturesProvider
	mu sync.Mutex

	lost      map[string]bool // symbols whose exit answer is lost
	lostErr   error           // what a lost exit returns (default: a ccxt RequestTimeout)
	booked    bool            // a lost exit was created all the same
	hidden    bool            // a lookup does not show a booked exit
	positions map[string]float64

	creates []broker.FuturesOrderRequest
	book    []ccxt.Order
	lookups int
}

func (f *lostExit) CreateFuturesOrder(_ context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	id := fmt.Sprintf("exit-%d", len(f.creates))
	cid, _ := req.Params["clientOrderId"].(string)
	sym, side, typ, amt := req.Symbol, req.Side, req.OrderType, req.Amount
	filled, status := amt, "closed"
	o := ccxt.Order{Id: &id, ClientOrderId: &cid, Symbol: &sym, Side: &side, Type: &typ, Amount: &amt, Filled: &filled, Status: &status}
	lost := f.lost[req.Symbol]
	if !lost || f.booked {
		f.book = append(f.book, o)
		f.positions[req.Symbol] -= req.Amount
	}
	if lost {
		if f.lostErr != nil {
			return ccxt.Order{}, f.lostErr
		}
		return ccxt.Order{}, &ccxt.Error{Type: "RequestTimeout", Message: "read tcp: i/o timeout SECRET-BODY-7d2"}
	}
	return o, nil
}

func (f *lostExit) FetchFuturesOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.book {
		if *o.Id == id {
			return o, nil
		}
	}
	return ccxt.Order{}, errors.New("no such order")
}

func (f *lostExit) FetchFuturesPositions(_ context.Context, _ []string) ([]ccxt.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ccxt.Position
	for sym, c := range f.positions {
		if c <= 1e-12 {
			continue
		}
		s, side, n := sym, "long", c
		out = append(out, ccxt.Position{Symbol: &s, Side: &side, Contracts: &n})
	}
	return out, nil
}

func (f *lostExit) snapshot() (creates []broker.FuturesOrderRequest, lookups int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]broker.FuturesOrderRequest(nil), f.creates...), f.lookups
}

// lostExitCap is lostExit for an adapter that takes a client order ID and looks it up.
type lostExitCap struct{ *lostExit }

func (lostExitCap) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (c lostExitCap) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	f := c.lostExit
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.hidden {
		return ccxt.Order{}, false, nil
	}
	for _, o := range f.book {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

// newLostExit installs a venue holding the given long positions, whose exits on the lost
// symbols lose their answer (booked unless the test says otherwise).
func newLostExit(t *testing.T, capable bool, positions map[string]float64, lost ...string) *lostExit {
	t.Helper()
	f := &lostExit{mockFuturesProvider: &mockFuturesProvider{}, lost: map[string]bool{}, booked: true, positions: positions}
	for _, s := range lost {
		f.lost[s] = true
	}
	origFn, origSettle := futuresProviderFn, futuresOpenSettle
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		if capable {
			return lostExitCap{f}, nil
		}
		return f, nil
	}
	futuresOpenSettle = testSettle
	t.Cleanup(func() { futuresProviderFn, futuresOpenSettle = origFn, origSettle })
	return f
}

const exitSym = "BTC/USDT:USDT"

func closeArgs() map[string]any {
	return map[string]any{"provider": "binance", "symbol": exitSym, "confirm": true}
}

func TestFuturesClose_LostAnswer_FoundByClientID(t *testing.T) {
	f := newLostExit(t, true, map[string]float64{exitSym: 0.5}, exitSym)

	res := NewFuturesClosePositionTool(seqCfg()).Execute(context.Background(), closeArgs())

	if res.IsError {
		t.Fatalf("a found close carries on as a success, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "exit-1") || !strings.Contains(res.ForLLM, "found it by its client order ID kq") {
		t.Errorf("the result must name the found order and how it was found: %s", res.ForLLM)
	}
	creates, lookups := f.snapshot()
	if len(creates) != 1 || lookups != 1 {
		t.Fatalf("creates=%d lookups=%d, want one exit and one look", len(creates), lookups)
	}
	c := creates[0]
	if cid, _ := c.Params["clientOrderId"].(string); cid == "" || !c.ReduceOnly || c.Side != "sell" || c.Amount != 0.5 {
		t.Errorf("the exit must be a reduce-only sell of the whole 0.5 with a client order ID, got %+v", c)
	}
}

func TestFuturesClose_LostAnswer_NotFound_IsUnknownWithNoSecondCreate(t *testing.T) {
	f := newLostExit(t, true, map[string]float64{exitSym: 0.5}, exitSym)
	f.hidden = true

	res := NewFuturesClosePositionTool(seqCfg()).Execute(context.Background(), closeArgs())

	assertOrderOutcomeUnknown(t, res)
	if !strings.Contains(res.ForLLM, NameFuturesGetPositions) || !strings.Contains(res.ForLLM, "reduce-only") {
		t.Errorf("the result must say the order is reduce-only and to check the positions: %s", res.ForLLM)
	}
	if creates, lookups := f.snapshot(); len(creates) != 1 || lookups != 1 {
		t.Errorf("creates=%d lookups=%d, want 1 and 1", len(creates), lookups)
	}
}

// On Binance mainnet ccxt classifies USDⓈ-M errors with its common table only, so a
// duplicate client order ID (-4116) and a rejected reduce-only order (-2022) both arrive as
// ExchangeError, not as a definite rejection. Neither is trusted: each is an unknown outcome
// whose first step is the lookup by client order ID. A booked order is found and used; an
// order that was not booked is reported as unknown, never as placed, and never placed again.
func TestFuturesClose_MainnetExchangeErrorIsLookedUpByClientID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		booked bool
	}{
		{"-4116 duplicate client order ID", &ccxt.Error{Type: "ExchangeError", Message: `binanceusdm {"code":-4116,"msg":"ClientOrderId is duplicated."}`}, true},
		{"-2022 reduce-only rejected", &ccxt.Error{Type: "ExchangeError", Message: `binanceusdm {"code":-2022,"msg":"ReduceOnly Order is rejected."}`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLostExit(t, true, map[string]float64{exitSym: 0.5}, exitSym)
			f.lostErr, f.booked = tc.err, tc.booked

			res := NewFuturesClosePositionTool(seqCfg()).Execute(context.Background(), closeArgs())

			if tc.booked {
				if res.IsError || !strings.Contains(res.ForLLM, "found it by its client order ID kq") {
					t.Fatalf("a booked order behind an ExchangeError must be found by its client order ID: %s", res.ForLLM)
				}
			} else {
				assertOrderOutcomeUnknown(t, res)
			}
			if creates, lookups := f.snapshot(); len(creates) != 1 || lookups != 1 {
				t.Errorf("creates=%d lookups=%d, want one exit and one look by client order ID", len(creates), lookups)
			}
		})
	}
}

// An exit is sent even when the turn was already cancelled, as it always was: a close the
// tool set out to make is not left undone by the cancel.
func TestFuturesClose_CancelledTurnStillSendsTheExit(t *testing.T) {
	f := newLostExit(t, true, map[string]float64{exitSym: 0.5})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewFuturesClosePositionTool(seqCfg()).Execute(ctx, closeArgs())

	if res.IsError {
		t.Fatalf("the close must still be sent: %s", res.ForLLM)
	}
	if creates, _ := f.snapshot(); len(creates) != 1 {
		t.Errorf("creates = %d, want the one exit", len(creates))
	}
}

// An adapter with no client order ID: the position shrank by about the order, so the
// reduce most likely went through. It is still an unknown outcome, never a second order.
func TestFuturesReduce_LostAnswer_NotCapable_PositionMoved_IsUnknown(t *testing.T) {
	f := newLostExit(t, false, map[string]float64{exitSym: 0.5}, exitSym)

	res := NewFuturesReducePositionTool(seqCfg()).Execute(context.Background(), map[string]any{
		"provider": "binance", "symbol": exitSym, "amount": 0.2, "confirm": true,
	})

	assertOrderOutcomeUnknown(t, res)
	if !strings.Contains(res.ForLLM, "most likely went through") {
		t.Errorf("a position that shrank by the order must be said to have most likely gone through: %s", res.ForLLM)
	}
	creates, _ := f.snapshot()
	if len(creates) != 1 || creates[0].Amount != 0.2 {
		t.Fatalf("creates = %+v, want one reduce of 0.2", creates)
	}
	if _, ok := creates[0].Params["clientOrderId"]; ok {
		t.Error("an adapter that cannot take a client order ID must not be sent one")
	}
}

// Flatten sends every close once, then looks for the lost ones after a single wait: the
// other positions are closed as before, and a lost close is found or reported as unknown.
func TestFuturesFlatten_LostAnswer_LooksAfterTheLoop(t *testing.T) {
	const eth = "ETH/USDT:USDT"
	for _, tc := range []struct {
		name   string
		hidden bool
		want   string
	}{
		{"found", false, "(found after the lost answer; nothing was placed twice)"},
		{"not found", true, "OUTCOME UNKNOWN — the close order may exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLostExit(t, true, map[string]float64{exitSym: 0.5, eth: 2}, exitSym)
			f.hidden = tc.hidden

			res := NewFuturesEmergencyFlattenTool(seqCfg()).Execute(context.Background(), map[string]any{"provider": "binance", "confirm": true})

			if res.IsError {
				t.Fatalf("flatten reports per position: %s", res.ForLLM)
			}
			if !strings.Contains(res.ForLLM, tc.want) || !strings.Contains(res.ForLLM, "long "+eth+": order exit-") {
				t.Errorf("want %q for the lost close and the other position closed: %s", tc.want, res.ForLLM)
			}
			if strings.Contains(res.ForLLM, "FAILED") || strings.Contains(res.ForLLM, "SECRET-BODY") {
				t.Errorf("a lost answer is not a failure and carries no exchange text: %s", res.ForLLM)
			}
			creates, lookups := f.snapshot()
			if len(creates) != 2 || lookups != 1 {
				t.Fatalf("creates=%d lookups=%d, want one close per position and one look", len(creates), lookups)
			}
		})
	}
}
