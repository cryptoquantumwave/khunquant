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

// KindOrder is the lab journal kind (labjournal) of the ledger's transitions, one entry
// each. The gateway declares it, and its source scan reads this file for it.
const KindOrder = "exchange.order"

// OrderEntry is the payload of a KindOrder entry. At is the sim time of the transition,
// which can be earlier than the time the simulator noticed it (see crossing).
type OrderEntry struct {
	At         time.Time `json:"at"`
	Event      string    `json:"event"` // create | fill | cancel | effect
	Venue      string    `json:"venue"`
	Market     string    `json:"market"`
	Symbol     string    `json:"symbol"` // unified: BTC/USDT:USDT, BTC/USDT
	Wire       string    `json:"wire"`   // the venue's own: BTCUSDT, BTC-USDT-SWAP
	Side       string    `json:"side"`
	Type       string    `json:"type"`
	Amount     float64   `json:"amount"`
	Filled     float64   `json:"filled"`
	Avg        float64   `json:"avg"`
	Status     string    `json:"status"` // open | partially_filled | filled | canceled | expired
	OrderID    string    `json:"order_id"`
	ClientID   string    `json:"client_id"`
	ReduceOnly bool      `json:"reduce_only"`
	Effect     string    `json:"effect,omitempty"`
}

// ordState is an order's life, whatever name a venue gives it.
type ordState int

const (
	stNew ordState = iota
	stPartial
	stFilled
	stCanceled
	stExpired
)

var (
	// nativeStates are the venues' own status names, in ordState order.
	nativeStates = map[string][5]string{
		"binance": {"NEW", "PARTIALLY_FILLED", "FILLED", "CANCELED", "EXPIRED"},
		"okx":     {"live", "partially_filled", "filled", "canceled", "canceled"},
	}
	// journalStates are the one vocabulary of the journal, so a scenario names a status once.
	journalStates = [5]string{"open", "partially_filled", "filled", "canceled", "expired"}
)

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

