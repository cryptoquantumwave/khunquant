package sources_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func fptr(v float64) *float64 { return &v }
func sptr(v string) *string   { return &v }

// fakeFutures is a futures provider that answers the two reads the source makes.
// Any other method panics on the nil embedded interface: the source must not call it.
type fakeFutures struct {
	broker.FuturesProvider

	mu        sync.Mutex
	positions []ccxt.Position
	err       error
	markets   map[string]ccxt.MarketInterface
	reads     int
	catalogue int
}

func (f *fakeFutures) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.positions, f.err
}

func (f *fakeFutures) LoadFuturesMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalogue++
	return f.markets, nil
}

func (f *fakeFutures) set(p []ccxt.Position, err error) {
	f.mu.Lock()
	f.positions, f.err = p, err
	f.mu.Unlock()
}

func (f *fakeFutures) counts() (reads, catalogue int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.catalogue
}

type futuresRig struct {
	src   *sources.Futures
	out   *collector
	clock *perceptiontest.Clock
	stop  func()
}

func newFuturesRig(t *testing.T, o sources.FuturesOptions) *futuresRig {
	t.Helper()
	r := &futuresRig{out: &collector{}, clock: perceptiontest.NewClock(epoch)}
	o.Clock = r.clock
	if o.Cfg == nil {
		o.Cfg = &config.Config{}
	}
	r.src = sources.NewFutures(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.src.Start(ctx, r.out); err != nil {
			t.Errorf("Start: %v", err)
		}
	}()
	r.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the source did not stop")
		}
	}
	t.Cleanup(r.stop)
	return r
}

func oneAccount(fp broker.FuturesProvider) sources.FuturesOptions {
	return sources.FuturesOptions{
		Accounts: []broker.AccountRef{{ProviderID: "okx", Account: "main"}},
		Provider: func(context.Context, string, string) (broker.FuturesProvider, error) { return fp, nil },
		Permit:   func(string, string) error { return nil },
	}
}

func position(symbol, side string, contracts float64) ccxt.Position {
	return ccxt.Position{Symbol: sptr(symbol), Side: sptr(side), Contracts: fptr(contracts), MarkPrice: fptr(100)}
}

func market(size float64) ccxt.MarketInterface { return ccxt.MarketInterface{ContractSize: fptr(size)} }

func (r *futuresRig) advance(t *testing.T, d time.Duration) {
	t.Helper()
	if !r.clock.WaitForTimers(1, 5*time.Second) {
		t.Fatal("the source is not waiting")
	}
	r.clock.Advance(d)
}

