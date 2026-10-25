package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

type seqCtxKey struct{}

// seqCall is one mutating call the fake received, with the state of the context it
// was handed at that moment.
type seqCall struct {
	op        string
	reduce    bool
	ctxErr    error
	ctxMarker any
	hasDL     bool
}

// seqFuturesProvider records every mutating call and can cancel the turn's context
// right after a chosen call returns, which is how a real cancel lands between the
// calls of a sequence.
type seqFuturesProvider struct {
	*mockFuturesProvider
	mu       sync.Mutex
	calls    []seqCall
	cancelOn string // op after which the turn context is cancelled
	cancel   context.CancelFunc
	open     []ccxt.Order
}

func (f *seqFuturesProvider) record(ctx context.Context, op string, reduce bool) {
	f.mu.Lock()
	_, hasDL := ctx.Deadline()
	f.calls = append(f.calls, seqCall{op: op, reduce: reduce, ctxErr: ctx.Err(), ctxMarker: ctx.Value(seqCtxKey{}), hasDL: hasDL})
	cancelNow := f.cancelOn == op && f.cancel != nil
	if cancelNow {
		f.cancelOn = "" // cancel once
	}
	f.mu.Unlock()
	if cancelNow {
		f.cancel()
	}
}

func (f *seqFuturesProvider) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.op)
	}
	return out
}

func (f *seqFuturesProvider) SetFuturesLeverage(ctx context.Context, _ string, _ int64, _, _ string) (map[string]interface{}, error) {
	f.record(ctx, "leverage", false)
	return nil, nil
}

func (f *seqFuturesProvider) CreateFuturesOrder(ctx context.Context, req broker.FuturesOrderRequest) (ccxt.Order, error) { //nolint:gocritic
	op := "entry"
	if req.ReduceOnly {
		op = "reduce_only"
	}
	f.record(ctx, op, req.ReduceOnly)
	id := "ord-" + op
	return ccxt.Order{Id: &id}, nil
}

func (f *seqFuturesProvider) FetchFuturesOpenOrders(_ context.Context, _ string) ([]ccxt.Order, error) {
	return f.open, nil
}

func (f *seqFuturesProvider) CancelFuturesOrder(ctx context.Context, _, _ string) (ccxt.Order, error) {
	f.record(ctx, "cancel", false)
	return ccxt.Order{}, nil
}

func (f *seqFuturesProvider) CancelAllFuturesOrders(ctx context.Context, _ string) ([]ccxt.Order, error) {
	f.record(ctx, "cancel_all", false)
	return nil, nil
}

func newSeqFuturesProvider(t *testing.T) *seqFuturesProvider {
	t.Helper()
	sym := "BTC/USDT:USDT"
	contracts := 0.01
	side := "long"
	active, swap := true, true
	minAmt, maxLev := 0.001, 100.0
	f := &seqFuturesProvider{mockFuturesProvider: &mockFuturesProvider{
		markPriceFn: func(context.Context, string) (float64, error) { return 50000, nil },
		loadMarketsFn: func(context.Context) (map[string]ccxt.MarketInterface, error) {
			return map[string]ccxt.MarketInterface{sym: {
				Active: &active, Swap: &swap,
				Limits: ccxt.Limits{Amount: ccxt.MinMax{Min: &minAmt}, Leverage: ccxt.MinMax{Max: &maxLev}},
			}}, nil
		},
		fetchFuturesPositionsFn: func(context.Context, []string) ([]ccxt.Position, error) {
			return []ccxt.Position{{Symbol: &sym, Contracts: &contracts, Side: &side}}, nil
		},
	}}
	origFn, origLimiter := futuresProviderFn, broker.DefaultLimiter
	futuresProviderFn = func(context.Context, *config.Config, string, string) (broker.FuturesProvider, error) {
		return f, nil
	}
	broker.DefaultLimiter = unlimitedLimiter{}
	t.Cleanup(func() { futuresProviderFn, broker.DefaultLimiter = origFn, origLimiter })
	return f
}

func seqCfg() *config.Config {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.AllowLeverage = true
	return cfg
}

func seqOpenArgs() map[string]any {
	return map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "side": "long",
		"amount": 0.01, "leverage": 5.0, "stop_loss": 45000.0, "take_profit": 60000.0, "confirm": true,
	}
}

