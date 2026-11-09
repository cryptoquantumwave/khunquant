package sandbox

import (
	"math"
	"slices"
	"strings"
	"time"
)

// Futures positions over time: they pay or receive funding at the venue's settlement
// instants and are liquidated when the mark reaches their liquidation price. Both are
// driven by Advance and need a DerivSource (the book) and, to be seen without polling, the
// sim clock's wake (reschedule); a simulator without them (an unlocked gateway's) does
// nothing here and never sets LastDerivs.

// KindFunding is the lab journal kind (labjournal) of one funding payment. The gateway
// declares it, and its source scan reads this file for it.
const KindFunding = "exchange.funding"

// FundingEntry is the payload of a KindFunding entry: a payment of one position, made at
// the settlement instant Time, on the mark the book had then. Payment is what the wallet
// received (negative: paid), after the floor at an empty wallet.
type FundingEntry struct {
	Time    time.Time `json:"time"`
	Venue   string    `json:"venue"`
	Symbol  string    `json:"symbol"` // unified: BTC/USDT:USDT
	Wire    string    `json:"wire"`
	Side    string    `json:"side"`
	Rate    float64   `json:"rate"`
	Mark    float64   `json:"mark"`
	Payment float64   `json:"payment"`
}

// IncomeEntry is one line of the venue's income ledger (Binance income, OKX bills): a
// funding payment. Income is USDT, negative when paid.
type IncomeEntry struct {
	Symbol string    `json:"symbol"` // wire
	Type   string    `json:"type"`   // FUNDING_FEE
	Income float64   `json:"income"`
	Rate   float64   `json:"rate"`
	Mark   float64   `json:"mark"`
	Time   time.Time `json:"time"`
}

// maxDerivEvents bounds the payments and liquidations one Advance processes; past it the
// next Advance carries on from where this one stopped.
const maxDerivEvents = 10000

// advanceDerivs brings one futures wallet's funding and liquidations up to now, in time
// order. Each round it finds the next event in (t, now]: the earliest liquidation, found by
// a crossing search over the price path so a crash that recovers between two polls is
// not missed, or else the next settlement instant. A liquidation at the instant of a
// settlement goes first (the position is gone and pays nothing, the others still settle at
// that instant); positions settling at one instant are paid, and two crossing at one instant liquidated, in symbol order. Every
// event is valued at its own time with the wallet the events before it left, so a
// payment that lowers the wallet moves the liquidation price the next search uses. It is
// idempotent: it works from LastDerivs, which it leaves at now.
//
// A wallet is never driven below zero by a payment, a fee or a liquidation: the shortfall
// is forgiven (there is no insurance fund). Called with sim.mu held.
func (sim *StatefulSimulator) advanceDerivs(key string, now time.Time) {
	src := sim.derivs()
	if src == nil || strings.Contains(key, ".") { // the spot wallet has no derivatives
		return
	}
	st := sim.stateManager.GetState(key)
	if st.LastDerivs.IsZero() {
		st.LastDerivs = now
		return
	}
	// t is how far the events are processed, paid how far the settlements are. They differ
	// only after a liquidation at a settlement instant: the positions that settle at that
	// instant are still owed their payment, and the next round pays them.
	t, paid, done := st.LastDerivs, st.LastDerivs, false
	if !now.After(t) {
		return
	}
	for n := 0; n < maxDerivEvents && !done; n++ {
		rs := sim.legRisks(key, st, t, func(p *Position) float64 { px, _ := src.MarkPriceAt(key, p.Symbol, t); return px })
		var settle time.Time
		for _, r := range rs {
			if f, ok := src.NextFunding(key, r.Pos.Symbol, paid); ok && (settle.IsZero() || f.Before(settle)) {
				settle = f
			}
		}
		due := !settle.IsZero() && !settle.After(now)
		until := now
		if due {
			until = settle
		}
		var hit *legRisk
		var at time.Time
		for i := range rs { // symbol order: the earlier of two at one instant wins
			if r := &rs[i]; r.Liq > 0 {
				if c, ok := src.FirstCross(key, r.Pos.Symbol, t, until, r.Liq, r.Qty > 0); ok && (hit == nil || c.Before(at)) {
					hit, at = r, c
				}
			}
		}
		switch {
		case hit != nil:
			sim.liquidate(key, st, *hit, at)
			t = at
			if !(due && at.Equal(settle)) { // else the others settling at this instant are paid next
				paid = at
			}
		case due:
			sim.settleFunding(key, st, rs, paid, settle, src)
			t, paid = settle, settle
		default:
			t, done = now, true
		}
	}
	st.LastDerivs = t
}

// credit adds delta USDT to the wallet and returns what it did. A debit takes the free
// balance first, then the margin the positions hold (positionLocked), never an open order's
// reservation, which the order's cancel would credit a second time. Past that the venue
// cancels the open orders that reserve USDT, oldest first and never keep (the order being
// filled), until the free balance covers the rest; each cancel is dated at. What is still
// missing is forgiven: the wallet never goes below empty.
func (b book) credit(delta float64, keep *Order, at time.Time) float64 {
	st := b.st
	bal := st.Balances["USDT"]
	if delta >= 0 {
		bal.Free += delta
		st.Balances["USDT"] = bal
		return delta
	}
	fromFree := math.Min(-delta, bal.Free)
	fromLocked := math.Min(-delta-fromFree, st.positionLocked(b.venue))
	bal.Free, bal.Locked = bal.Free-fromFree, bal.Locked-fromLocked
	st.Balances["USDT"] = bal // before any cancel: close credits the balance it reads
	took := fromFree + fromLocked
	if rest := -delta - took; rest > -delta*1e-9 { // more than rounding
		for _, o := range b.usdtOrders(keep) {
			if st.Balances["USDT"].Free >= rest {
				break
			}
			if o.Reserved > 0 {
				b.closeAs(o, stCanceled, at, "margin")
			}
		}
		bal = st.Balances["USDT"]
		last := math.Min(rest, bal.Free)
		bal.Free -= last
		st.Balances["USDT"] = bal
		took += last
	}
	return -took
}

