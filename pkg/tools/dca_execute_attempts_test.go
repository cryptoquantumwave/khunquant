package tools

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// MR-006: every DCA attempt leaves exactly one dca_executions row, the USD caps
// are measured in USD, and the order path persists atomically.

func dcaF64(v float64) *float64 { return &v }

// fixDCAClock pins the clock behind DCA row timestamps and the period cap.
func fixDCAClock(t *testing.T, at time.Time) time.Time {
	t.Helper()
	prev := dcaNow
	dcaNow = func() time.Time { return at }
	t.Cleanup(func() { dcaNow = prev })
	return at
}

// noDCABackoff removes the pause between persistence attempts.
func noDCABackoff(t *testing.T) {
	t.Helper()
	prev := dcaPersistBackoff
	dcaPersistBackoff = 0
	t.Cleanup(func() { dcaPersistBackoff = prev })
}

// installDCALimiter swaps the package limiter (TestMain installs an always-allow
// one) for l until the test ends.
func installDCALimiter(t *testing.T, l broker.RateLimiter) {
	t.Helper()
	prev := broker.DefaultLimiter
	broker.DefaultLimiter = l
	t.Cleanup(func() { broker.DefaultLimiter = prev })
}

// setDCAPlan rewrites a stored plan.
func setDCAPlan(t *testing.T, store *dca.Store, planID int64, mutate func(*dca.Plan)) {
	t.Helper()
	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	mutate(plan)
	if err := store.UpdatePlan(context.Background(), plan); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
}

func getDCAPlan(t *testing.T, store *dca.Store, planID int64) *dca.Plan {
	t.Helper()
	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	return plan
}

// seedDCARow inserts a row as production writes it (UTC).
func seedDCARow(t *testing.T, store *dca.Store, planID int64, status, reason string, at time.Time) {
	t.Helper()
	at = at.UTC()
	if _, err := store.SaveExecution(context.Background(), &dca.Execution{
		PlanID: planID, ExecutedAt: at, Symbol: "BTC/USDT", Provider: "x", AmountQuote: 100,
		Status: status, ErrorMsg: reason, Side: "buy", CreatedAt: at,
	}); err != nil {
		t.Fatalf("SaveExecution: %v", err)
	}
}

func dcaRows(t *testing.T, store *dca.Store, planID int64) []dca.Execution {
	t.Helper()
	rows, err := store.GetExecutions(context.Background(), dca.QueryFilter{PlanID: &planID, Limit: 200})
	if err != nil {
		t.Fatalf("GetExecutions: %v", err)
	}
	return rows
}

// countRows counts rows with the status whose error_msg contains reason.
func countRows(rows []dca.Execution, status, reason string) int {
	n := 0
	for _, r := range rows {
		if r.Status == status && strings.Contains(r.ErrorMsg, reason) {
			n++
		}
	}
	return n
}

func runDCA(cfg *config.Config, store *dca.Store, planID int64) *ToolResult {
	return NewExecuteDCAOrderTool(cfg, store).Execute(testCtx(), map[string]any{"plan_id": float64(planID)})
}

// newTHBFixture is a Bitkub-style market: BTC/THB at 100,000 THB and
// USDT/THB at 36, so a 5,000 THB order is about 138.89 USD.
func newTHBFixture(t *testing.T, providerName string, amountQuote float64) (*dca.Store, *dcaGateMock, int64) {
	t.Helper()
	store, mock, planID := newDCAGateFixture(t, providerName)
	mock.last = 100000
	mock.strict = true
	mock.tickers = map[string]float64{"BTC/THB": 100000, "USDT/THB": 36}
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.Symbol = "BTC/THB"
		p.AmountPerOrder = amountQuote
	})
	return store, mock, planID
}