func TestFuturesSource_BacksOffWhenFlat(t *testing.T) {
	fp := &fakeFutures{}
	r := newFuturesRig(t, oneAccount(fp))

	waitFor(t, "first read", func() bool { n, _ := fp.counts(); return n == 1 })
	waitFor(t, "the flat sample", func() bool { return len(r.out.events()) == 1 })
	flat := r.out.events()[0].Payload.(sources.FuturesPositionsPayload)
	if len(flat.Positions) != 0 || flat.Provider != "okx" || flat.Account != "main" {
		t.Fatalf("first sample = %+v", flat)
	}

	// 14 minutes: still nothing. A flat account is read every 15, not every 2.
	r.advance(t, 14*time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n, _ := fp.counts(); n != 1 {
		t.Fatalf("read %d times after 14 minutes flat, want 1", n)
	}
	r.advance(t, time.Minute)
	waitFor(t, "second read", func() bool { n, _ := fp.counts(); return n == 2 })
	if len(r.out.events()) != 1 {
		t.Fatalf("a repeated empty sample was emitted: %d events", len(r.out.events()))
	}

	// A position appears: it is emitted, and the next read is two minutes later.
	fp.set([]ccxt.Position{position("BTC/USDT:USDT", "long", 5)}, nil)
	r.advance(t, 15*time.Minute)
	waitFor(t, "the position", func() bool { return len(r.out.events()) == 2 })
	r.advance(t, 2*time.Minute)
	waitFor(t, "the read two minutes later", func() bool { n, _ := fp.counts(); return n == 4 })
	waitFor(t, "the active sample", func() bool { return len(r.out.events()) == 3 })

	// It closes: the empty sample is emitted once, and it backs off again.
	fp.set(nil, nil)
	r.advance(t, 2*time.Minute)
	waitFor(t, "the close", func() bool { return len(r.out.events()) == 4 })
	last := r.out.events()[3].Payload.(sources.FuturesPositionsPayload)
	if len(last.Positions) != 0 {
		t.Fatalf("close sample = %+v", last)
	}
	r.advance(t, 2*time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n, _ := fp.counts(); n != 5 {
		t.Fatalf("read %d times, want 5: after the close the next read is 15 minutes away", n)
	}
}

func TestFuturesSource_SkipsAccountsWithoutTheNeededPermissionOrAPI(t *testing.T) {
	traded := &fakeFutures{}
	var mu sync.Mutex
	built := map[string]int{}
	o := sources.FuturesOptions{
		Accounts: []broker.AccountRef{
			{ProviderID: "okx", Account: "readonly"},
			{ProviderID: "bitkub", Account: "spot"},
			{ProviderID: "okx", Account: "main"},
		},
		Permit: func(p, a string) error {
			if a == "readonly" {
				return errors.New("no trade permission")
			}
			return nil
		},
		Provider: func(_ context.Context, p, a string) (broker.FuturesProvider, error) {
			mu.Lock()
			built[p+"/"+a]++
			mu.Unlock()
			if p == "bitkub" {
				return nil, sources.ErrNoFutures
			}
			return traded, nil
		},
	}
	r := newFuturesRig(t, o)
	waitFor(t, "the traded account", func() bool { return len(r.out.events()) == 1 })
	pl := r.out.events()[0].Payload.(sources.FuturesPositionsPayload)
	if pl.Account != "main" {
		t.Fatalf("polled %q", pl.Account)
	}
	mu.Lock()
	defer mu.Unlock()
	if built["okx/readonly"] != 0 {
		t.Error("a provider was built for an account without the permission")
	}
	if st := r.src.Stats(); st.Skipped != 2 || st.Polls != 1 {
		t.Errorf("stats = %+v, want 2 skipped and 1 poll", st)
	}
	// A refused account is checked once, not every cycle.
	r.advance(t, 15*time.Minute)
	waitFor(t, "second cycle", func() bool { return r.src.Stats().Polls == 2 })
	if built["bitkub/spot"] != 1 {
		t.Errorf("the spot-only provider was built %d times", built["bitkub/spot"])
	}
}

func TestFuturesSource_ValuesPositionsWithTheContractSize(t *testing.T) {
	tests := []struct {
		name     string
		symbol   string
		contract float64
		mark     float64
		pos      *float64 // the position's own ContractSize
		market   map[string]ccxt.MarketInterface
		wantSize float64
		wantUSD  float64
		known    bool
	}{
		{"OKX CHZ, 10 per contract, from the market", "CHZ/USDT:USDT", 136, 0.033, nil,
			map[string]ccxt.MarketInterface{"CHZ/USDT:USDT": market(10)}, 10, 44.88, true},
		{"OKX BTC, 0.01 per contract, from the market", "BTC/USDT:USDT", 200, 2000, nil,
			map[string]ccxt.MarketInterface{"BTC/USDT:USDT": market(0.01)}, 0.01, 4000, true},
		{"size on the position wins", "BTC/USDT:USDT", 200, 2000, fptr(0.01), nil, 0.01, 4000, true},
		{"size unknown is not guessed as 1", "NEW/USDT:USDT", 5, 100, nil,
			map[string]ccxt.MarketInterface{}, 0, 0, false},
		{"inverse contract has no USD notional", "BTC/USD:BTC", 5, 100, fptr(100), nil, 100, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := position(tc.symbol, "long", tc.contract)
			p.MarkPrice = fptr(tc.mark)
			p.ContractSize = tc.pos
			fp := &fakeFutures{positions: []ccxt.Position{p}, markets: tc.market}
			r := newFuturesRig(t, oneAccount(fp))
			waitFor(t, "the sample", func() bool { return len(r.out.events()) == 1 })
			got := r.out.events()[0].Payload.(sources.FuturesPositionsPayload).Positions[0]
			if got.ContractSizeKnown != (tc.wantSize > 0) || got.ContractSize != tc.wantSize {
				t.Errorf("size = %v known %v, want %v", got.ContractSize, got.ContractSizeKnown, tc.wantSize)
			}
			if got.NotionalKnown != tc.known || math.Abs(got.NotionalUSD-tc.wantUSD) > 1e-9 {
				t.Errorf("notional = %v known %v, want %v known %v", got.NotionalUSD, got.NotionalKnown, tc.wantUSD, tc.known)
			}
			if got.Contracts != tc.contract {
				t.Errorf("contracts = %v, want the count %v unchanged", got.Contracts, tc.contract)
			}
		})
	}
}

func TestFuturesSource_CarriesWhatALiquidationTriggerNeeds(t *testing.T) {
	p := ccxt.Position{
		Symbol: sptr("BTC/USDT:USDT"), Side: sptr("SHORT"), Contracts: fptr(-3), ContractSize: fptr(0.01),
		MarkPrice: fptr(60000), LiquidationPrice: fptr(66000), MarginRatio: fptr(0.42), UnrealizedPnl: fptr(-12.5),
	}
	partial := ccxt.Position{Symbol: sptr("ETH/USDT:USDT"), Contracts: fptr(2), ContractSize: fptr(0.1), LiquidationPrice: fptr(0)}
	fp := &fakeFutures{positions: []ccxt.Position{p, partial, {Symbol: sptr("XRP/USDT:USDT"), Contracts: fptr(0)}}}
	r := newFuturesRig(t, oneAccount(fp))
	waitFor(t, "the sample", func() bool { return len(r.out.events()) == 1 })
	ev := r.out.events()[0]
	if ev.Kind != events.KindPerceptionFuturesPositions || ev.TrustLevel() != proactive.TrustExchange {
		t.Fatalf("event = %s trust %v", ev.Kind, ev.TrustLevel())
	}
	rows := ev.Payload.(sources.FuturesPositionsPayload).Positions
	if len(rows) != 2 {
		t.Fatalf("rows = %+v (the zero-contract position must be left out)", rows)
	}
	a := rows[0]
	if a.Side != "short" || a.Contracts != 3 || !a.LiqPriceKnown || a.LiqPrice != 66000 || !a.MarginRatioKnown || a.MarginRatio != 0.42 ||
		!a.UnrealizedPnlKnown || a.UnrealizedPnl != -12.5 || !a.NotionalKnown || math.Abs(a.NotionalUSD-1800) > 1e-9 {
		t.Errorf("full position = %+v", a)
	}
	b := rows[1]
	if b.Side != "" || b.MarkPriceKnown || b.LiqPriceKnown || b.MarginRatioKnown || b.UnrealizedPnlKnown || b.NotionalKnown {
		t.Errorf("a position with missing fields reports them as known: %+v", b)
	}
}

func TestFuturesSource_DropsASampleWithABadNumber(t *testing.T) {
	bad := map[string]func(*ccxt.Position){
		"NaN mark":        func(p *ccxt.Position) { p.MarkPrice = fptr(math.NaN()) },
		"negative mark":   func(p *ccxt.Position) { p.MarkPrice = fptr(-1) },
		"Inf liq":         func(p *ccxt.Position) { p.LiquidationPrice = fptr(math.Inf(1)) },
		"negative margin": func(p *ccxt.Position) { p.MarginRatio = fptr(-0.1) },
		"NaN pnl":         func(p *ccxt.Position) { p.UnrealizedPnl = fptr(math.NaN()) },
		"Inf contracts":   func(p *ccxt.Position) { p.Contracts = fptr(math.Inf(1)) },
		"no symbol":       func(p *ccxt.Position) { p.Symbol = nil },
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			good := position("ETH/USDT:USDT", "long", 1)
			broken := position("BTC/USDT:USDT", "long", 1)
			mut(&broken)
			fp := &fakeFutures{positions: []ccxt.Position{good, broken}}
			r := newFuturesRig(t, oneAccount(fp))
			// poll counts the drop before it records the failure in health, so
			// wait for the streak, the last thing the failed poll writes.
			waitFor(t, "the drop", func() bool {
				return r.src.Stats().Dropped == 1 && r.src.Health().ConsecutiveErrors == 1
			})
			if n := len(r.out.events()); n != 0 {
				t.Fatalf("%d events emitted: a sample that omits a position reads as a close", n)
			}
			if r.src.Health().ConsecutiveErrors != 1 {
				t.Errorf("health = %+v", r.src.Health())
			}
		})
	}
}

