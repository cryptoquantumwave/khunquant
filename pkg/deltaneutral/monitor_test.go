package deltaneutral

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// ---- fakes: only the read methods the monitor asserts to --------------------

type fakeFutures struct {
	positions []ccxt.Position
	mark      float64
	rate      float64
	history   []ccxt.FundingRateHistory
	size      float64 // what the market catalogue says
	histCalls int
}

func (f *fakeFutures) FetchFuturesPositions(context.Context, []string) ([]ccxt.Position, error) {
	return f.positions, nil
}
func (f *fakeFutures) FetchFuturesMarkPrice(context.Context, string) (float64, error) {
	return f.mark, nil
}
func (f *fakeFutures) LoadFuturesMarkets(context.Context) (map[string]ccxt.MarketInterface, error) {
	s := f.size
	return map[string]ccxt.MarketInterface{"CHZ/USDT:USDT": {ContractSize: &s}}, nil
}
func (f *fakeFutures) FetchFuturesFundingRate(context.Context, string) (ccxt.FundingRate, error) {
	r := f.rate
	return ccxt.FundingRate{FundingRate: &r}, nil
}
func (f *fakeFutures) FetchPublicFundingRateHistory(context.Context, string, *int64, int) ([]ccxt.FundingRateHistory, error) {
	f.histCalls++
	return f.history, nil
}

type fakeSpot struct{ price, qty float64 }

func (s *fakeSpot) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	p := s.price
	return ccxt.Ticker{Last: &p}, nil
}
func (s *fakeSpot) GetBalances(context.Context) ([]broker.Balance, error) {
	return []broker.Balance{{Asset: "CHZ", Free: s.qty}}, nil
}

func resolver(spot *fakeSpot, fut *fakeFutures) ProviderResolver {
	return ProviderResolverFunc(func(providerID, _ string) (any, error) {
		switch providerID {
		case "spot":
			return spot, nil
		case "fut":
			return fut, nil
		}
		return nil, errors.New("no such provider")
	})
}

func fl(v float64) *float64 { return &v }
func st(v string) *string   { return &v }

func monitorPlan() Plan {
	return Plan{
		ID: 7, Name: "p", Status: PlanStatusActive, Enabled: true,
		SpotProvider: "spot", SpotSymbol: "CHZ/USDT", FuturesProvider: "fut", FuturesSymbol: "CHZ/USDT:USDT",
		FuturesSide: "short", SpotNotionalUSDT: 44.55, RiskPolicy: DefaultRiskPolicy(),
	}
}

func shortPos(symbol, side string, contracts, size float64) ccxt.Position {
	return ccxt.Position{
		Symbol: st(symbol), Side: st(side), Contracts: fl(contracts), ContractSize: fl(size),
		MarkPrice: fl(0.033), LiquidationPrice: fl(0.1), MarginRatio: fl(0.8), UnrealizedPnl: fl(0),
	}
}

func hist(rates ...float64) []ccxt.FundingRateHistory {
	var out []ccxt.FundingRateHistory
	base := time.Now().UnixMilli() - int64(len(rates))*8*3600*1000
	for i, r := range rates {
		ts, rate := base+int64(i)*8*3600*1000, r
		out = append(out, ccxt.FundingRateHistory{Timestamp: &ts, FundingRate: &rate})
	}
	return out
}

func hasCode(codes []string, want string) bool { return slices.Contains(codes, want) }

// ---- Monitor.Check ------------------------------------------------------------

func TestMonitorCheck_PopulatesRecentRates_FundingReversalFires(t *testing.T) {
	// A crafted series: positive, then three negative settlements, and a negative rate now.
	fut := &fakeFutures{
		positions: []ccxt.Position{shortPos("CHZ/USDT:USDT", "short", 135, 10)}, mark: 0.033, rate: -0.0002,
		history: hist(0.0001, -0.0001, -0.0002, -0.0003),
	}
	m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
	res, err := m.Check(context.Background(), monitorPlan())
	if err != nil {
		t.Fatal(err)
	}
	if res.Eval.Snapshot.FundingState != "reversing" || !hasCode(res.Eval.BreachCodes, "funding_reversal") {
		t.Fatalf("funding %q, codes %v", res.Eval.Snapshot.FundingState, res.Eval.BreachCodes)
	}

	// The same current rate with no run behind it is a single negative, not a reversal.
	fut.history = hist(0.0001, 0.0002, 0.0001, 0.0003)
	res, _ = m.Check(context.Background(), monitorPlan())
	if res.Eval.Snapshot.FundingState != "negative" || hasCode(res.Eval.BreachCodes, "funding_reversal") {
		t.Fatalf("funding %q, codes %v", res.Eval.Snapshot.FundingState, res.Eval.BreachCodes)
	}
}