func assertPlanTotals(t *testing.T, store *dca.Store, planID int64, invested, qty, avg float64) {
	t.Helper()
	p := getDCAPlan(t, store, planID)
	if math.Abs(p.TotalInvested-invested) > 1e-9 || math.Abs(p.TotalQuantity-qty) > 1e-12 || math.Abs(p.AvgCost-avg) > 1e-9 {
		t.Errorf("plan totals = invested %v qty %v avg %v; want %v %v %v",
			p.TotalInvested, p.TotalQuantity, p.AvgCost, invested, qty, avg)
	}
}

func TestDCAExecute_OverCap_SkippedRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.TradingRiskConfig)
		want string
	}{
		{"dca-cap", func(r *config.TradingRiskConfig) { r.MaxDCAAmountPerOrderUSD = 100 }, "max_dca_amount_per_order_usd"},
		{"order-value-cap", func(r *config.TradingRiskConfig) { r.MaxOrderValueUSD = 100 }, "max_order_value_usd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			// 5,000 THB at 36 THB/USD is ~138.89 USD: over a 100 USD cap even
			// though "5000" compared as USD would be 50x over and "100" 1x.
			store, mock, planID := newTHBFixture(t, "dca-overcap-"+tc.name, 5000)
			cfg := config.DefaultConfig()
			tc.set(&cfg.TradingRisk)

			res := runDCA(cfg, store, planID)
			if !res.IsError || !strings.Contains(res.ForLLM, tc.want) || !strings.Contains(res.ForLLM, "138.89") {
				t.Fatalf("expected a refusal naming %s with the USD value, got %+v", tc.want, res)
			}
			if mock.ordered {
				t.Fatal("an order was placed over the cap")
			}
			rows := dcaRows(t, store, planID)
			if len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, tc.want) {
				t.Fatalf("expected one skipped row naming the cap, got %+v", rows)
			}
			assertPlanTotals(t, store, planID, 0, 0, 0)

			// 3,000 THB is ~83.33 USD: under the cap, so it goes through.
			setDCAPlan(t, store, planID, func(p *dca.Plan) { p.AmountPerOrder = 3000 })
			res = runDCA(cfg, store, planID)
			if res.IsError || mock.orders != 1 {
				t.Fatalf("3,000 THB order should proceed: %+v (orders=%d)", res, mock.orders)
			}
			rows = dcaRows(t, store, planID)
			if len(rows) != 2 || countRows(rows, "completed", "") != 1 || countRows(rows, "skipped", "") != 1 {
				t.Fatalf("expected one skipped and one completed row, got %+v", rows)
			}
		})
	}
}

func TestDCAExecute_CapSetNoRate_FailsClosed_SkippedRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.TradingRiskConfig)
	}{
		{"dca-cap", func(r *config.TradingRiskConfig) { r.MaxDCAAmountPerOrderUSD = 100 }},
		{"order-value-cap", func(r *config.TradingRiskConfig) { r.MaxOrderValueUSD = 100 }},
		{"both-caps", func(r *config.TradingRiskConfig) { r.MaxDCAAmountPerOrderUSD = 100; r.MaxOrderValueUSD = 100 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			store, mock, planID := newTHBFixture(t, "dca-norate-"+tc.name, 100)
			delete(mock.tickers, "USDT/THB") // THB cannot be priced in USD
			cfg := config.DefaultConfig()
			tc.set(&cfg.TradingRisk)

			res := runDCA(cfg, store, planID)
			if !res.IsError || !strings.Contains(res.ForLLM, "USD conversion rate") {
				t.Fatalf("expected a fail-closed refusal about the USD rate, got %+v", res)
			}
			if mock.ordered {
				t.Fatal("an order was placed although its USD value is unknown")
			}
			rows := dcaRows(t, store, planID)
			if len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, "USD conversion rate") {
				t.Fatalf("expected one skipped row, got %+v", rows)
			}
		})
	}

	t.Run("no-caps-no-rate-fetch", func(t *testing.T) {
		fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
		store, mock, planID := newTHBFixture(t, "dca-norate-nocaps", 100)
		delete(mock.tickers, "USDT/THB")

		if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
			t.Fatalf("with no caps the order should proceed: %+v", res)
		}
		for _, sym := range mock.fetched {
			if sym == "USDT/THB" || sym == "THB/USDT" {
				t.Errorf("a USD rate was fetched (%s) although no cap is set; fetched=%v", sym, mock.fetched)
			}
		}
		if mock.orders != 1 {
			t.Errorf("orders = %d; want 1", mock.orders)
		}
	})
}

