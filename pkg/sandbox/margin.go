package sandbox

import (
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// DefaultMaintRate is the maintenance margin rate (a fraction of notional) of a position
// with none of its own and a source that names none.
const DefaultMaintRate = 0.004

func (sim *StatefulSimulator) derivs() DerivSource {
	d, _ := sim.prices.(DerivSource)
	return d
}

// base is the position's quantity in base currency (contracts x contract size), negative
// for a short. Never contracts x price: a contract is not one coin.
func (p *Position) base() float64 {
	q := p.Contracts * p.ContractSize
	if p.Side == "short" {
		return -q
	}
	return q
}

func (p *Position) isolated() bool { return strings.EqualFold(p.MarginMode, "isolated") }

// margin is the isolated margin backing the position: what the seed gave, else its entry
// notional over its leverage.
func (p *Position) margin() float64 {
	if p.Isolated > 0 {
		return p.Isolated
	}
	return math.Abs(p.base()) * p.EntryPrice / float64(max(p.Leverage, 1))
}

// legRisk is one position valued at one instant. With q the signed base quantity, E the
// entry, P the mark and m the maintenance rate: UPnL = q(P-E), Maint = m|q|P. Base is the
// wallet that backs the position and Liq the mark at which UPnL + Base = Maint:
//
//	long   L = (|q|E - Base) / (|q|(1-m))
//	short  L = (|q|E + Base) / (|q|(1+m))
//
// Cross: Base is the USDT wallet plus every other cross position's UPnL - Maint, the others
// held at their current mark. Isolated: Base is its own margin. Liq is 0 when there is no
// positive liquidation price.
type legRisk struct {
	Pos                          *Position
	Qty, Mark, Rate, UPnL, Maint float64
	Base, Liq                    float64
}

// Equity is what the position has left to lose: Base plus its UPnL.
func (r legRisk) Equity() float64 { return r.Base + r.UPnL }

// rateFor is a position's maintenance rate at t: its own, else the source's, else the default.
func (sim *StatefulSimulator) rateFor(venue string, p *Position, t time.Time) float64 {
	if p.MaintRate > 0 && p.MaintRate < 1 {
		return p.MaintRate
	}
	if d := sim.derivs(); d != nil {
		if r, ok := d.MaintRateAt(venue, p.Symbol, t); ok && r > 0 && r < 1 {
			return r
		}
	}
	return DefaultMaintRate
}

// legRisks values the venue's open positions (in symbol order) at marks. A position with
// no positive mark is left out.
func (sim *StatefulSimulator) legRisks(venue string, st *VenueState, at time.Time, mark func(*Position) float64) []legRisk {
	var out []legRisk
	wallet := st.Balances["USDT"].Free + st.Balances["USDT"].Locked
	net := 0.0 // the cross positions' UPnL - Maint
	for _, sym := range slices.Sorted(maps.Keys(st.Positions)) {
		p := st.Positions[sym]
		if p == nil || !(p.Contracts > 0) || p.base() == 0 {
			continue
		}
		px := mark(p)
		if !(px > 0) || !finite(px) {
			continue
		}
		r := legRisk{Pos: p, Qty: p.base(), Mark: px, Rate: sim.rateFor(venue, p, at)}
		r.UPnL, r.Maint = r.Qty*(px-p.EntryPrice), r.Rate*math.Abs(r.Qty)*px
		if !p.isolated() {
			net += r.UPnL - r.Maint
		}
		out = append(out, r)
	}
	for i := range out {
		r := &out[i]
		r.Base = r.Pos.margin()
		if !r.Pos.isolated() {
			r.Base = wallet + net - (r.UPnL - r.Maint)
		}
		q, m := math.Abs(r.Qty), r.Rate
		if r.Qty > 0 {
			r.Liq = (q*r.Pos.EntryPrice - r.Base) / (q * (1 - m))
		} else {
			r.Liq = (q*r.Pos.EntryPrice + r.Base) / (q * (1 + m))
		}
		if !finite(r.Liq) || r.Liq <= 0 {
			r.Liq = 0
		}
	}
	return out
}

// markOf is a position's mark as the wire shows it: refreshMarks has just set it.
func markOf(p *Position) float64 { return p.MarkPrice }

// risksNow values the venue's positions at their current marks, as the wire shows them.
func (sim *StatefulSimulator) risksNow(venue string, st *VenueState) map[string]legRisk {
	sim.refreshMarks(venue, st)
	out := map[string]legRisk{}
	for _, r := range sim.legRisks(venue, st, sim.clock(), markOf) {
		out[r.Pos.Symbol] = r
	}
	return out
}

// cushion is the margin the wire shows ccxt for an isolated position (and for a Binance
// one), floored above zero: ccxt derives a negative margin ratio from a negative margin,
// and perception then drops the whole account's sample.
func (r legRisk) cushion() float64 { return math.Max(r.Equity(), 1e-9) }
