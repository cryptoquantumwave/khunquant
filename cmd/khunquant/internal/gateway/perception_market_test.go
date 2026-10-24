package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// stubMarket is a market-data provider that counts the reads made of it. It embeds
// the interface so that it also "has" every other method: calling one would panic,
// which is what this test wants to see if the source ever tried a different read.
type stubMarket struct {
	broker.MarketDataProvider
	id      string
	tickers atomic.Int64
	batches atomic.Int64
	ohlcv   atomic.Int64
	err     atomic.Pointer[error]
}

func (s *stubMarket) ID() string                     { return s.id }
func (s *stubMarket) Category() broker.AssetCategory { return broker.CategoryCrypto }

func (s *stubMarket) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	s.tickers.Add(1)
	if e := s.err.Load(); e != nil {
		return ccxt.Ticker{}, *e
	}
	last := 42.0
	return ccxt.Ticker{Last: &last}, nil
}

func (s *stubMarket) FetchTickers(_ context.Context, symbols []string) (map[string]ccxt.Ticker, error) {
	s.batches.Add(1)
	out := map[string]ccxt.Ticker{}
	last := 42.0
	for _, sym := range symbols {
		out[sym] = ccxt.Ticker{Last: &last}
	}
	return out, nil
}

func (s *stubMarket) FetchOHLCV(context.Context, string, string, *int64, int) ([]ccxt.OHLCV, error) {
	s.ohlcv.Add(1)
	return nil, nil
}

func (s *stubMarket) reads() int64 { return s.tickers.Load() + s.batches.Load() + s.ohlcv.Load() }

// registerStub registers a fresh stub provider under name.
func registerStub(t *testing.T, name string) *stubMarket {
	t.Helper()
	st := &stubMarket{id: name}
	broker.RegisterFactory(name, func(*config.Config) (broker.Provider, error) { return st, nil })
	t.Cleanup(broker.ResetInstanceCache)
	return st
}

func TestBrokerMarketReaders_PermissionIsCheckedBeforeAnything(t *testing.T) {
	cfg := &config.Config{}
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "trader", Permissions: []config.PermissionScope{config.ScopeTrade}},
		{Name: "reader", Permissions: []config.PermissionScope{config.ScopeMarketData}},
	}
	rs := newBrokerMarketReaders(cfg)
	if _, err := rs.Reader("binance", "trader"); !errors.Is(err, sources.ErrNotPermitted) {
		t.Fatalf("an account without market_data: err = %v, want ErrNotPermitted", err)
	}
	if len(rs.wrapped) != 0 {
		t.Fatal("a provider was built for an account that may not read")
	}
}

func TestBrokerMarketReaders_WrapsOnceAndRewrapsAfterAReset(t *testing.T) {
	var made []*stubMarket
	broker.RegisterFactory("stubmkt1", func(*config.Config) (broker.Provider, error) {
		st := &stubMarket{id: "stubmkt1"}
		made = append(made, st)
		return st, nil
	})
	t.Cleanup(broker.ResetInstanceCache)
	rs := newBrokerMarketReaders(&config.Config{})

	r1, err := rs.Reader("stubmkt1", "")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := rs.Reader("stubmkt1", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // one wrapper, so the 10 s ticker cache is shared
		if _, err := r1.FetchTicker(context.Background(), "BTC/USDT"); err != nil {
			t.Fatal(err)
		}
		if _, err := r2.FetchTicker(context.Background(), "BTC/USDT"); err != nil {
			t.Fatal(err)
		}
	}
	if len(made) != 1 || made[0].tickers.Load() != 1 {
		t.Fatalf("%d providers, %d reads for 6 cached reads, want 1 and 1", len(made), made[0].tickers.Load())
	}

	// what the source holds cannot be turned into a trading provider.
	if _, ok := any(r1).(broker.TradingProvider); ok {
		t.Fatal("the reader is a trading provider")
	}
	if _, ok := any(r1).(interface {
		CreateOrder(ctx context.Context, symbol, orderType, side string, amount float64, price *float64, params map[string]interface{}) (ccxt.Order, error)
	}); ok {
		t.Fatal("the reader can create orders")
	}

	// a config change resets the registry; the next instance gets a new wrapper.
	broker.ResetInstanceCache()
	r3, err := rs.Reader("stubmkt1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r3.FetchTicker(context.Background(), "BTC/USDT"); err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 || made[1].tickers.Load() != 1 || made[0].tickers.Load() != 1 {
		t.Fatalf("after a reset: %d providers; the new one must be read through, not the stale wrapper", len(made))
	}
}