func TestDCAExecute_DailyLossBlocksBuyNotSell(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	tr := swapLossTracker(t)
	tr.RecordLoss(500)
	cfg := config.DefaultConfig()
	cfg.TradingRisk.DailyLossLimitUSD = 100

	// A buy is blocked and the refusal is recorded.
	store, mock, planID := newDCAGateFixture(t, "dca-loss-buy")
	res := runDCA(cfg, store, planID)
	if !res.IsError || !strings.Contains(res.ForLLM, "daily loss limit") {
		t.Fatalf("buy should be blocked by the daily loss limit: %+v", res)
	}
	if mock.ordered {
		t.Error("a buy was placed past the daily loss limit")
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, "daily loss limit") {
		t.Fatalf("expected one skipped row for the loss limit, got %+v", rows)
	}

	// A sell reduces exposure and is not blocked.
	store, mock, planID = newDCAGateFixture(t, "dca-loss-sell")
	setDCAPlan(t, store, planID, func(p *dca.Plan) { p.Side = "sell" })
	res = runDCA(cfg, store, planID)
	if res.IsError || mock.orders != 1 || mock.orderSide != "sell" {
		t.Fatalf("sell should proceed past the daily loss limit: %+v (orders=%d)", res, mock.orders)
	}
	rows = dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "completed" || rows[0].Side != "sell" {
		t.Fatalf("expected one completed sell row, got %+v", rows)
	}
}

func TestDCAExecute_PaperModeSimulated(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "dca-paper-sim")
	cfg := config.DefaultConfig()
	cfg.TradingRisk.PaperTradingMode = true

	res := runDCA(cfg, store, planID)
	if res.IsError || !strings.Contains(res.ForUser, "[paper]") {
		t.Fatalf("expected a simulated fill, got %+v", res)
	}
	if mock.ordered {
		t.Fatal("a live order was placed in paper mode")
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly one row, got %+v", rows)
	}
	r := rows[0]
	if r.Status != "skipped" || r.ErrorMsg != dca.PaperSkipReason || r.Side != "buy" || r.FilledQuantity != 0 || r.OrderID != "" {
		t.Errorf("unexpected paper row: %+v", r)
	}
	assertPlanTotals(t, store, planID, 0, 0, 0)
}

func TestDCAExecute_PaperModeRespectsPeriodCap(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "dca-paper-cap")
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.MaxExecPerPeriod = 2
		p.ExecPeriod = "day"
	})
	paper := config.DefaultConfig()
	paper.TradingRisk.PaperTradingMode = true

	for i := 1; i <= 2; i++ {
		if res := runDCA(paper, store, planID); res.IsError || !strings.Contains(res.ForUser, "[paper]") {
			t.Fatalf("paper run %d should simulate: %+v", i, res)
		}
	}
	// The third paper run hits the cap, exactly as a real one would.
	res := runDCA(paper, store, planID)
	if res.IsError || !res.Silent || !strings.Contains(res.ForLLM, "guardrail") {
		t.Fatalf("third paper run should hit the period cap: %+v", res)
	}
	rows := dcaRows(t, store, planID)
	if countRows(rows, "skipped", dca.PaperSkipReason) != 2 || countRows(rows, "skipped", "period cap reached") != 1 || len(rows) != 3 {
		t.Fatalf("unexpected rows after three paper runs: %+v", rows)
	}

	// With paper mode off the paper rows are ignored: a real order goes out.
	res = runDCA(config.DefaultConfig(), store, planID)
	if res.IsError || mock.orders != 1 {
		t.Fatalf("paper rows must not count once paper mode is off: %+v (orders=%d)", res, mock.orders)
	}
	if countRows(dcaRows(t, store, planID), "completed", "") != 1 {
		t.Error("expected the real run to be recorded as completed")
	}
}