const (
	// restWatch is how often the clock is made to stop while a limit order rests, so a
	// crossing is noticed within it even when nothing polls.
	restWatch = time.Minute
	// sampleStep is the grain at which crossing walks a price path; maxSamples bounds the
	// work of one walk (the step grows past it).
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

// place validates a create against the ledger and, if it passes, books it: reservation,
// effect, journal, and the first fill when it fills at once. reported is what a
// create_reports_fill rule makes the create response claim.
func (b book) place(sp orderSpec) (o *Order, reported *float64, rj *reject) {
	sim, st := b.sim, b.st
	m, known := st.Markets[sp.Symbol]
	if !known {
		return nil, nil, &reject{kind: "symbol"}
	}
	cs, last := m.ContractSize, sim.markPrice(b.venue, sp.Symbol, st)
	if !(last > 0) {
		return nil, nil, &reject{kind: "symbol", detail: "no price for " + sp.Symbol + " yet (sandbox_market sets one)"}
	}
	px := last
	if sp.Type == "limit" {
		px = sp.Price
	}
	if sp.ClientID != "" && st.openByClient(sp.ClientID) != nil {
		return nil, nil, &reject{kind: "duplicate"}
	}
	pos := st.Positions[sp.Symbol]
	dir, closing := direction(sp, pos)
	var need float64
	switch {
	case closing && (pos == nil || pos.Contracts <= 0 || pos.Side != dir):
		return nil, nil, &reject{kind: "reduce"}
	case !closing && pos != nil && pos.Contracts > 0 && pos.Side != dir:
		return nil, nil, &reject{kind: "side", detail: "both sides of one symbol are not modelled"}
	case !closing:
		if need = sp.Amount * cs * px; st.Balances["USDT"].Free < need {
			return nil, nil, &reject{kind: "funds"}
		}
	}

	// The one question to the fault injector, after every refusal above.
	var eff OrderEffect
	var fired bool
	if sim.effects != nil {
		eff, fired = sim.effects.OrderEffect(OrderCreate, OrderInfo{Venue: b.venue, Market: "futures", Symbol: sp.Symbol, Side: sp.Side,
			Type: sp.Type, ClientID: sp.ClientID, ReduceOnly: sp.ReduceOnly, Amount: sp.Amount})
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
	b.emit("create", o, "")

	force := false
	if fired {
		switch eff.Kind {
		case "partial":
			o.Partial = eff.Fraction
		case "accept_no_fill":
			o.Hold = true
		case "cancel_after_accept":
			o.Hold, o.CancelAt = true, now.Add(eff.After)
		case "lookup_lag":
			o.VisibleAt = now.Add(eff.Lag)
		case "create_reports_fill":
			r := eff.Reported
			reported, force = &r, true
		}
		b.emit("effect", o, eff.Kind)
	}
	marketable := sp.Type == "market" || (sp.Side == "buy" && last <= sp.Price) || (sp.Side == "sell" && last >= sp.Price)
	if !o.Hold && (marketable || force) {
		b.firstFill(o, px, now)
	}
	sim.reschedule(now)
	return o, reported, nil
}

// firstFill fills what an order gets when it first trades: all of it, or the fraction a
// partial rule set (the rest then rests, held). A market order has no resting: whatever
// did not fill expires.
func (b book) firstFill(o *Order, px float64, at time.Time) {
	qty := o.Amount - o.Filled
	if o.Partial > 0 && o.Filled == 0 {
		qty, o.Hold = o.Amount*o.Partial, true
	}
	b.fill(o, qty, px, at)
	if o.OrderType == "market" && o.Filled < o.Amount {
		b.close(o, stExpired, at)
	}
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
	b.emit("fill", o, "")
}

// close ends an unfilled remainder: the reservation is released, the order moves to the
// closed map as cancelled or expired.
func (b book) close(o *Order, to ordState, at time.Time) {
	bal := b.st.Balances["USDT"]
	bal.Locked, bal.Free = math.Max(0, bal.Locked-o.Reserved), bal.Free+o.Reserved
	b.st.Balances["USDT"] = bal
	o.Reserved, o.UpdatedAt = 0, at
	b.finish(o, to)
	b.emit("cancel", o, "")
}

func (b book) finish(o *Order, to ordState) {
	o.Status = nativeStates[b.venue][to]
	delete(b.st.OpenOrders, o.ID)
	b.st.ClosedOrders[o.ID] = o
}

// emit writes the transition to the journal when one is attached. The time is the
// transition's own.
func (b book) emit(event string, o *Order, effect string) {
	if b.sim.journal == nil {
		return
	}
	at := o.UpdatedAt
	if event == "create" {
		at = o.CreatedAt
	}
	b.sim.journal(KindOrder, b.venue, OrderEntry{At: at, Event: event, Venue: b.venue, Market: o.Market, Symbol: unifiedSymbol(b.venue, o), Wire: o.Symbol,
		Side: o.Side, Type: o.OrderType, Amount: o.Amount, Filled: o.Filled, Avg: o.Average, Status: journalStates[stateOf(b.venue, o.Status)],
		OrderID: o.ID, ClientID: o.ClientID, ReduceOnly: o.ReduceOnly, Effect: effect})
}

// unifiedSymbol is the order's symbol as ccxt writes it.
func unifiedSymbol(venue string, o *Order) string {
	var base, quote string
	switch venue {
	case "okx":
		parts := strings.Split(strings.TrimSuffix(o.Symbol, "-SWAP"), "-")
		if len(parts) != 2 {
			return o.Symbol
		}
		base, quote = parts[0], parts[1]
	default:
		for _, q := range []string{"FDUSD", "USDT", "USDC", "BUSD", "BTC", "ETH", "BNB", "TRY", "EUR"} {
			if b, ok := strings.CutSuffix(o.Symbol, q); ok && b != "" {
				base, quote = b, q
				break
			}
		}
		if quote == "" {
			return o.Symbol
		}
	}
	if o.Market == "spot" {
		return base + "/" + quote
	}
	return base + "/" + quote + ":" + quote
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

// visible says whether lookups and listings show the order yet (lookup_lag).
func (o *Order) visible(now time.Time) bool { return o.VisibleAt.IsZero() || !now.Before(o.VisibleAt) }

// orderBefore is the ledger's order: creation time, then ID as a number.
func orderBefore(a, b *Order) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(len(a.ID), len(b.ID)), cmp.Compare(a.ID, b.ID))
}

func sortOrders(os []*Order) { slices.SortFunc(os, orderBefore) }

// list is the visible orders of one map, oldest first, for one symbol (all when empty).
func (b book) list(closed bool, symbol string, now time.Time) []*Order {
	src := b.st.OpenOrders
	if closed {
		src = b.st.ClosedOrders
	}
	var out []*Order
	for _, o := range src {
		if o.visible(now) && (symbol == "" || o.Symbol == symbol) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, orderBefore)
	return out
}

// advance brings the key's orders up to now: it cancels what is due and fills the
// resting limit orders whose price was crossed, each at its own time, oldest event first.
func (b book) advance(now time.Time) {
	type event struct {
		o      *Order
		at     time.Time
		cancel bool
	}
	var evs []event
	for _, o := range b.st.OpenOrders {
		e := event{o: o}
		if !o.CancelAt.IsZero() && !o.CancelAt.After(now) {
			e.at, e.cancel = o.CancelAt, true
		}
		if !o.Hold && o.Price > 0 {
			if t, ok := b.crossing(o, now); ok && (e.at.IsZero() || t.Before(e.at)) {
				e.at, e.cancel = t, false
			} else if !ok {
				o.EvalAt = now
			}
		}
		if !e.at.IsZero() {
			evs = append(evs, e)
		}
	}
	slices.SortFunc(evs, func(x, y event) int { return cmp.Or(x.at.Compare(y.at), orderBefore(x.o, y.o)) })
	for _, e := range evs {
		if e.cancel {
			b.close(e.o, stCanceled, e.at)
		} else {
			b.firstFill(e.o, e.o.Price, e.at)
		}
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
