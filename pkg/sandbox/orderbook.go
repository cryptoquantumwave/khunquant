package sandbox

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"
)

// The order ledger. Venue handlers parse a request into an orderSpec and render the
// answer; everything between (client IDs, reservations, fills, cancels, timed events) is
// here and does not know a wire format. It works on the VenueState maps, so the account
// group, reset and snapshots see exactly what it does. Every method runs under sim.mu.

// ordState is an order's life, whatever name a venue gives it.
type ordState int

const (
	stNew ordState = iota
	stPartial
	stFilled
	stCanceled
	stExpired
)

// nativeStates are the venues' own status names, in ordState order.
var nativeStates = map[string][5]string{
	"binance": {"NEW", "PARTIALLY_FILLED", "FILLED", "CANCELED", "EXPIRED"},
	"okx":     {"live", "partially_filled", "filled", "canceled", "canceled"},
}

func (s ordState) open() bool { return s == stNew || s == stPartial }

// stateOf reads a stored status (an order may come from a seed or a test).
func stateOf(venue, native string) ordState {
	for i, n := range nativeStates[venue] {
		if strings.EqualFold(n, native) {
			return ordState(i)
		}
	}
	return stFilled
}

// sampleStep is the grain at which crossing walks a price path; maxSamples bounds the work
// of one walk (the step grows past it).
const (
	sampleStep = time.Minute
	maxSamples = 2000
)

// orderSpec is a create request as the ledger sees it.
type orderSpec struct {
	Market, Symbol, Side, Type, ClientID, PosSide string
	Amount, Price                                 float64
	ReduceOnly                                    bool
}

// book is the ledger of one state key ("binance", "okx"), valid while sim.mu is held.
type book struct {
	sim   *StatefulSimulator
	key   string
	venue string
	st    *VenueState
}