func TestBrokerMarketReaders_NoMarketDataSurface(t *testing.T) {
	broker.RegisterFactory("stubnomd", func(*config.Config) (broker.Provider, error) { return noMarket{}, nil })
	t.Cleanup(broker.ResetInstanceCache)
	_, err := newBrokerMarketReaders(&config.Config{}).Reader("stubnomd", "")
	if !errors.Is(err, sources.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

type noMarket struct{ broker.Provider }

func TestBrokerMarketReaders_RateLimitIsTaggedForTheSource(t *testing.T) {
	st := registerStub(t, "stubmkt2")
	e := broker.ErrRateLimited
	st.err.Store(&e)
	r, err := newBrokerMarketReaders(&config.Config{}).Reader("stubmkt2", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.FetchTicker(context.Background(), "BTC/USDT")
	if !errors.Is(err, sources.ErrRateLimited) || !errors.Is(err, broker.ErrRateLimited) {
		t.Fatalf("err = %v", err)
	}
}

func TestPerception_MarketSourceIdleWithoutDemandAndPollsWhenDemanded(t *testing.T) {
	st := registerStub(t, "stubmkt3")
	limiterBefore := broker.DefaultLimiter.Status()

	// no demand: the source runs (it is in the stats) and makes no read.
	f := newAssemblyFixture(t, liveWithPerception())
	rt := f.services.Perception
	if rt == nil || rt.market == nil {
		t.Fatal("perception or its market source was not built")
	}
	waitUntil(t, "the first poll", func() bool { return rt.market.Stats().Polls >= 1 })
	if st.reads() != 0 || rt.market.Stats().Requests != 0 {
		t.Fatalf("%d reads with no demand", st.reads())
	}
	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v", code, err)
	}
	var raw struct {
		Perception struct {
			Market map[string]any `json:"market"`
		} `json:"perception"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Perception.Market == nil || raw.Perception.Market["requests"] != float64(0) {
		t.Fatalf("market stats = %v", raw.Perception.Market)
	}
	for k, v := range raw.Perception.Market {
		if _, num := v.(float64); !num { // counts only: no symbol, no error text
			t.Fatalf("market stat %q is %T, want a number", k, v)
		}
	}

	// with a demand: one read, one tick through the pipeline.
	demands := perception.NewStaticDemands(perception.Demand{
		Provider: "stubmkt3", Symbol: "BTC/USDT", Kind: perception.DemandTicker,
	})
	prev := perceptionDemandSource
	perceptionDemandSource = func(assemblyDeps) perception.DemandSource { return demands }
	t.Cleanup(func() { perceptionDemandSource = prev })

	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	rt = f.services.Perception
	waitUntil(t, "the tick", func() bool { return rt.engine.Stats().Processed >= 1 })
	if got := st.reads(); got != 1 {
		t.Fatalf("%d reads for one demand, want 1", got)
	}
	if ms := rt.marketStats(); ms == nil || ms.TicksEmitted != 1 || ms.Requests != 1 {
		t.Fatalf("market stats = %+v", ms)
	}

	if after := broker.DefaultLimiter.Status(); !reflect.DeepEqual(limiterBefore, after) {
		t.Fatalf("the trading limiter changed: %v -> %v", limiterBefore, after)
	}
}