func TestFuturesSource_ReadErrorsGoToHealthAndBackOff(t *testing.T) {
	fp := &fakeFutures{err: errors.New("503 from exchange"), positions: []ccxt.Position{position("BTC/USDT:USDT", "long", 1)}}
	r := newFuturesRig(t, oneAccount(fp))
	waitFor(t, "first failure", func() bool { return r.src.Health().ConsecutiveErrors == 1 })
	if h := r.src.Health(); h.LastError == "" || h.ExpectedEvery != 2*time.Minute {
		t.Fatalf("health = %+v", h)
	}
	// After one failure the account waits 4 minutes, not 2.
	r.advance(t, 2*time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n, _ := fp.counts(); n != 1 {
		t.Fatalf("read again after 2 minutes of a 4-minute backoff (%d reads)", n)
	}
	r.advance(t, 2*time.Minute)
	waitFor(t, "second failure", func() bool { return r.src.Health().ConsecutiveErrors == 2 })
	if len(r.out.events()) != 0 {
		t.Fatal("events emitted while reads fail")
	}
	// Recovery.
	fp.set([]ccxt.Position{position("BTC/USDT:USDT", "long", 1)}, nil)
	r.advance(t, 8*time.Minute)
	waitFor(t, "the sample", func() bool { return len(r.out.events()) == 1 })
	waitFor(t, "health to recover", func() bool { return r.src.Health().ConsecutiveErrors == 0 })
}

func TestFuturesSource_CataloguePersistsAcrossPolls(t *testing.T) {
	p := position("CHZ/USDT:USDT", "long", 136)
	fp := &fakeFutures{positions: []ccxt.Position{p}, markets: map[string]ccxt.MarketInterface{"CHZ/USDT:USDT": market(10)}}
	r := newFuturesRig(t, oneAccount(fp))
	waitFor(t, "first sample", func() bool { return len(r.out.events()) == 1 })
	for i := 0; i < 3; i++ {
		r.advance(t, 2*time.Minute)
		want := i + 2
		waitFor(t, "next sample", func() bool { return len(r.out.events()) == want })
	}
	if _, c := fp.counts(); c != 1 {
		t.Fatalf("the market catalogue was loaded %d times in four polls, want 1", c)
	}
}

func TestFuturesSource_DefaultPermissionIsTheTradeScope(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exchanges.OKX = config.OKXExchangeConfig{Enabled: true, Accounts: []config.OKXExchangeAccount{
		{ExchangeAccount: config.ExchangeAccount{Name: "ro", Permissions: []config.PermissionScope{config.ScopeMarketData}}},
		{ExchangeAccount: config.ExchangeAccount{Name: "rw", Permissions: []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade}}},
	}}
	var asked []string
	var mu sync.Mutex
	fp := &fakeFutures{}
	o := sources.FuturesOptions{
		Cfg: cfg,
		Provider: func(_ context.Context, _, a string) (broker.FuturesProvider, error) {
			mu.Lock()
			asked = append(asked, a)
			mu.Unlock()
			return fp, nil
		},
	}
	r := newFuturesRig(t, o)
	waitFor(t, "the permitted account", func() bool { return len(r.out.events()) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "rw" {
		t.Fatalf("providers built for %v, want only rw", asked)
	}
}

func TestFuturesSource_NeverPlacesAnOrder(t *testing.T) {
	// fakeFutures embeds a nil interface: any call other than the two reads panics,
	// and the Manager would report it. Run a cycle and check nothing was recovered.
	fp := &fakeFutures{positions: []ccxt.Position{position("BTC/USDT:USDT", "long", 1)}}
	src := sources.NewFutures(func() sources.FuturesOptions {
		o := oneAccount(fp)
		o.Clock = perceptiontest.NewClock(epoch)
		return o
	}())
	if err := src.Reconcile(context.Background(), &collector{}); err != nil {
		t.Fatal(err)
	}
}