func TestDCAExecute_PersistFailure_ReportsDoNotRetry(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	noDCABackoff(t)
	store, mock, planID := newDCAGateFixture(t, "dca-persist-fail")

	attempts := 0
	prev := dcaSaveExecutionTx
	dcaSaveExecutionTx = func(context.Context, *dca.Store, *dca.Execution, *dca.StatsDelta) (int64, error) {
		attempts++
		return 0, errors.New("disk full")
	}
	t.Cleanup(func() { dcaSaveExecutionTx = prev })

	res := runDCA(config.DefaultConfig(), store, planID)
	if mock.orders != 1 {
		t.Fatalf("the order should have been placed once, orders=%d", mock.orders)
	}
	if attempts != 3 {
		t.Errorf("persistence attempts = %d; want 3", attempts)
	}
	if !res.IsError {
		t.Fatalf("expected an error result: %+v", res)
	}
	for _, want := range []string{"order mock-order placed but not recorded; do not retry", "BTC/USDT", "buy", "0.002"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("result lacks %q: %s", want, res.ForLLM)
		}
	}
	if strings.Contains(res.ForLLM, "failed to place") || strings.Contains(res.ForLLM, "execution failed") {
		t.Errorf("a placed order must never read as a failed placement: %s", res.ForLLM)
	}
	assertPlanTotals(t, store, planID, 0, 0, 0)
	if rows := dcaRows(t, store, planID); len(rows) != 0 {
		t.Errorf("no row should exist: %+v", rows)
	}
}

func TestDCAExecute_MarketOrderPassesNilPrice(t *testing.T) {
	for _, side := range []string{"buy", "sell"} {
		t.Run(side, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			store, mock, planID := newDCAGateFixture(t, "dca-nilprice-"+side)
			setDCAPlan(t, store, planID, func(p *dca.Plan) { p.Side = side })

			if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
				t.Fatalf("unexpected error: %+v", res)
			}
			if mock.orders != 1 || mock.orderType != "market" || mock.orderSide != side {
				t.Fatalf("unexpected order: orders=%d type=%q side=%q", mock.orders, mock.orderType, mock.orderSide)
			}
			if mock.orderPrice != nil {
				t.Errorf("a market order was placed with price %v; it must be nil", *mock.orderPrice)
			}
		})
	}
}

// flatCandles is 40 bars with a constant close of 100.
func flatCandles() []ccxt.OHLCV {
	bars := make([]ccxt.OHLCV, 40)
	for i := range bars {
		bars[i] = ccxt.OHLCV{Open: 100, High: 101, Low: 99, Close: 100, Volume: 10}
	}
	return bars
}

func TestDCAExecute_TriggerFalse_SkippedRecorded(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "dca-trigger-false")
	mock.candles = flatCandles()
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.Trigger = &dca.Trigger{Timeframe: "1h", Expression: "close > 1000000"}
	})

	res := runDCA(config.DefaultConfig(), store, planID)
	if res.IsError || !res.Silent || !strings.Contains(res.ForLLM, "expression false") {
		t.Fatalf("expected a silent skip, got %+v", res)
	}
	if mock.ordered {
		t.Fatal("an order was placed with the trigger false")
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, "trigger condition not met: close > 1000000") {
		t.Fatalf("expected one skipped row naming the trigger, got %+v", rows)
	}

	// Control: the same plan with a true expression trades.
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.Trigger = &dca.Trigger{Timeframe: "1h", Expression: "close < 1000000"}
	})
	if res := runDCA(config.DefaultConfig(), store, planID); res.IsError || mock.orders != 1 {
		t.Fatalf("a true trigger should trade: %+v", res)
	}
}

