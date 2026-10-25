package control_test

import (
	"context"
	"errors"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func sp(s string) *string { return &s }

// fakeSpot is a spot provider that records what is asked of it. Anything it does not
// implement panics, which is how a test notices a call it should not have made.
type fakeSpot struct {
	broker.TradingProvider
	status    map[string]string
	fetchErr  error
	fetched   []string
	cancelled []string
}

func (f *fakeSpot) FetchOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	f.fetched = append(f.fetched, id)
	if f.fetchErr != nil {
		return ccxt.Order{}, f.fetchErr
	}
	st, ok := f.status[id]
	if !ok {
		return ccxt.Order{}, errors.New("no such order")
	}
	return ccxt.Order{Id: sp(id), Status: sp(st)}, nil
}

func (f *fakeSpot) CancelOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	f.cancelled = append(f.cancelled, id)
	return ccxt.Order{Id: sp(id), Status: sp("canceled")}, nil
}

type fakeFutures struct {
	broker.FuturesProvider
	cancelled []string
}

func (f *fakeFutures) FetchFuturesOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	return ccxt.Order{Id: sp(id), Status: sp("open")}, nil
}

func (f *fakeFutures) CancelFuturesOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	f.cancelled = append(f.cancelled, id)
	return ccxt.Order{}, nil
}

func deskOver(cfg *config.Config, s *fakeSpot, f *fakeFutures) control.BrokerDesk {
	return control.BrokerDesk{
		Cfg:     cfg,
		Spot:    func(*config.Config, string, string) (broker.TradingProvider, error) { return s, nil },
		Futures: func(*config.Config, string, string) (broker.FuturesProvider, error) { return f, nil },
	}
}

func TestBrokerDesk_LookupAndCancel(t *testing.T) {
	ctx := context.Background()
	cfg := config.DefaultConfig()
	spot := &fakeSpot{status: map[string]string{"open-1": "open", "done-1": "closed"}}
	fut := &fakeFutures{}
	d := deskOver(cfg, spot, fut)
	spotRef := func(id string) broker.OrderRef {
		return broker.OrderRef{Provider: "okx", Market: broker.OrderMarketSpot, Symbol: "BTC/USDT", OrderID: id}
	}

	if st, err := d.Lookup(ctx, spotRef("open-1")); err != nil || !st.Found || !st.Open || st.ID != "open-1" {
		t.Errorf("open: %+v, %v", st, err)
	}
	if st, err := d.Lookup(ctx, spotRef("done-1")); err != nil || !st.Found || st.Open {
		t.Errorf("closed: %+v, %v", st, err)
	}
	if _, err := d.Lookup(ctx, spotRef("gone")); err == nil {
		t.Error("an unknown order answered without an error")
	}
	if _, err := d.Lookup(ctx, broker.OrderRef{Market: broker.OrderMarketSpot, Symbol: "BTC/USDT"}); err == nil {
		t.Error("an order with no id and no description was looked up")
	}
	if err := d.Cancel(ctx, spotRef("open-1"), "open-1"); err != nil || len(spot.cancelled) != 1 {
		t.Errorf("cancel: %v, %v", err, spot.cancelled)
	}

	futRef := broker.OrderRef{Provider: "okx", Market: broker.OrderMarketFutures, Symbol: "BTC/USDT:USDT", OrderID: "f-1"}
	if st, err := d.Lookup(ctx, futRef); err != nil || !st.Open {
		t.Errorf("futures lookup: %+v, %v", st, err)
	}
	if err := d.Cancel(ctx, futRef, "f-1"); err != nil || len(fut.cancelled) != 1 {
		t.Errorf("futures cancel: %v, %v", err, fut.cancelled)
	}
}

// A paper-mode order is looked at as its record says and never asked of an exchange:
// the simulation answers "open" for any id. If paper mode is off by now there is
// nothing real to touch.
func TestBrokerDesk_PaperOrdersStaySimulated(t *testing.T) {
	ctx := context.Background()
	paper := config.DefaultConfig()
	paper.TradingRisk.PaperTradingMode = true
	spot := &fakeSpot{}
	ref := broker.OrderRef{Provider: "okx", Market: broker.OrderMarketSpot, Symbol: "BTC/USDT", OrderID: "sim-1", Paper: true}

	d := deskOver(paper, spot, &fakeFutures{})
	st, err := d.Lookup(ctx, ref)
	if err != nil || !st.Simulated || !st.Open || !st.Found {
		t.Errorf("paper: %+v, %v", st, err)
	}
	if len(spot.fetched) != 0 {
		t.Errorf("a simulated order was fetched from a provider: %v", spot.fetched)
	}
	ref.Done = true
	if st, _ := d.Lookup(ctx, ref); st.Open {
		t.Error("a simulated order that is done reads open")
	}

	live := deskOver(config.DefaultConfig(), spot, &fakeFutures{})
	ref.Done = false
	st, err = live.Lookup(ctx, ref)
	if err != nil || !st.Simulated || len(spot.fetched) != 0 {
		t.Errorf("paper order with paper mode off: %+v, %v, fetched %v", st, err, spot.fetched)
	}
	if err := live.Cancel(ctx, ref, "sim-1"); err != nil || len(spot.cancelled) != 0 {
		t.Errorf("a simulated order reached a real provider on cancel: %v %v", err, spot.cancelled)
	}
}
