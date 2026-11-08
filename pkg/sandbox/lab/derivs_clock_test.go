package lab

import (
	"encoding/json"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

var (
	btcPair = market.Pair{Base: "BTC", Quote: "USDT"}
	ethPair = market.Pair{Base: "ETH", Quote: "USDT"}
)

// derivLab is a manual-clock lab whose simulator journals into the test, with prices for BTC
// and ETH. Nothing in these tests sends a request to the exchange: whatever happens, the
// clock and the simulator's wake made it happen.
type derivLab struct {
	*Lab
	mu      sync.Mutex
	funding []sandbox.FundingEntry
	orders  []sandbox.OrderEntry
}

// journaled is what the simulator has journaled so far.
func (d *derivLab) journaled() ([]sandbox.FundingEntry, []sandbox.OrderEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.funding), slices.Clone(d.orders)
}

func newDerivLab(t *testing.T) *derivLab {
	t.Helper()
	d := &derivLab{Lab: manualLab(t)}
	d.Sim.SetClock(func() time.Time { return d.Now() })
	d.Sim.SetJournal(func(_, _ string, payload any) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch p := payload.(type) {
		case sandbox.FundingEntry:
			d.funding = append(d.funding, p)
		case sandbox.OrderEntry:
			d.orders = append(d.orders, p)
		}
	})
	for p, px := range map[market.Pair]float64{btcPair: 70000, ethPair: 3000} {
		if err := d.Market.Set("", p, px); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// seed puts the wallet and positions in the binance futures state, as the lab's account seed
// does, and asks for the wake the positions need.
func (d *derivLab) seed(t *testing.T, wallet float64, ps ...*sandbox.Position) {
	t.Helper()
	err := d.Sim.Mutate("binance", func(st *sandbox.VenueState) error {
		st.Balances["USDT"] = sandbox.Balance{Free: wallet}
		for _, p := range ps {
			p.MarkPrice, p.UpdatedAt = p.EntryPrice, d.Now()
			st.Positions[p.Symbol] = p
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (d *derivLab) advance(t *testing.T, by string) map[string]any {
	t.Helper()
	sum, err := call(t, d.Lab, `{"op":"advance","by":"`+by+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func (d *derivLab) wallet() (total float64) {
	d.Sim.Read("binance", func(st *sandbox.VenueState) { total = st.Balances["USDT"].Free + st.Balances["USDT"].Locked })
	return total
}

func (d *derivLab) positions() (n int) {
	d.Sim.Read("binance", func(st *sandbox.VenueState) { n = len(st.Positions) })
	return n
}

func long(sym string, base, entry float64) *sandbox.Position {
	return &sandbox.Position{Symbol: sym, Side: "long", Contracts: base, ContractSize: 1, EntryPrice: entry, Leverage: 5}
}

func short(sym string, base, entry float64) *sandbox.Position {
	return &sandbox.Position{Symbol: sym, Side: "short", Contracts: base, ContractSize: 1, EntryPrice: entry, Leverage: 5}
}

// Funding is paid at the venue's settlement instants while the clock runs with nothing
// polling the exchange: the simulator asks the clock to stop at each one, so each payment is
// made on the mark of its own instant. A day from 09:00 holds 16:00, 00:00 and 08:00, a long
// pays and a short receives a positive rate, and the wallet moves by the sum of the
// payments, which the income ledger and the journal both carry.
func TestFunding_ThreePaymentsOver24h(t *testing.T) {
	d := newDerivLab(t)
	if err := d.Market.Path("", btcPair, 77000, 24*time.Hour, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []market.Pair{btcPair, ethPair} {
		if err := d.Market.SetFunding("", p, map[market.Pair]float64{btcPair: 0.0001, ethPair: 0.0002}[p]); err != nil {
			t.Fatal(err)
		}
	}
	d.seed(t, 10000, long("BTCUSDT", 0.1, 70000), short("ETHUSDT", 2, 3000))
	if at, ok := d.Clock.NextAt(); !ok || d.Clock.Pending() != 1 || !at.Equal(clockT0.Add(7*time.Hour)) {
		t.Fatalf("after the seed: %d timers, next %v: want the one 16:00 UTC settlement a position asks for", d.Clock.Pending(), at)
	}

	sum := d.advance(t, "24h")
	if steps := sum["steps"].(float64); steps != 4 {
		t.Errorf("%v clock steps, want 4: a stop at each of 16:00, 00:00 and 08:00, then the target", steps)
	}
	funding, _ := d.journaled()
	if len(funding) != 6 {
		t.Fatalf("%d funding payments %+v, want 3 settlements x 2 positions", len(funding), funding)
	}
	want := 10000.0
	for i, f := range funding {
		at := clockT0.Add(time.Duration([]int{7, 7, 15, 15, 23, 23}[i]) * time.Hour)
		mark, _ := d.Market.Price("binance", btcPair, at)
		pay, rate, sym, side := -0.1*mark*0.0001, 0.0001, "BTC/USDT:USDT", "long"
		if i%2 == 1 {
			pay, rate, mark, sym, side = 2*3000*0.0002, 0.0002, 3000, "ETH/USDT:USDT", "short"
		}
		if !f.Time.Equal(at) || f.Symbol != sym || f.Side != side || f.Rate != rate || math.Abs(f.Mark-mark) > 1e-9 || math.Abs(f.Payment-pay) > 1e-9 {
			t.Errorf("payment %d = %+v, want %s %s at %v: rate %v, mark %v, payment %v", i, f, sym, side, at.Format("15:04"), rate, mark, pay)
		}
		want += pay
	}
	if got := d.wallet(); math.Abs(got-want) > 1e-6 {
		t.Errorf("wallet = %v, want %v after the payments", got, want)
	}
	var ledger int
	d.Sim.Read("binance", func(st *sandbox.VenueState) { ledger = len(st.Income) })
	if ledger != 6 {
		t.Errorf("income ledger has %d lines, want 6", ledger)
	}
	if d.Clock.Pending() != 1 {
		t.Errorf("%d timers armed after the day, want the next settlement", d.Clock.Pending())
	}
}

// The schedule is the market's: a 4 h interval settles six times in a day, and a rate set
// between two settlements is the one the next payment uses.
func TestFunding_ScheduleAndRateAtTheInstant(t *testing.T) {
	d := newDerivLab(t)
	if err := d.Market.SetFundingInterval("", btcPair, 4*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := d.Market.SetFunding("", btcPair, 0.0001); err != nil {
		t.Fatal(err)
	}
	d.seed(t, 10000, long("BTCUSDT", 0.1, 70000))
	d.advance(t, "2h") // 11:00: the 12:00 settlement is still ahead
	if err := d.Market.SetFunding("", btcPair, -0.0003); err != nil {
		t.Fatal(err)
	}
	d.advance(t, "22h")
	funding, _ := d.journaled()
	if len(funding) != 6 {
		t.Fatalf("%d payments, want 6 on a 4 h interval: %+v", len(funding), funding)
	}
	for i, f := range funding {
		if f.Rate != -0.0003 || f.Payment <= 0 || !f.Time.Equal(clockT0.Add(time.Duration(3+4*i)*time.Hour)) {
			t.Errorf("payment %d = %+v, want rate -0.0003 (a long receives it) at %v", i, f, clockT0.Add(time.Duration(3+4*i)*time.Hour))
		}
	}
}

// A payment can take the whole wallet and no more: the shortfall is forgiven, and the
// wallet is never negative.
func TestFunding_NeverNegativeWallet(t *testing.T) {
	d := newDerivLab(t)
	if err := d.Market.SetFunding("", btcPair, 0.9); err != nil {
		t.Fatal(err)
	}
	d.seed(t, 100, long("BTCUSDT", 0.1, 70000)) // owes 0.1 x 70000 x 0.9 = 6300
	d.advance(t, "8h")
	if got := d.wallet(); got != 0 {
		t.Errorf("wallet = %v, want exactly 0", got)
	}
	if funding, _ := d.journaled(); len(funding) != 1 || funding[0].Payment != -100 {
		t.Errorf("payments = %+v, want one of -100: what the wallet held", funding)
	}
}

// A price path that goes through the liquidation price and comes back inside one advance
// still liquidates the position, at the instant the path first reached it, with no one
// polling. The same path with a wallet that keeps the liquidation price out of reach is the
// control. The loss and the fee are the whole wallet, which ends at zero.
func TestLiquidation_CrashBetweenPolls(t *testing.T) {
	for _, c := range []struct {
		name      string
		pos       *sandbox.Position
		wallet    float64
		peak      float64
		wantAt    time.Duration
		wantSide  string
		wantLiq   float64
		liquidate bool
	}{
		// 0.1 BTC short from 70000 on 1000 at 0.5%: L = 8000 / 0.1005; the spike to 80000 reaches it 96% of the way up.
		{"short, spike up", short("BTCUSDT", 0.1, 70000), 1000, 80000, 0, "buy", 8000 / 0.1005, true},
		// 0.1 BTC long from 70000 on 600: L = (7000 - 600) / (0.1 x 0.995); the dip to 60000 reaches it.
		{"long, dip down", long("BTCUSDT", 0.1, 70000), 600, 60000, 0, "sell", 6400 / 0.0995, true},
		{"control: a wallet that keeps it out of reach", short("BTCUSDT", 0.1, 70000), 10000, 80000, 0, "buy", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDerivLab(t)
			c.pos.MaintRate = 0.005
			d.seed(t, c.wallet, c.pos)
			// To the peak in 10 minutes and back to 70000 in 10 more: after the hour it is where it began.
			if err := d.Market.Path("", btcPair, 70000, 20*time.Minute, market.ShapeLinear, []market.Keyframe{{At: 10 * time.Minute, Price: c.peak}}); err != nil {
				t.Fatal(err)
			}
			d.advance(t, "1h")
			if px, _ := d.Market.Price("binance", btcPair, d.Now()); px != 70000 {
				t.Fatalf("the path ended at %v", px)
			}
			_, orders := d.journaled()
			var liq []sandbox.OrderEntry
			for _, o := range orders {
				if o.Effect == "liquidation" {
					liq = append(liq, o)
				}
			}
			if !c.liquidate {
				if len(liq) != 0 || d.positions() != 1 || d.wallet() != c.wallet {
					t.Errorf("control: liquidations %+v, positions %d, wallet %v: nothing should have happened", liq, d.positions(), d.wallet())
				}
				return
			}
			if len(liq) != 1 || d.positions() != 0 {
				t.Fatalf("liquidations %+v, positions %d, want one liquidation and no position", liq, d.positions())
			}
			o := liq[0]
			frac := math.Abs(c.wantLiq-70000) / math.Abs(c.peak-70000) // how far up the first leg
			wantAt := clockT0.Add(time.Duration(frac * float64(10*time.Minute)))
			if o.Event != "fill" || o.Status != "filled" || o.Side != c.wantSide || !o.ReduceOnly || o.Wire != "BTCUSDT" || o.Symbol != "BTC/USDT:USDT" ||
				math.Abs(o.Avg-c.wantLiq) > 1e-6 || o.At.Sub(wantAt).Abs() > 2*time.Millisecond {
				t.Errorf("liquidation = %+v, want a %s fill at %.4f dated %v", o, c.wantSide, c.wantLiq, wantAt)
			}
			if got := d.wallet(); got < 0 || got > 1e-6 {
				t.Errorf("wallet = %v, want the loss and the fee to have taken all of it, and not more", got)
			}
			var forced int
			d.Sim.Read("binance", func(st *sandbox.VenueState) { forced = len(st.Liquidations) })
			if forced != 1 {
				t.Errorf("%d liquidation orders kept for forceOrders, want 1", forced)
			}
		})
	}
}

// A funding payment that lowers the wallet moves the liquidation price, and the crossing
// search that follows uses the new one: the mark sits between the two, and the position is
// liquidated at the settlement instant, after the payment. With no funding it is the control.
func TestLiquidation_OrderAfterFunding(t *testing.T) {
	for _, rate := range []float64{-0.05, 0} {
		d := newDerivLab(t)
		if err := d.Market.SetFunding("", btcPair, rate); err != nil {
			t.Fatal(err)
		}
		if err := d.Market.Set("", btcPair, 78000); err != nil { // under L = 79601.99, over the lowered one
			t.Fatal(err)
		}
		pos := short("BTCUSDT", 0.1, 70000)
		pos.MaintRate = 0.005
		d.seed(t, 1000, pos)
		d.advance(t, "8h")
		funding, orders := d.journaled()
		var liqAt time.Time
		for _, o := range orders {
			if o.Effect == "liquidation" {
				liqAt = o.At
			}
		}
		n := len(funding)
		switch {
		case rate != 0 && (n != 1 || !liqAt.Equal(clockT0.Add(7*time.Hour)) || d.positions() != 0):
			t.Errorf("rate %v: %d payments, liquidated at %v, positions %d; want one payment and the liquidation at 16:00, after it", rate, n, liqAt, d.positions())
		case rate == 0 && (!liqAt.IsZero() || d.positions() != 1):
			t.Errorf("control: liquidated at %v with positions %d, want none", liqAt, d.positions())
		}
	}
}

// Two isolated positions that reach their liquidation prices at one instant are liquidated
// in symbol order, and two runs of the same path give the same state.
func TestDerivs_DeterministicOrder(t *testing.T) {
	run := func() ([]string, string) {
		d := newDerivLab(t)
		for _, p := range []market.Pair{btcPair, ethPair} {
			if err := d.Market.Set("", p, 100); err != nil {
				t.Fatal(err)
			}
			if err := d.Market.Path("", p, 200, time.Hour, market.ShapeLinear, nil); err != nil {
				t.Fatal(err)
			}
		}
		var ps []*sandbox.Position
		for _, sym := range []string{"ETHUSDT", "BTCUSDT"} { // seeded in the wrong order on purpose
			p := short(sym, 1, 100)
			p.MarginMode, p.MaintRate, p.Isolated = "isolated", 0.01, 51.5 // L = (100 + 51.5) / 1.01 = 150
			ps = append(ps, p)
		}
		d.seed(t, 0, ps...)
		d.advance(t, "2h")
		_, orders := d.journaled()
		var order []string
		for _, o := range orders {
			if o.Effect == "liquidation" {
				order = append(order, o.Wire+"@"+o.At.Format(time.RFC3339Nano))
			}
		}
		var state []byte
		d.Sim.Read("binance", func(st *sandbox.VenueState) { state, _ = json.Marshal(st) })
		return order, string(state)
	}
	first, stateA := run()
	if len(first) != 2 || first[0][:7] != "BTCUSDT" || first[1][:7] != "ETHUSDT" {
		t.Fatalf("liquidations %v, want BTCUSDT then ETHUSDT", first)
	}
	if first[0][8:] != first[1][8:] {
		t.Errorf("the two crossed at %v and %v, want one instant", first[0][8:], first[1][8:])
	}
	if _, stateB := run(); stateA != stateB {
		t.Errorf("two runs differ:\n%s\n%s", stateA, stateB)
	}
}

// A liquidation found exactly at a settlement instant takes that position out, and the other
// positions settling at that instant are still paid, with their income line and journal
// entry (184-1). The price steps through the liquidation price at 16:00, the first
// settlement; the survivor is the symbol that sorts after the liquidated one and before it.
func TestLiquidation_AtASettlementStillPaysTheOthers(t *testing.T) {
	for _, c := range []struct {
		name       string
		pair, surv market.Pair
		wire, sWire string
		liq        *sandbox.Position
		stepTo     float64
		survivor   *sandbox.Position
		pay        float64 // the survivor's payment: -q x mark x rate
	}{
		{"survivor sorts after", btcPair, ethPair, "BTCUSDT", "ETHUSDT", short("BTCUSDT", 0.1, 70000), 90000, long("ETHUSDT", 2, 3000), -2 * 3000 * 0.0002},
		{"survivor sorts before", ethPair, btcPair, "ETHUSDT", "BTCUSDT", short("ETHUSDT", 2, 3000), 4000, long("BTCUSDT", 0.1, 70000), -0.1 * 70000 * 0.0001},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDerivLab(t)
			for p, rate := range map[market.Pair]float64{btcPair: 0.0001, ethPair: 0.0002} {
				if err := d.Market.SetFunding("", p, rate); err != nil {
					t.Fatal(err)
				}
			}
			entry := c.liq.EntryPrice
			c.liq.MarginMode, c.liq.MaintRate, c.liq.Isolated = "isolated", 0.005, entry*c.liq.Contracts*0.1 // L is under the step
			// The price holds until 16:00 (7 h in), then steps past the liquidation price.
			if err := d.Market.Path("", c.pair, entry, 8*time.Hour, market.ShapeStep, []market.Keyframe{{At: 7 * time.Hour, Price: c.stepTo}}); err != nil {
				t.Fatal(err)
			}
			d.seed(t, 10000, c.liq, c.survivor)
			settle := clockT0.Add(7 * time.Hour)
			check := func(when string) {
				t.Helper()
				funding, orders := d.journaled()
				if len(funding) != 1 || funding[0].Wire != c.sWire || !funding[0].Time.Equal(settle) || math.Abs(funding[0].Payment-c.pay) > 1e-9 {
					t.Fatalf("%s: funding %+v, want one payment of %v to %s at 16:00 and none to the liquidated %s", when, funding, c.pay, c.sWire, c.wire)
				}
				var liqAt []time.Time
				for _, o := range orders {
					if o.Effect == "liquidation" {
						liqAt = append(liqAt, o.At)
					}
				}
				if len(liqAt) != 1 || !liqAt[0].Equal(settle) || d.positions() != 1 {
					t.Fatalf("%s: liquidations at %v, positions %d, want one at 16:00 and the survivor kept", when, liqAt, d.positions())
				}
				var income []sandbox.IncomeEntry
				d.Sim.Read("binance", func(st *sandbox.VenueState) { income = slices.Clone(st.Income) })
				if len(income) != 1 || income[0].Symbol != c.sWire || math.Abs(income[0].Income-c.pay) > 1e-9 {
					t.Fatalf("%s: income %+v, want the survivor's one line", when, income)
				}
				if got := d.wallet(); math.Abs(got-(10000+c.pay)) > 1e-6 { // the liquidated one is isolated: the wallet is untouched
					t.Fatalf("%s: wallet %v, want %v", when, got, 10000+c.pay)
				}
			}
			d.advance(t, "8h")
			check("after the advance")
			for _, at := range []time.Time{{}, d.Now()} { // idempotent at one now
				d.Sim.Advance(at)
			}
			check("after a second Advance at the same now")
		})
	}
}

// Advance is idempotent at one time: a second call changes nothing, neither the state nor the journal, so a request, a clock step and a lab read can
// all call it.
func TestDerivs_IdempotentAtSameNow(t *testing.T) {
	d := newDerivLab(t)
	if err := d.Market.Path("", btcPair, 79000, 8*time.Hour, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	pos := short("BTCUSDT", 0.1, 70000)
	pos.MaintRate = 0.005
	d.seed(t, 1000, pos)
	d.advance(t, "9h")
	snapshot := func() (string, int) {
		funding, orders := d.journaled()
		var state []byte
		d.Sim.Read("binance", func(st *sandbox.VenueState) { state, _ = json.Marshal(st) })
		return string(state), len(funding) + len(orders)
	}
	before, n := snapshot()
	if n == 0 {
		t.Fatal("nothing happened in the first advance: the test proves nothing")
	}
	for _, at := range []time.Time{{}, d.Now()} {
		d.Sim.Advance(at)
	}
	if after, m := snapshot(); after != before || m != n {
		t.Errorf("a second advance changed things:\nbefore %s\nafter  %s\njournal %d -> %d", before, after, n, m)
	}
}