func TestDCAExecute_PeriodCap_SkippedRecorded(t *testing.T) {
	now := fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "dca-period-cap")
	setDCAPlan(t, store, planID, func(p *dca.Plan) {
		p.MaxExecPerPeriod = 1
		p.ExecPeriod = "day"
	})
	seedDCARow(t, store, planID, "completed", "", now.Add(-2*time.Hour))

	res := runDCA(config.DefaultConfig(), store, planID)
	if res.IsError || !res.Silent || !strings.Contains(res.ForLLM, "1/1") {
		t.Fatalf("expected a silent period-cap skip, got %+v", res)
	}
	if mock.ordered {
		t.Fatal("an order was placed past the period cap")
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 2 || countRows(rows, "skipped", "period cap reached: 1/1 executions this day") != 1 {
		t.Fatalf("expected the seeded row plus a skipped row, got %+v", rows)
	}
}

// dcaFix is one fixture for the table-driven attempt tests.
type dcaFix struct {
	store    *dca.Store
	mock     *dcaGateMock
	planID   int64
	cfg      *config.Config
	provider string
	seeded   int
	now      time.Time
}

type dcaScenario struct {
	name   string
	status string // status of the one new row
	reason string // substring of its error_msg
	// gateSkip marks a skip decided before the limiter: it must not spend a token.
	gateSkip bool
	setup    func(t *testing.T, f *dcaFix)
}

func dcaScenarios() []dcaScenario {
	return []dcaScenario{
		{name: "completed", status: "completed"},
		{name: "failed-order", status: "failed", reason: "order placement failed", setup: func(t *testing.T, f *dcaFix) {
			f.mock.orderErr = errors.New("exchange down")
		}},
		{name: "failed-ticker", status: "failed", reason: "failed to fetch ticker", setup: func(t *testing.T, f *dcaFix) {
			f.mock.strict = true
		}},
		{name: "skipped-trigger-false", status: "skipped", reason: "trigger condition not met", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			f.mock.candles = flatCandles()
			setDCAPlan(t, f.store, f.planID, func(p *dca.Plan) {
				p.Trigger = &dca.Trigger{Timeframe: "1h", Expression: "close > 1000000"}
			})
		}},
		{name: "skipped-period-cap", status: "skipped", reason: "period cap reached", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			setDCAPlan(t, f.store, f.planID, func(p *dca.Plan) { p.MaxExecPerPeriod = 1; p.ExecPeriod = "day" })
			seedDCARow(t, f.store, f.planID, "completed", "", f.now.Add(-time.Hour))
			f.seeded = 1
		}},
		{name: "skipped-over-cap", status: "skipped", reason: "max_dca_amount_per_order_usd", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			f.cfg.TradingRisk.MaxDCAAmountPerOrderUSD = 50
		}},
		{name: "skipped-usd-unknown", status: "skipped", reason: "USD conversion rate", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			f.mock.strict = true
			f.mock.tickers = map[string]float64{"BTC/THB": 100000}
			setDCAPlan(t, f.store, f.planID, func(p *dca.Plan) { p.Symbol = "BTC/THB" })
			f.cfg.TradingRisk.MaxOrderValueUSD = 100
		}},
		{name: "skipped-daily-loss", status: "skipped", reason: "daily loss limit", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			swapLossTracker(t).RecordLoss(500)
			f.cfg.TradingRisk.DailyLossLimitUSD = 100
		}},
		{name: "skipped-permission-denied", status: "skipped", reason: "permission check failed", gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			// Only built-in provider IDs resolve accounts, so use binanceth with
			// an account that may read market data but not trade.
			f.cfg.Exchanges.BinanceTH = config.BinanceTHExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{
				{Name: "main", Permissions: []config.PermissionScope{config.ScopeMarketData}},
			}}
			setDCAPlan(t, f.store, f.planID, func(p *dca.Plan) { p.Provider = "binanceth"; p.Account = "main" })
		}},
		{name: "skipped-paper", status: "skipped", reason: dca.PaperSkipReason, gateSkip: true, setup: func(t *testing.T, f *dcaFix) {
			f.cfg.TradingRisk.PaperTradingMode = true
		}},
		{name: "skipped-rate-limited", status: "skipped", reason: "rate limit exceeded", setup: func(t *testing.T, f *dcaFix) {
			lim := broker.NewTokenBucketLimiter(1, time.Hour)
			lim.Allow(f.provider) // the bucket is now empty
			installDCALimiter(t, lim)
		}},
	}
}

