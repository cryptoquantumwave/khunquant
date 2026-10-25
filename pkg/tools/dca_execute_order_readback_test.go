package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// dcaFakeProvider is a trading + market-data provider whose CreateOrder and
// FetchOrder responses are scripted per test.
type dcaFakeProvider struct {
	mockTradingProvider
	create       ccxt.Order
	fetchResults []dcaFetchResult // consumed in order; the last one repeats
	fetchCalls   int
	cancelCalls  int
	createCalls  int
	createdQty   float64
}

type dcaFetchResult struct {
	order ccxt.Order
	err   error
}

func (p *dcaFakeProvider) CreateOrder(_ context.Context, _, _, _ string, amount float64, _ *float64, _ map[string]any) (ccxt.Order, error) {
	p.createCalls++
	p.createdQty = amount
	return p.create, nil
}

func (p *dcaFakeProvider) FetchOrder(_ context.Context, _, _ string) (ccxt.Order, error) {
	i := p.fetchCalls
	p.fetchCalls++
	if len(p.fetchResults) == 0 {
		return ccxt.Order{}, errors.New("no scripted FetchOrder result")
	}
	if i >= len(p.fetchResults) {
		i = len(p.fetchResults) - 1
	}
	return p.fetchResults[i].order, p.fetchResults[i].err
}

func (p *dcaFakeProvider) CancelOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	p.cancelCalls++
	return ccxt.Order{Id: &id}, nil
}

func (p *dcaFakeProvider) FetchTicker(_ context.Context, symbol string) (ccxt.Ticker, error) {
	last := 100.0
	return ccxt.Ticker{Symbol: &symbol, Last: &last}, nil
}

func (p *dcaFakeProvider) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, nil
}

func (p *dcaFakeProvider) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	return nil, nil
}

func (p *dcaFakeProvider) FetchOrderBook(context.Context, string, int) (ccxt.OrderBook, error) {
	return ccxt.OrderBook{}, nil
}

func (p *dcaFakeProvider) LoadMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	return nil, nil
}

func dcaOrd(id, status string, filled float64) ccxt.Order {
	o := ccxt.Order{Id: &id}
	if status != "" {
		o.Status = &status
	}
	if filled > 0 {
		avg := 100.0
		o.Filled = &filled
		o.Average = &avg
	}
	return o
}

// dcaReadBackFixture registers fake under a unique provider name, seeds a
// plan buying 1 BTC-equivalent per run (AmountPerOrder 100 quote at price 100)
// and returns the tool, store and plan id. Each test uses its own provider name
// because the order rate limiter is keyed by provider.
func dcaReadBackFixture(t *testing.T, fake *dcaFakeProvider) (*ExecuteDCAOrderTool, *dca.Store, int64, *int) {
	t.Helper()
	name := "dca-readback-" + strings.ReplaceAll(strings.ToLower(t.Name()), "/", "-")
	broker.RegisterFactory(name, func(*config.Config) (broker.Provider, error) { return fake, nil })

	store := newTestDCAStore(t)
	now := time.Now().UTC()
	planID, err := store.SavePlan(context.Background(), &dca.Plan{
		Name: "ReadBack", Provider: name, Symbol: "BTC/THB", AmountPerOrder: 100,
		FrequencyExpr: "0 9 * * 1", Timezone: "UTC", Enabled: true, Side: "buy",
		StartDate: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	tool := NewExecuteDCAOrderTool(config.DefaultConfig(), store)
	sleeps := new(int)
	prevSleep := dcaSleep
	dcaSleep = func(context.Context, time.Duration) { *sleeps++ }
	t.Cleanup(func() { dcaSleep = prevSleep })
	return tool, store, planID, sleeps
}

func dcaRun(t *testing.T, tool *ExecuteDCAOrderTool, planID int64) *ToolResult {
	t.Helper()
	return tool.Execute(testCtx(), map[string]any{"plan_id": float64(planID)})
}

func dcaLastExec(t *testing.T, store *dca.Store, planID int64) dca.Execution {
	t.Helper()
	id := planID
	execs, err := store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &id, Limit: 10})
	if err != nil || len(execs) != 1 {
		t.Fatalf("GetExecutions: got %d rows, err %v; want exactly 1", len(execs), err)
	}
	return execs[0]
}