func (sim *StatefulSimulator) book(key string) book {
	venue, _, _ := strings.Cut(key, ".")
	return book{sim: sim, key: key, venue: venue, st: sim.stateManager.GetState(key)}
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// reject is a refusal in the ledger's words; a venue handler renders it in its own
// (rejects, below). Detail replaces the generic text where it helps the reader.
type reject struct{ kind, detail string }

var rejects = map[string]struct {
	bn            int
	bnMsg, ok, om string
}{
	// -2010 with this text is what ccxt (the lab rewrites the host, so only its common
	// table applies) turns into InsufficientFunds: the library row of the fault injector.
	"funds":     {-2010, "Account has insufficient balance for requested action.", "51008", "Order failed. Insufficient balance"},
	"duplicate": {-2010, "Duplicate order sent.", "51016", "Duplicated clOrdId"},
	// Not -1121 / 51001, which the market's delist answers: a test (and a reader) tells a
	// delisted symbol from one the simulator has no market for by the code.
	"symbol": {-4625, "This symbol does not exist", "51000", "Instrument ID does not exist"},
	"reduce": {-2022, "ReduceOnly Order is rejected.", "51169", "Order failed because you don't have any positions in this direction"},
	"side":   {-4061, "Order's position side does not match user's setting.", "51000", "Parameter posSide error"},
	"param":  {-1013, "Invalid quantity.", "51000", "Parameter error"},
}

// position side of an order, and whether it closes: a hedge-mode posSide names the
// position; in one-way mode the order closes when it opposes the position held.
func direction(sp orderSpec, pos *Position) (dir string, closing bool) {
	own := "long"
	if sp.Side == "sell" {
		own = "short"
	}
	if sp.PosSide == "long" || sp.PosSide == "short" {
		return sp.PosSide, sp.PosSide != own || sp.ReduceOnly
	}
	if pos != nil && pos.Contracts > 0 && (pos.Side == "long" || pos.Side == "short") {
		return pos.Side, pos.Side != own || sp.ReduceOnly
	}
	return own, sp.ReduceOnly
}

// place validates a create against the ledger and, if it passes, books it: the
// reservation, and the first fill when it fills at once.
func (b book) place(sp orderSpec) (o *Order, rj *reject) {
	sim, st := b.sim, b.st
	m, known := st.Markets[sp.Symbol]
	if !known {
		return nil, &reject{kind: "symbol"}
	}
	cs, last := m.ContractSize, sim.markPrice(b.venue, sp.Symbol, st)
	if !(last > 0) {
		return nil, &reject{kind: "symbol", detail: "no price for " + sp.Symbol + " yet (sandbox_market sets one)"}
	}
	px := last
	if sp.Type == "limit" {
		px = sp.Price
	}
	if sp.ClientID != "" && st.openByClient(sp.ClientID) != nil {
		return nil, &reject{kind: "duplicate"}
	}
	pos := st.Positions[sp.Symbol]
	dir, closing := direction(sp, pos)
	var need float64
	switch {
	case closing && (pos == nil || pos.Contracts <= 0 || pos.Side != dir):
		return nil, &reject{kind: "reduce"}
	case !closing && pos != nil && pos.Contracts > 0 && pos.Side != dir:
		return nil, &reject{kind: "side", detail: "both sides of one symbol are not modelled"}
	case !closing:
		if need = sp.Amount * cs * px; st.Balances["USDT"].Free < need {
			return nil, &reject{kind: "funds"}
		}
	}

	id := sim.stateManager.NextOrderID(b.key)
	now := sim.clock()
	o = &Order{ID: id, Symbol: sp.Symbol, OrderType: sp.Type, Side: sp.Side, Amount: sp.Amount, CreatedAt: now, UpdatedAt: now, EvalAt: now,
		ClientID: cmp.Or(sp.ClientID, "sbx"+id), Market: "futures", ReduceOnly: sp.ReduceOnly, Reserved: need,
		Params: map[string]interface{}{"posSide": sp.PosSide, "dir": dir, "closing": closing}}
	o.Status = nativeStates[b.venue][stNew]
	if sp.Type == "limit" {
		o.Price = sp.Price
	}
	bal := st.Balances["USDT"]
	bal.Free, bal.Locked = bal.Free-need, bal.Locked+need
	st.Balances["USDT"] = bal
	st.OpenOrders[id] = o

	if sp.Type == "market" || (sp.Side == "buy" && last <= sp.Price) || (sp.Side == "sell" && last >= sp.Price) {
		b.fill(o, sp.Amount, px, now)
	}
	return o, nil
}

// fill books qty of o at px: its share of the reservation, the position or the balances,
// and the status. The order moves to the closed map when nothing is left.
func (b book) fill(o *Order, qty, px float64, at time.Time) {
	st := b.st
	rem := o.Amount - o.Filled
	if qty >= rem*(1-1e-12) {
		qty = rem
	}
	if !(qty > 0) {
		return
	}
	o.Reserved -= o.Reserved * qty / rem
	cs := st.Markets[o.Symbol].ContractSize
	dir, _ := o.Params["dir"].(string)
	closing, _ := o.Params["closing"].(bool)
	switch pos := st.Positions[o.Symbol]; {
	case closing && pos != nil && pos.Side == dir: // margin comes back with the profit or loss
		n := math.Min(qty, pos.Contracts)
		pnl := n * cs * (px - pos.EntryPrice)
		if dir == "short" {
			pnl = -pnl
		}
		margin := n * cs * pos.EntryPrice
		bal := st.Balances["USDT"]
		bal.Locked, bal.Free = math.Max(0, bal.Locked-margin), bal.Free+margin+pnl
		st.Balances["USDT"] = bal
		if pos.Contracts -= n; pos.Contracts <= 0 {
			delete(st.Positions, o.Symbol)
		} else {
			pos.UpdatedAt = at
		}
	case pos != nil && pos.Contracts > 0 && !closing:
		pos.EntryPrice = (pos.EntryPrice*pos.Contracts + px*qty) / (pos.Contracts + qty)
		pos.Contracts, pos.MarkPrice, pos.UpdatedAt = pos.Contracts+qty, px, at
	case !closing:
		st.Positions[o.Symbol] = &Position{Symbol: o.Symbol, Side: dir, Contracts: qty, ContractSize: cs, EntryPrice: px, MarkPrice: px,
			Leverage: st.Leverage[o.Symbol], UpdatedAt: at}
	}
	o.Average = (o.Average*o.Filled + px*qty) / (o.Filled + qty)
	o.Cost += qty * cs * px
	o.Filled += qty
	o.UpdatedAt = at
	if qty == rem {
		o.Filled = o.Amount
		b.finish(o, stFilled)
	} else {
		o.Status = nativeStates[b.venue][stPartial]
	}
}

// close ends an unfilled remainder: the reservation is released, the order moves to the
// closed map as cancelled or expired.
func (b book) close(o *Order, to ordState, at time.Time) {
	bal := b.st.Balances["USDT"]
	bal.Locked, bal.Free = math.Max(0, bal.Locked-o.Reserved), bal.Free+o.Reserved
	b.st.Balances["USDT"] = bal
	o.Reserved, o.UpdatedAt = 0, at
	b.finish(o, to)
}

func (b book) finish(o *Order, to ordState) {
	o.Status = nativeStates[b.venue][to]
	delete(b.st.OpenOrders, o.ID)
	b.st.ClosedOrders[o.ID] = o
}

// openByClient is the open order carrying the client ID (open-only uniqueness).
func (st *VenueState) openByClient(cid string) *Order {
	for _, o := range st.OpenOrders {
		if o.ClientID == cid {
			return o
		}
	}
	return nil
}

// find looks an order up by exchange ID or, failing that, client ID (the open order
// first, else the newest closed one). A symbol that is not the order's finds nothing.
func (b book) find(id, cid, symbol string) *Order {
	var o *Order
	if id != "" {
		if o = b.st.OpenOrders[id]; o == nil {
			o = b.st.ClosedOrders[id]
		}
	} else if cid != "" {
		if o = b.st.openByClient(cid); o == nil {
			for _, c := range b.st.ClosedOrders {
				if c.ClientID == cid && (o == nil || orderBefore(o, c) < 0) {
					o = c
				}
			}
		}
	}
	if o != nil && symbol != "" && o.Symbol != symbol {
		return nil
	}
	return o
}

// orderBefore is the ledger's order: creation time, then ID as a number.
func orderBefore(a, b *Order) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
}