func newDCAFix(t *testing.T, name string) *dcaFix {
	t.Helper()
	provider := "dca-attempt-" + name
	store, mock, planID := newDCAGateFixture(t, provider)
	return &dcaFix{
		store: store, mock: mock, planID: planID, cfg: config.DefaultConfig(), provider: provider,
		now: fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)),
	}
}

func TestDCAExecute_ExactlyOneRowPerAttempt(t *testing.T) {
	for _, sc := range dcaScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			f := newDCAFix(t, sc.name)
			if sc.setup != nil {
				sc.setup(t, f)
			}
			runDCA(f.cfg, f.store, f.planID)

			rows := dcaRows(t, f.store, f.planID)
			if len(rows) != f.seeded+1 {
				t.Fatalf("expected %d row(s), got %d: %+v", f.seeded+1, len(rows), rows)
			}
			newest := rows[0]
			for _, r := range rows {
				if r.ID > newest.ID {
					newest = r
				}
			}
			if newest.Status != sc.status || !strings.Contains(newest.ErrorMsg, sc.reason) {
				t.Errorf("row = %s %q; want %s containing %q", newest.Status, newest.ErrorMsg, sc.status, sc.reason)
			}
			if newest.Side != "buy" {
				t.Errorf("side = %q; want buy", newest.Side)
			}
			if sc.status == "completed" && (newest.OrderID == "" || newest.FilledQuantity <= 0) {
				t.Errorf("completed row lacks fill data: %+v", newest)
			}
			if sc.status != "completed" && newest.OrderID != "" {
				t.Errorf("a row without an order carries an order id: %+v", newest)
			}
		})
	}
}

func TestDCAExecute_SkipsDoNotConsumeRateLimit(t *testing.T) {
	for _, sc := range dcaScenarios() {
		if !sc.gateSkip {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			f := newDCAFix(t, "lim-"+sc.name)
			// The real limiter, installed after the always-allow one from TestMain.
			lim := broker.NewTokenBucketLimiter(1, time.Hour)
			installDCALimiter(t, lim)
			if sc.setup != nil {
				sc.setup(t, f)
			}
			runDCA(f.cfg, f.store, f.planID)

			if f.mock.ordered {
				t.Fatal("a skip scenario placed an order")
			}
			if st := lim.Status(); len(st) != 0 {
				t.Errorf("a skipped tick spent a rate-limit token: %+v", st)
			}
		})
	}

	t.Run("order-spends-one-token", func(t *testing.T) {
		f := newDCAFix(t, "lim-order")
		lim := broker.NewTokenBucketLimiter(1, time.Hour)
		installDCALimiter(t, lim)

		if res := runDCA(f.cfg, f.store, f.planID); res.IsError {
			t.Fatalf("first order should go out: %+v", res)
		}
		if st := lim.Status()[f.provider]; st.TokensLeft != 0 {
			t.Errorf("tokens left = %d; want 0", st.TokensLeft)
		}
		res := runDCA(f.cfg, f.store, f.planID)
		if !res.IsError || !strings.Contains(res.ForLLM, "rate limit exceeded") || f.mock.orders != 1 {
			t.Fatalf("second order should be rate limited: %+v (orders=%d)", res, f.mock.orders)
		}
		rows := dcaRows(t, f.store, f.planID)
		if countRows(rows, "completed", "") != 1 || countRows(rows, "skipped", "rate limit exceeded") != 1 {
			t.Errorf("unexpected rows: %+v", rows)
		}
	})
}