func TestRecentFundingRates_LastNOldestFirstWhateverTheOrder(t *testing.T) {
	h := hist(1, 2, 3, 4, 5)
	slices.Reverse(h) // an adapter that returns newest first
	got := RecentFundingRates(h, 3)
	if !slices.Equal(got, []float64{3, 4, 5}) {
		t.Fatalf("got %v", got)
	}
	if RecentFundingRates(h, 0) != nil {
		t.Fatal("n=0 must give nil")
	}
}

func TestMonitorCheck_MatchesPositionBySymbolSide(t *testing.T) {
	fut := &fakeFutures{mark: 0.033, rate: 0.0001, positions: []ccxt.Position{
		shortPos("BTC/USDT:USDT", "short", 500, 0.01),
		shortPos("CHZ/USDT:USDT", "long", 700, 10), // the opposite side of the same contract
		shortPos("CHZ/USDT:USDT", "short", 135, 10),
	}}
	m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
	res, err := m.Check(context.Background(), monitorPlan())
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshot.FuturesContracts != 135 {
		t.Fatalf("contracts = %v, want 135 (the CHZ short)", res.Snapshot.FuturesContracts)
	}

	// No position of the plan's symbol and side: none is taken, whatever else is listed.
	fut.positions = fut.positions[:2]
	res, _ = m.Check(context.Background(), monitorPlan())
	if res.Snapshot.FuturesContracts != 0 {
		t.Fatalf("contracts = %v, want 0", res.Snapshot.FuturesContracts)
	}

	// A position that names neither symbol nor side (the request was filtered) is accepted.
	p := shortPos("", "", 135, 10)
	p.Symbol, p.Side = nil, nil
	fut.positions = []ccxt.Position{p}
	res, _ = m.Check(context.Background(), monitorPlan())
	if res.Snapshot.FuturesContracts != 135 {
		t.Fatalf("contracts = %v, want 135", res.Snapshot.FuturesContracts)
	}
}

func TestMonitorCheck_ContractSizeFromTheMarketWhenThePositionOmitsIt(t *testing.T) {
	p := shortPos("CHZ/USDT:USDT", "short", 135, 10)
	p.ContractSize = nil
	fut := &fakeFutures{positions: []ccxt.Position{p}, mark: 0.033, rate: 0.0001, size: 10}
	m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
	res, err := m.Check(context.Background(), monitorPlan())
	if err != nil {
		t.Fatal(err)
	}
	if !floatAlmostEqual(res.Snapshot.FuturesNotionalUSDT, 44.55, 1e-6) || hasCode(res.Eval.BreachCodes, "leg_mismatch") {
		t.Fatalf("notional %v, codes %v", res.Snapshot.FuturesNotionalUSDT, res.Eval.BreachCodes)
	}
}

func TestMonitorCheck_HistoryTTL_ReadsTheLongHistoryOncePerTTL(t *testing.T) {
	fut := &fakeFutures{positions: []ccxt.Position{shortPos("CHZ/USDT:USDT", "short", 135, 10)}, mark: 0.033, rate: 0.0001, history: hist(0.0001)}
	now := time.Now()
	m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut), HistoryTTL: 30 * time.Minute, Clock: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Minute)
		if _, err := m.Check(context.Background(), monitorPlan()); err != nil {
			t.Fatal(err)
		}
	}
	// The checks at +10..+25 minutes reuse the read made at +5.
	if fut.histCalls != 1 {
		t.Fatalf("history read %d times in 25 minutes, want 1", fut.histCalls)
	}
	now = now.Add(10 * time.Minute)
	_, _ = m.Check(context.Background(), monitorPlan())
	if fut.histCalls != 2 {
		t.Fatalf("history read %d times after the TTL, want 2", fut.histCalls)
	}

	// Without a TTL (the legacy job) every check reads it.
	fut.histCalls = 0
	legacy := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
	_, _ = legacy.Check(context.Background(), monitorPlan())
	_, _ = legacy.Check(context.Background(), monitorPlan())
	if fut.histCalls != 2 {
		t.Fatalf("legacy monitor read the history %d times, want 2", fut.histCalls)
	}
}