func TestFuturesOpenPosition_CancelAfterEntryStillPlacesProtection(t *testing.T) {
	f := newSeqFuturesProvider(t)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), seqCtxKey{}, "turn-42"))
	defer cancel()
	f.cancel, f.cancelOn = cancel, "entry"

	res := NewFuturesOpenPositionTool(seqCfg()).Execute(ctx, seqOpenArgs())

	if ctx.Err() == nil {
		t.Fatal("test setup: the turn context was never cancelled")
	}
	if res.IsError {
		t.Fatalf("protection must still be placed after the cancel, got error: %s", res.ForLLM)
	}
	if got, want := strings.Join(f.ops(), ","), "leverage,entry,reduce_only,reduce_only"; got != want {
		t.Fatalf("calls = %s, want %s (entry, then stop-loss and take-profit)", got, want)
	}
	for _, c := range f.calls {
		if c.ctxErr != nil {
			t.Errorf("%s ran on a cancelled context: %v", c.op, c.ctxErr)
		}
		if c.ctxMarker != "turn-42" {
			t.Errorf("%s lost the turn's context values: %v", c.op, c.ctxMarker)
		}
		if !c.hasDL {
			t.Errorf("%s ran without the bounded sequence deadline", c.op)
		}
	}
	if !strings.Contains(res.ForUser, "stop_loss") || !strings.Contains(res.ForUser, "take_profit") {
		t.Errorf("result does not report both protection orders: %s", res.ForUser)
	}
}

func TestFuturesOpenPosition_AlreadyCancelledPlacesNothing(t *testing.T) {
	f := newSeqFuturesProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewFuturesOpenPositionTool(seqCfg()).Execute(ctx, seqOpenArgs())

	if !res.IsError {
		t.Fatalf("a cancelled turn must be refused, got: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "cancelled") || !strings.Contains(res.ForLLM, "Nothing was changed") {
		t.Errorf("refusal is not clear: %s", res.ForLLM)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("nothing may be sent to the exchange, got %v", ops)
	}
}

func TestFuturesModifyProtection_CancelAfterRemovingOldStillPlacesNew(t *testing.T) {
	f := newSeqFuturesProvider(t)
	oldID := "old-sl"
	reduce := true
	f.open = []ccxt.Order{{Id: &oldID, ReduceOnly: &reduce}}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), seqCtxKey{}, "turn-7"))
	defer cancel()
	f.cancel, f.cancelOn = cancel, "cancel"

	res := NewFuturesModifyProtectionTool(seqCfg()).Execute(ctx, map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "stop_loss": 45000.0, "take_profit": 60000.0,
		"position_side": "long", "amount": 0.01, "confirm": true,
	})

	if res.IsError {
		t.Fatalf("replacement must still be placed after the cancel, got error: %s", res.ForLLM)
	}
	if got, want := strings.Join(f.ops(), ","), "cancel,reduce_only,reduce_only"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	for _, c := range f.calls {
		if c.ctxErr != nil || c.ctxMarker != "turn-7" {
			t.Errorf("%s: ctxErr=%v marker=%v", c.op, c.ctxErr, c.ctxMarker)
		}
	}
}

func TestFuturesModifyProtection_AlreadyCancelledTouchesNothing(t *testing.T) {
	f := newSeqFuturesProvider(t)
	oldID := "old-sl"
	reduce := true
	f.open = []ccxt.Order{{Id: &oldID, ReduceOnly: &reduce}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewFuturesModifyProtectionTool(seqCfg()).Execute(ctx, map[string]any{
		"provider": "binance", "symbol": "BTC/USDT:USDT", "stop_loss": 45000.0,
		"position_side": "long", "amount": 0.01, "confirm": true,
	})

	if !res.IsError || !strings.Contains(res.ForLLM, "Nothing was changed") {
		t.Fatalf("expected a clear refusal, got: %+v", res)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("the existing protection must be left in place, got calls %v", ops)
	}
}

func TestFuturesEmergencyFlatten_CancelAfterOrderCancelStillClosesPositions(t *testing.T) {
	f := newSeqFuturesProvider(t)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), seqCtxKey{}, "turn-9"))
	defer cancel()
	f.cancel, f.cancelOn = cancel, "cancel_all"

	res := NewFuturesEmergencyFlattenTool(seqCfg()).Execute(ctx, map[string]any{"provider": "binance", "confirm": true})

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got, want := strings.Join(f.ops(), ","), "cancel_all,reduce_only"; got != want {
		t.Fatalf("calls = %s, want %s", got, want)
	}
	for _, c := range f.calls {
		if c.ctxErr != nil || c.ctxMarker != "turn-9" {
			t.Errorf("%s: ctxErr=%v marker=%v", c.op, c.ctxErr, c.ctxMarker)
		}
	}
}

func TestFuturesEmergencyFlatten_AlreadyCancelledTouchesNothing(t *testing.T) {
	f := newSeqFuturesProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := NewFuturesEmergencyFlattenTool(seqCfg()).Execute(ctx, map[string]any{"provider": "binance", "confirm": true})

	if !res.IsError || !strings.Contains(res.ForLLM, "Nothing was changed") {
		t.Fatalf("expected a clear refusal, got: %+v", res)
	}
	if ops := f.ops(); len(ops) != 0 {
		t.Fatalf("nothing may be sent to the exchange, got %v", ops)
	}
}