func TestDCAExecute_SellBelowAvgCostRecordsRealisedLoss(t *testing.T) {
	sellPlan := func(avg, qty float64) func(*dca.Plan) {
		return func(p *dca.Plan) {
			p.Side = "sell"
			p.AvgCost = avg
			p.TotalQuantity = qty
			p.TotalInvested = avg * qty
		}
	}
	fill := func(price, qty float64) *ccxt.Order {
		id := "sell-1"
		return &ccxt.Order{Id: &id, Average: dcaF64(price), Filled: dcaF64(qty)}
	}

	t.Run("usdt-quote", func(t *testing.T) {
		fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
		tr := swapLossTracker(t)
		store, mock, planID := newDCAGateFixture(t, "dca-sellloss-usdt")
		setDCAPlan(t, store, planID, sellPlan(60000, 0.01))
		mock.orderResp = fill(50000, 0.002)

		if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
			t.Fatalf("unexpected error: %+v", res)
		}
		// (60000-50000) x 0.002 = 20 USD.
		if got := tr.TodayLossUSD(); math.Abs(got-20) > 1e-9 {
			t.Errorf("recorded loss = %v; want 20", got)
		}
		// The sell also reduced the plan's holding, not grew it.
		assertPlanTotals(t, store, planID, 480, 0.008, 60000)
	})

	t.Run("thb-quote-uses-usd-rate-without-caps", func(t *testing.T) {
		fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
		tr := swapLossTracker(t)
		store, mock, planID := newTHBFixture(t, "dca-sellloss-thb", 0.001)
		mock.last = 3000000
		mock.tickers["BTC/THB"] = 3000000
		setDCAPlan(t, store, planID, func(p *dca.Plan) {
			sellPlan(3600000, 0.01)(p)
			p.AmountUnit = "base"
		})
		mock.orderResp = fill(3000000, 0.001)

		if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
			t.Fatalf("unexpected error: %+v", res)
		}
		// 600,000 THB x 0.001 = 600 THB = 16.67 USD at 36.
		if got := tr.TodayLossUSD(); math.Abs(got-600.0/36) > 1e-6 {
			t.Errorf("recorded loss = %v; want %v", got, 600.0/36)
		}
	})

	t.Run("unknown-usd-value-is-silent", func(t *testing.T) {
		fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
		tr := swapLossTracker(t)
		store, mock, planID := newTHBFixture(t, "dca-sellloss-norate", 0.001)
		delete(mock.tickers, "USDT/THB")
		setDCAPlan(t, store, planID, func(p *dca.Plan) {
			sellPlan(3600000, 0.01)(p)
			p.AmountUnit = "base"
		})
		mock.orderResp = fill(3000000, 0.001)

		res := runDCA(config.DefaultConfig(), store, planID)
		if res.IsError || mock.orders != 1 {
			t.Fatalf("the sell must still complete: %+v", res)
		}
		if got := tr.TodayLossUSD(); got != 0 {
			t.Errorf("recorded %v although the USD value is unknown", got)
		}
	})

	for _, tc := range []struct {
		name string
		side string
		avg  float64
		fill float64
	}{
		{"sell-above-cost", "sell", 40000, 50000},
		{"sell-without-avg-cost", "sell", 0, 50000},
		{"buy-below-avg-never-records", "buy", 60000, 50000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			tr := swapLossTracker(t)
			store, mock, planID := newDCAGateFixture(t, "dca-sellloss-"+tc.name)
			setDCAPlan(t, store, planID, func(p *dca.Plan) {
				p.Side = tc.side
				p.AvgCost = tc.avg
				p.TotalQuantity = 0.01
				p.TotalInvested = tc.avg * 0.01
			})
			mock.orderResp = fill(tc.fill, 0.002)

			if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
				t.Fatalf("unexpected error: %+v", res)
			}
			if got := tr.TodayLossUSD(); got != 0 {
				t.Errorf("recorded loss %v; want none", got)
			}
		})
	}
}