func TestMonitorCheck_ProfitAndLossNeedsTheBookedAmount(t *testing.T) {
	pos := shortPos("CHZ/USDT:USDT", "short", 135, 10)
	pos.UnrealizedPnl = fl(-2)
	fut := &fakeFutures{positions: []ccxt.Position{pos}, mark: 0.033, rate: 0.0001}
	plan := monitorPlan()
	plan.RiskPolicy.MaxDrawdownUSDT = 5
	plan.RiskPolicy.ProfitTargetUSDT = 10

	// No source of the booked amount: the profit and loss is unknown, nothing is judged.
	m := &Monitor{Providers: resolver(&fakeSpot{0.033, 1350}, fut)}
	res, _ := m.Check(context.Background(), plan)
	if hasCode(res.Eval.BreachCodes, "max_drawdown_breached") || hasCode(res.Eval.BreachCodes, "profit_target_met") {
		t.Fatalf("codes %v with no booked amount", res.Eval.BreachCodes)
	}

	// Booked -4: net = -2 (futures) + 0 (spot at entry value) - 4 = -6 < -5.
	m.Booked = func(context.Context, int64) (float64, bool) { return -4, true }
	res, _ = m.Check(context.Background(), plan)
	if !hasCode(res.Eval.BreachCodes, "max_drawdown_breached") {
		t.Fatalf("codes %v, want max_drawdown_breached", res.Eval.BreachCodes)
	}

	// Booked +13: net = +11 >= 10.
	m.Booked = func(context.Context, int64) (float64, bool) { return 13, true }
	res, _ = m.Check(context.Background(), plan)
	if !hasCode(res.Eval.BreachCodes, "profit_target_met") || res.Eval.Snapshot.HealthScore < 15 {
		t.Fatalf("codes %v score %d", res.Eval.BreachCodes, res.Eval.Snapshot.HealthScore)
	}
}

func TestMonitorCheck_NoProvidersIsAnError(t *testing.T) {
	if _, err := (&Monitor{}).Check(context.Background(), monitorPlan()); !errors.Is(err, ErrNoProviders) {
		t.Fatalf("err = %v", err)
	}
}

func TestMonitorable(t *testing.T) {
	ctx := context.Background()
	exp := stubExposure{}
	cases := []struct {
		name   string
		mutate func(*Plan)
		want   bool
	}{
		{"active", func(*Plan) {}, true},
		{"disabled", func(p *Plan) { p.Enabled = false }, false},
		{"draft", func(p *Plan) { p.Status = PlanStatusDraft }, false},
		{"ready", func(p *Plan) { p.Status = PlanStatusReady }, false},
		{"closed", func(p *Plan) { p.Status = PlanStatusClosed }, false},
		{"archived", func(p *Plan) { p.Status = PlanStatusArchived }, false},
		{"paused", func(p *Plan) { p.Status = PlanStatusPaused }, true},
		{"recovery required", func(p *Plan) { p.Status = PlanStatusRecoveryRequired }, true},
		{"failed without exposure", func(p *Plan) { p.Status = PlanStatusFailed }, false},
	}
	for _, c := range cases {
		p := monitorPlan()
		c.mutate(&p)
		if got, _ := Monitorable(ctx, &p, exp); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	p := monitorPlan()
	p.Status = PlanStatusFailed
	if ok, _ := Monitorable(ctx, &p, stubExposure{has: true}); !ok {
		t.Error("a failed plan with open exposure must be monitored")
	}
	if ok, _ := Monitorable(ctx, &p, stubExposure{err: errors.New("db")}); !ok {
		t.Error("a failed plan whose exposure cannot be checked must be monitored")
	}
}

type stubExposure struct {
	has bool
	err error
}

func (s stubExposure) HasOpenExposure(context.Context, int64) (bool, error) { return s.has, s.err }