func dcaPlanStats(t *testing.T, store *dca.Store, planID int64) (invested, qty float64) {
	t.Helper()
	p, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	return p.TotalInvested, p.TotalQuantity
}

func TestExecuteDCAOrder_ImmediateFullFill_NoReadBack(t *testing.T) {
	fake := &dcaFakeProvider{create: dcaOrd("o-1", "closed", 1)}
	tool, store, planID, sleeps := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if fake.fetchCalls != 0 || *sleeps != 0 {
		t.Errorf("FetchOrder calls = %d, sleeps = %d; want 0 and 0 for an immediate full fill", fake.fetchCalls, *sleeps)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "completed" || e.FilledQuantity != 1 || e.OrderID != "o-1" {
		t.Errorf("execution = %+v; want completed, qty 1, order o-1", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 100 || q != 1 {
		t.Errorf("stats = (%v, %v); want (100, 1)", inv, q)
	}
}

func TestExecuteDCAOrder_OpenThenFilledOnReadBack(t *testing.T) {
	fake := &dcaFakeProvider{
		create: dcaOrd("o-2", "open", 0),
		fetchResults: []dcaFetchResult{
			{order: dcaOrd("o-2", "open", 0)},
			{order: dcaOrd("o-2", "closed", 0.9)},
		},
	}
	tool, store, planID, sleeps := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if fake.fetchCalls != 2 {
		t.Errorf("FetchOrder calls = %d; want 2 (stop once filled)", fake.fetchCalls)
	}
	if *sleeps != 1 {
		t.Errorf("sleeps = %d; want 1 between the two reads", *sleeps)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "completed" || e.FilledQuantity != 0.9 {
		t.Errorf("execution = %+v; want completed with the actual qty 0.9 (requested 1)", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); q != 0.9 || inv != 90 {
		t.Errorf("stats = (%v, %v); want (90, 0.9)", inv, q)
	}
}

func TestExecuteDCAOrder_StillOpenAfterRetries_Pending(t *testing.T) {
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-3", "open", 0),
		fetchResults: []dcaFetchResult{{order: dcaOrd("o-3", "open", 0)}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("pending must not be an error result (a retry would double-buy): %s", res.ForLLM)
	}
	if fake.fetchCalls != dcaReadBackTries {
		t.Errorf("FetchOrder calls = %d; want %d", fake.fetchCalls, dcaReadBackTries)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelOrder called %d times; the order must never be cancelled automatically", fake.cancelCalls)
	}
	for _, want := range []string{"o-3", "still", "filled 0 of 1"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result missing %q:\n%s", want, res.ForLLM)
		}
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "pending" || e.OrderID != "o-3" || e.FilledQuantity != 0 {
		t.Errorf("execution = %+v; want pending, order o-3, qty 0", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 0 || q != 0 {
		t.Errorf("stats = (%v, %v); want unchanged (0, 0)", inv, q)
	}
}

func TestExecuteDCAOrder_PartialFillStillOpen_RecordsOnlyFilled(t *testing.T) {
	part := dcaOrd("o-4", "open", 0.4)
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-4", "open", 0),
		fetchResults: []dcaFetchResult{{order: part}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "filled 0.4 of 1") {
		t.Errorf("result should say filled 0.4 of 1:\n%s", res.ForLLM)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "pending" || e.FilledQuantity != 0.4 {
		t.Errorf("execution = %+v; want pending with qty 0.4", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); q != 0.4 || inv != 40 {
		t.Errorf("stats = (%v, %v); want only the filled part (40, 0.4)", inv, q)
	}
}

func TestExecuteDCAOrder_ReadBackErrors_Unknown(t *testing.T) {
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-5", "open", 0),
		fetchResults: []dcaFetchResult{{err: errors.New("boom")}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unknown must not be an error result (a retry would double-buy): %s", res.ForLLM)
	}
	if fake.fetchCalls != dcaReadBackTries {
		t.Errorf("FetchOrder calls = %d; want %d", fake.fetchCalls, dcaReadBackTries)
	}
	if fake.cancelCalls != 0 {
		t.Errorf("CancelOrder called %d times; want 0", fake.cancelCalls)
	}
	for _, want := range []string{"o-5", "boom", "check it on the exchange"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result missing %q:\n%s", want, res.ForLLM)
		}
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "unknown" || e.OrderID != "o-5" || e.FilledQuantity != 0 || e.AmountQuote != 0 {
		t.Errorf("execution = %+v; want unknown, order o-5, nothing filled", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 0 || q != 0 {
		t.Errorf("stats = (%v, %v); want unchanged (0, 0)", inv, q)
	}
}

func TestExecuteDCAOrder_NoOrderID_Unknown(t *testing.T) {
	fake := &dcaFakeProvider{create: ccxt.Order{}}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("FetchOrder calls = %d; want 0 without an order id", fake.fetchCalls)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "unknown" || e.FilledQuantity != 0 {
		t.Errorf("execution = %+v; want unknown, nothing filled", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 0 || q != 0 {
		t.Errorf("stats = (%v, %v); want unchanged", inv, q)
	}
}

func TestExecuteDCAOrder_ClosedWithoutFill_NotTrusted(t *testing.T) {
	// "closed" but no Filled figure must not be treated as a fill, in either
	// the create response or the read-back.
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-6", "closed", 0),
		fetchResults: []dcaFetchResult{{order: dcaOrd("o-6", "closed", 0)}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "unknown" || e.FilledQuantity != 0 {
		t.Errorf("execution = %+v; want unknown, nothing filled", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 0 || q != 0 {
		t.Errorf("stats = (%v, %v); want unchanged", inv, q)
	}
}

func TestExecuteDCAOrder_RejectedOnReadBack_Failed(t *testing.T) {
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-7", "open", 0),
		fetchResults: []dcaFetchResult{{order: dcaOrd("o-7", "rejected", 0)}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if !res.IsError {
		t.Fatalf("a rejected order with no fill should be an error result:\n%s", res.ForLLM)
	}
	if fake.fetchCalls != 1 {
		t.Errorf("FetchOrder calls = %d; want 1 (terminal state stops polling)", fake.fetchCalls)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "failed" || e.OrderID != "o-7" {
		t.Errorf("execution = %+v; want failed with order o-7", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); inv != 0 || q != 0 {
		t.Errorf("stats = (%v, %v); want unchanged", inv, q)
	}
}

func TestExecuteDCAOrder_RejectedInCreateResponse_Failed(t *testing.T) {
	fake := &dcaFakeProvider{create: dcaOrd("o-8", "canceled", 0)}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if !res.IsError {
		t.Fatalf("expected error result:\n%s", res.ForLLM)
	}
	if fake.fetchCalls != 0 {
		t.Errorf("FetchOrder calls = %d; want 0 when the create response is already terminal", fake.fetchCalls)
	}
	if e := dcaLastExec(t, store, planID); e.Status != "failed" {
		t.Errorf("status = %q; want failed", e.Status)
	}
}

func TestExecuteDCAOrder_CanceledAfterPartialFill_RecordsFilled(t *testing.T) {
	fake := &dcaFakeProvider{
		create:       dcaOrd("o-9", "open", 0),
		fetchResults: []dcaFetchResult{{order: dcaOrd("o-9", "canceled", 0.3)}},
	}
	tool, store, planID, _ := dcaReadBackFixture(t, fake)

	res := dcaRun(t, tool, planID)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	e := dcaLastExec(t, store, planID)
	if e.Status != "completed" || e.FilledQuantity != 0.3 {
		t.Errorf("execution = %+v; want completed with the partial qty 0.3", e)
	}
	if inv, q := dcaPlanStats(t, store, planID); q != 0.3 || inv != 30 {
		t.Errorf("stats = (%v, %v); want (30, 0.3)", inv, q)
	}
}