// settleFunding pays the positions whose next settlement after t is at: -q x mark x rate,
// so a long pays when the rate is positive and a short receives it.
func (sim *StatefulSimulator) settleFunding(key string, st *VenueState, rs []legRisk, t, at time.Time, src DerivSource) {
	for _, r := range rs {
		sym := r.Pos.Symbol
		if f, ok := src.NextFunding(key, sym, t); !ok || !f.Equal(at) {
			continue
		}
		rate, okR := src.FundingAt(key, sym, at)
		mark, okM := src.MarkPriceAt(key, sym, at)
		if !okR || !okM || !finite(rate) || !(mark > 0) {
			continue
		}
		pay := sim.book(key).credit(-r.Qty*mark*rate, nil, at)
		st.Income = append(st.Income, IncomeEntry{Symbol: sym, Type: "FUNDING_FEE", Income: pay, Rate: rate, Mark: mark, Time: at})
		if sim.journal != nil {
			o := &Order{Symbol: sym, Market: "futures"}
			sim.journal(KindFunding, key, FundingEntry{Time: at, Venue: key, Symbol: unifiedSymbol(key, o), Wire: sym, Side: r.Pos.Side,
				Rate: rate, Mark: mark, Payment: pay})
		}
	}
}

// liquidate closes the position at its liquidation price, at the instant the price path
// reached it. Cross: the venue first cancels the open orders that reserve USDT (journaled
// with effect "margin"), as venues do before a liquidation, so their reservations are wallet
// again; then the position's margin is released, and the loss q(L-E) and a fee equal to its
// maintenance margin are debited from the wallet (floored at empty). Isolated: the
// position's own margin goes and the wallet and the orders are untouched. The venue's
// liquidation order is kept for forceOrders and journaled as a fill with effect "liquidation".
func (sim *StatefulSimulator) liquidate(key string, st *VenueState, r legRisk, at time.Time) {
	p, px := r.Pos, r.Liq
	fee := r.Rate * math.Abs(r.Qty) * px
	if !p.isolated() {
		b := sim.book(key)
		for _, o := range b.usdtOrders(nil) {
			b.closeAs(o, stCanceled, at, "margin")
		}
		bal := st.Balances["USDT"]
		rel := math.Min(st.positionLocked(key), math.Abs(r.Qty)*p.EntryPrice) // what the ledger locked when it opened
		bal.Locked, bal.Free = bal.Locked-rel, bal.Free+rel
		st.Balances["USDT"] = bal
		b.credit(r.Qty*(px-p.EntryPrice)-fee, nil, at)
	}
	delete(st.Positions, p.Symbol)

	id, side := sim.stateManager.NextOrderID(key), "sell" // a long is closed by a sell
	if r.Qty < 0 {
		side = "buy"
	}
	o := &Order{ID: id, Symbol: p.Symbol, OrderType: "market", Side: side, Amount: p.Contracts, Filled: p.Contracts, Average: px,
		Cost: math.Abs(r.Qty) * px, Status: nativeStates[key][stFilled], Fee: fee, CreatedAt: at, UpdatedAt: at,
		Market: "futures", ClientID: "liq" + id, ReduceOnly: true, Params: map[string]interface{}{"liquidation": true, "posSide": p.Side}}
	st.Liquidations = append(st.Liquidations, o)
	if sim.journal != nil {
		sim.journal(KindOrder, key, OrderEntry{At: at, Event: "fill", Venue: key, Market: "futures", Symbol: unifiedSymbol(key, o), Wire: p.Symbol,
			Side: side, Type: "market", Amount: o.Amount, Filled: o.Filled, Avg: px, Status: journalStates[stFilled], OrderID: id,
			ClientID: o.ClientID, ReduceOnly: true, Effect: "liquidation"})
	}
}

// nextSettlement is the earliest settlement after now among the open positions of the
// venue's futures wallet; reschedule asks the clock to stop there.
func (sim *StatefulSimulator) nextSettlement(key string, now time.Time) (time.Time, bool) {
	src := sim.derivs()
	if src == nil || strings.Contains(key, ".") {
		return time.Time{}, false
	}
	var next time.Time
	for _, p := range sim.stateManager.GetState(key).Positions {
		if p != nil && p.Contracts > 0 {
			if f, ok := src.NextFunding(key, p.Symbol, now); ok && (next.IsZero() || f.Before(next)) {
				next = f
			}
		}
	}
	return next, !next.IsZero()
}

// liquidationsOf lists a wallet's liquidation orders, oldest first.
func liquidationsOf(st *VenueState) []*Order {
	out := slices.Clone(st.Liquidations)
	sortOrders(out)
	return out
}