// list is the orders of one map, oldest first, for one symbol (all when empty).
func (b book) list(closed bool, symbol string) []*Order {
	src := b.st.OpenOrders
	if closed {
		src = b.st.ClosedOrders
	}
	var out []*Order
	for _, o := range src {
		if symbol == "" || o.Symbol == symbol {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, orderBefore)
	return out
}

// advance fills the key's resting limit orders whose price was crossed since they were
// last looked at, each at its own time, oldest first.
func (b book) advance(now time.Time) {
	type event struct {
		o  *Order
		at time.Time
	}
	var evs []event
	for _, o := range b.st.OpenOrders {
		if o.Price <= 0 {
			continue
		}
		if t, ok := b.crossing(o, now); ok {
			evs = append(evs, event{o, t})
		} else {
			o.EvalAt = now
		}
	}
	slices.SortFunc(evs, func(x, y event) int { return cmp.Or(x.at.Compare(y.at), orderBefore(x.o, y.o)) })
	for _, e := range evs {
		b.fill(e.o, e.o.Amount-e.o.Filled, e.o.Price, e.at)
	}
}

// crossing finds when the limit order's price was first reached since it was last
// looked at. The price path is read at sampleStep grain through the source's history
// (MarkPriceAt), so a crossing is dated to within a sample however late it is noticed;
// a crossing that reverses inside one sample is missed. Without a history only now is
// looked at.
func (b book) crossing(o *Order, now time.Time) (time.Time, bool) {
	from := o.EvalAt
	if from.Before(o.CreatedAt) {
		from = o.CreatedAt
	}
	if !now.After(from) {
		return time.Time{}, false
	}
	hit := func(p float64) bool { return (o.Side == "buy" && p <= o.Price) || (o.Side == "sell" && p >= o.Price) }
	hist, _ := b.sim.prices.(PriceHistory)
	if hist == nil {
		return now, hit(b.sim.markPrice(b.venue, o.Symbol, b.st))
	}
	step := sampleStep
	if n := now.Sub(from) / step; n > maxSamples {
		step = now.Sub(from) / maxSamples
	}
	for t := from.Add(step); ; t = t.Add(step) {
		t = minTime(t, now)
		if p, ok := hist.MarkPriceAt(b.venue, o.Symbol, t); ok && hit(p) {
			return t, true
		}
		if t.Equal(now) {
			return time.Time{}, false
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
