package tools

import (
	"math"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// TestDCAExecute_LossGateContractSell: a plain spot sell is not stopped by the
// daily loss limit. A buy or a sell of a contract symbol (which can open a
// short) is refused outright by the contract-symbol guard, before the loss gate
// and before any order is attempted, so no attempt row is written.
func TestDCAExecute_LossGateContractSell(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.TradingRisk.DailyLossLimitUSD = 100

	for _, tc := range []struct {
		name    string
		symbol  string
		side    string
		blocked bool
	}{
		{"spot-sell-allowed", "BTC/USDT", "sell", false},
		{"contract-sell-blocked", "BTC/USDT:USDT", "sell", true},
		{"contract-buy-blocked", "BTC/USDT:USDT", "buy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			swapLossTracker(t).RecordLoss(500)
			store, mock, planID := newDCAGateFixture(t, "dca-lossgate-"+tc.name)
			setDCAPlan(t, store, planID, func(p *dca.Plan) { p.Symbol = tc.symbol; p.Side = tc.side })

			res := runDCA(cfg, store, planID)
			rows := dcaRows(t, store, planID)
			if tc.blocked {
				if !res.IsError || mock.ordered || len(rows) != 0 || !strings.Contains(res.ForLLM, "contract (futures/swap) symbol") {
					t.Errorf("expected a contract-symbol refusal and no order or row: %+v rows=%+v", res, rows)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("expected one row, got %+v", rows)
			}
			if res.IsError || mock.orders != 1 || rows[0].Status != "completed" {
				t.Errorf("expected the order to go through: %+v rows=%+v", res, rows)
			}
		})
	}
}

// TestDCAExecute_EquityUnknownIsARefusal: with the equity guard armed and no
// sample for the account, a risk-increasing DCA order is refused and recorded.
func TestDCAExecute_EquityUnknownIsARefusal(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	tr := swapLossTracker(t)
	cfg := config.DefaultConfig()
	cfg.TradingRisk.DailyLossLimitUSD = 100
	store, mock, planID := newDCAGateFixture(t, "dca-equity-unknown")
	tr.EnableEquityGuard([]string{broker.AccountKey(cfg, "dca-equity-unknown", "")}, time.Minute)
	t.Cleanup(tr.DisableEquityGuard)

	res := runDCA(cfg, store, planID)
	rows := dcaRows(t, store, planID)
	if !res.IsError || mock.ordered || len(rows) != 1 || rows[0].Status != "skipped" || !strings.Contains(rows[0].ErrorMsg, "equity unknown") {
		t.Fatalf("expected a recorded equity-unknown refusal: %+v rows=%+v", res, rows)
	}
}

func TestDCAExecute_RejectsNonFiniteInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		last   float64
		amount float64
		unit   string
		want   string
	}{
		{"price-nan", math.NaN(), 100, "quote", "invalid price"},
		{"price-inf", math.Inf(1), 100, "quote", "invalid price"},
		{"price-zero", 0, 100, "quote", "invalid price"},
		{"amount-inf-quote", 50000, math.Inf(1), "quote", "invalid order amount"},
		{"amount-inf-base", 50000, math.Inf(1), "base", "invalid order amount"},
		{"amount-negative", 50000, -5, "quote", "invalid order amount"},
		{"amount-zero", 50000, 0, "base", "invalid order amount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
			store, mock, planID := newDCAGateFixture(t, "dca-nonfinite-"+tc.name)
			mock.last = tc.last
			setDCAPlan(t, store, planID, func(p *dca.Plan) { p.AmountPerOrder = tc.amount; p.AmountUnit = tc.unit })

			res := runDCA(config.DefaultConfig(), store, planID)
			if !res.IsError || !strings.Contains(res.ForLLM, tc.want) || mock.ordered {
				t.Fatalf("expected %q and no order: %+v", tc.want, res)
			}
			rows := dcaRows(t, store, planID)
			if len(rows) != 1 || rows[0].Status != "failed" || math.IsNaN(rows[0].AmountQuote) || math.IsInf(rows[0].AmountQuote, 0) {
				t.Errorf("expected one failed row with a finite amount, got %+v", rows)
			}
		})
	}
}

// TestDCAExecute_NonFiniteFillValuesAreIgnored: bad numbers in the adapter's
// order response fall back as if missing and never reach the row or the stats.
func TestDCAExecute_NonFiniteFillValuesAreIgnored(t *testing.T) {
	fixDCAClock(t, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))
	store, mock, planID := newDCAGateFixture(t, "dca-nonfinite-fill")
	id := "weird-fill"
	nan, inf := math.NaN(), math.Inf(1)
	mock.orderResp = &ccxt.Order{Id: &id, Average: &nan, Price: &inf, Filled: &nan, Fee: ccxt.Fee{Cost: &inf}}

	if res := runDCA(config.DefaultConfig(), store, planID); res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}
	rows := dcaRows(t, store, planID)
	if len(rows) != 1 || rows[0].Status != "completed" {
		t.Fatalf("rows = %+v", rows)
	}
	r := rows[0]
	// Fallback: ticker price 50000, requested quantity 100/50000.
	if math.Abs(r.FilledPrice-50000) > 1e-9 || math.Abs(r.FilledQuantity-0.002) > 1e-12 || math.Abs(r.AmountQuote-100) > 1e-9 || r.FeeQuote != 0 {
		t.Errorf("row not on the fallback values: %+v", r)
	}
	assertPlanTotals(t, store, planID, 100, 0.002, 50000)
}
