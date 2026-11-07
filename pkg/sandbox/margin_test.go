package sandbox

import (
	"math"
	"testing"
	"time"
)

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// TestLiqPrice_CrossAndIsolated checks the maths against values worked out by hand, long
// and short, one and two positions, with a contract size of 1 (Binance USDT-M) and of 0.01
// (OKX BTC-USDT-SWAP): a contract count is not a coin count.
func TestLiqPrice_CrossAndIsolated(t *testing.T) {
	type pos struct {
		sym, side                    string
		contracts, cs, entry, mark   float64
		mode                         string
		rate, iso                    float64
		lev                          int64
		wantLiq, wantUPnL, wantMaint float64
	}
	for _, c := range []struct {
		name   string
		wallet float64
		ps     []pos
	}{
		// 10 contracts x 0.01 = 0.1 BTC short from 70000 on 1000: L = (7000+1000)/(0.1 x 1.005).
		{"okx short cross", 1000, []pos{{sym: "BTC-USDT-SWAP", side: "short", contracts: 10, cs: 0.01, entry: 70000, mark: 70000, rate: 0.005,
			wantLiq: 8000 / 0.1005, wantUPnL: 0, wantMaint: 0.005 * 0.1 * 70000}}},
		// 0.5 BTC long from 60000 on 10000: L = (30000-10000)/(0.5 x 0.996); the mark is 55000.
		{"binance long cross", 10000, []pos{{sym: "BTCUSDT", side: "long", contracts: 0.5, cs: 1, entry: 60000, mark: 55000, rate: 0.004,
			wantLiq: 20000 / 0.498, wantUPnL: -2500, wantMaint: 0.004 * 0.5 * 55000}}},
		// The same 5 contracts of OKX are 0.05 BTC, not 5: counting contracts as coins would move L by 100x.
		{"okx long, contract size is not one", 100, []pos{{sym: "BTC-USDT-SWAP", side: "long", contracts: 5, cs: 0.01, entry: 60000, mark: 60000, rate: 0.004,
			wantLiq: (0.05*60000 - 100) / (0.05 * 0.996), wantUPnL: 0, wantMaint: 0.004 * 0.05 * 60000}}},
		// Two cross positions hold the other at its mark. A: long 1 from 100, mark 90 (uPnL -10, maint 0.9).
		// B: short 2 from 50, mark 60 (uPnL -20, maint 1.2). W' of A = 100 - 32.1 + 10.9 = 78.8; of B = 89.1.
		{"two cross", 100, []pos{
			{sym: "A", side: "long", contracts: 1, cs: 1, entry: 100, mark: 90, rate: 0.01, wantLiq: 21.2 / 0.99, wantUPnL: -10, wantMaint: 0.9},
			{sym: "B", side: "short", contracts: 2, cs: 1, entry: 50, mark: 60, rate: 0.01, wantLiq: 189.1 / 2.02, wantUPnL: -20, wantMaint: 1.2},
		}},
		// Isolated: its own margin only, the wallet is out of it. 10 given, else notional / leverage = 20.
		{"isolated long, given margin", 5000, []pos{{sym: "A", side: "long", contracts: 1, cs: 1, entry: 100, mark: 100, mode: "isolated", iso: 10, rate: 0.01,
			wantLiq: 90 / 0.99, wantMaint: 1}}},
		{"isolated long, margin from leverage", 5000, []pos{{sym: "A", side: "long", contracts: 1, cs: 1, entry: 100, mark: 100, mode: "isolated", lev: 5, rate: 0.01,
			wantLiq: 80 / 0.99, wantMaint: 1}}},
		{"isolated short", 5000, []pos{{sym: "A", side: "short", contracts: 1, cs: 1, entry: 100, mark: 100, mode: "isolated", iso: 10, rate: 0.01,
			wantLiq: 110 / 1.01, wantMaint: 1}}},
		// A long backed by more than its notional can never be liquidated: no price, "0" on the wire.
		{"long with no liquidation price", 1000, []pos{{sym: "A", side: "long", contracts: 1, cs: 1, entry: 100, mark: 100, rate: 0.01, wantLiq: 0, wantMaint: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sim := NewStatefulSimulator(NewStateManager())
			st := sim.stateManager.GetState("okx")
			st.Balances["USDT"] = Balance{Free: c.wallet}
			marks := map[string]float64{}
			for _, p := range c.ps {
				st.Positions[p.sym] = &Position{Symbol: p.sym, Side: p.side, Contracts: p.contracts, ContractSize: p.cs, EntryPrice: p.entry,
					MarginMode: p.mode, MaintRate: p.rate, Isolated: p.iso, Leverage: p.lev}
				marks[p.sym] = p.mark
			}
			rs := sim.legRisks("okx", st, time.Time{}, func(p *Position) float64 { return marks[p.Symbol] })
			if len(rs) != len(c.ps) {
				t.Fatalf("%d risks, want %d", len(rs), len(c.ps))
			}
			for i, p := range c.ps {
				if r := rs[i]; r.Pos.Symbol != p.sym || !approx(r.Liq, p.wantLiq) || !approx(r.UPnL, p.wantUPnL) || !approx(r.Maint, p.wantMaint) {
					t.Errorf("%s: liq %v uPnL %v maint %v, want %v %v %v", p.sym, r.Liq, r.UPnL, r.Maint, p.wantLiq, p.wantUPnL, p.wantMaint)
				}
			}
		})
	}
}

// A position's own rate wins, then the source's, then the default; a nonsense rate is ignored.
func TestMaintRate_Precedence(t *testing.T) {
	sim := NewStatefulSimulator(NewStateManager())
	p := &Position{Symbol: "BTCUSDT"}
	if got := sim.rateFor("binance", p, time.Time{}); got != DefaultMaintRate {
		t.Errorf("no source = %v, want the default", got)
	}
	sim.SetPriceSource(fakeDerivs{maint: 0.01})
	if got := sim.rateFor("binance", p, time.Time{}); got != 0.01 {
		t.Errorf("source = %v, want 0.01", got)
	}
	p.MaintRate = 0.02
	if got := sim.rateFor("binance", p, time.Time{}); got != 0.02 {
		t.Errorf("own = %v, want 0.02", got)
	}
	p.MaintRate = 1.5
	if got := sim.rateFor("binance", p, time.Time{}); got != 0.01 {
		t.Errorf("an out-of-range own rate = %v, want the source's", got)
	}
}

// fakeDerivs is a DerivSource with one maintenance rate and no price path.
type fakeDerivs struct{ maint float64 }

func (fakeDerivs) MarkPrice(string, string) (float64, bool)              { return 0, false }
func (fakeDerivs) MarkPriceAt(string, string, time.Time) (float64, bool) { return 0, false }
func (fakeDerivs) FundingAt(string, string, time.Time) (float64, bool)   { return 0, false }
func (fakeDerivs) NextFunding(string, string, time.Time) (time.Time, bool) {
	return time.Time{}, false
}
func (f fakeDerivs) MaintRateAt(string, string, time.Time) (float64, bool) { return f.maint, true }
func (fakeDerivs) FirstCross(string, string, time.Time, time.Time, float64, bool) (time.Time, bool) {
	return time.Time{}, false
}
