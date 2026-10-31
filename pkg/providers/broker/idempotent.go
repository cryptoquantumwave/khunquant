package broker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// Defaults of the reconcile.
const (
	// ReconcileSettleDelay is how long after the FIRST attempt began a retry waits
	// before it will conclude "no such order, place it again". An exchange may take
	// a moment to show an order it has just accepted, so an empty answer from a
	// moment ago proves nothing.
	ReconcileSettleDelay = 5 * time.Second

	// ReconcileWindowSlack is how far before the first attempt a reconcile looks
	// (clock differences between this host and the exchange).
	ReconcileWindowSlack = 2 * time.Minute

	// reconcileListLimit is the page size asked of the closed-orders and trades
	// listings. A listing that returns this many rows is treated as truncated.
	// 100 is within every adapter's cap (Bitkub 100, Settrade 100, Binance TH 200).
	reconcileListLimit = 100

	// amountTolerance is the relative amount difference still counted as the same
	// order; priceTolerance the same for a limit price (or one tick, if larger).
	amountTolerance = 0.005
	priceTolerance  = 0.001
)

// OrderRequest is an order to place, spot or futures, in one type. It is one type
// rather than two entry points so a workflow step that places an order has a
// single call whatever the market; the fields only a futures order has are marked.
type OrderRequest struct {
	// Market is OrderMarketSpot or OrderMarketFutures.
	Market string
	Symbol string
	// Type is "market" or "limit" for a capable adapter (anything else is
	// reconciled by shape). Side is "buy" or "sell" (the order direction; for a
	// futures order the position side goes in PositionSide).
	Type, Side string
	// Amount is in the unit the adapter's order call takes: base currency for spot,
	// a CONTRACT count for futures (see contractsFromNotional in pkg/tools).
	Amount float64
	Price  *float64
	// PriceTick is the symbol's price increment, if the caller knows it; the shape
	// match treats two limit prices closer than max(PriceTick, 0.1%) as the same.
	PriceTick float64

	// Futures only.
	ReduceOnly   bool
	MarginMode   string
	PositionSide string
	// PositionBefore is the contracts held in Symbol (on PositionSide, if set) just
	// before the first attempt, recorded by an earlier journaled step. It lets a
	// reconcile of an adapter with no client order IDs and no closed-orders listing
	// notice that a position changed by about Amount. Optional.
	PositionBefore *float64

	// Params go to the adapter as they are (copied), plus the client order ID.
	// They must not carry a client order ID of their own.
	Params map[string]any
}

// PlaceOutcome says what a successful PlaceOrderIdempotent did, for the caller's
// accounting (see the rate limiter note on PlaceOrderIdempotent).
type PlaceOutcome string

const (
	// OutcomePlaced: attempt 1, or a retry that was entitled to place, and a place
	// call was made once.
	OutcomePlaced PlaceOutcome = "placed"
	// OutcomeFoundExisting: an earlier attempt's order was found and returned;
	// nothing was placed.
	OutcomeFoundExisting PlaceOutcome = "found_existing"
	// OutcomePlacedAfterReconcile: a retry looked, found no order, and placed.
	OutcomePlacedAfterReconcile PlaceOutcome = "placed_after_reconcile"
)

// PlaceOption configures one PlaceOrderIdempotent call. The clock and the delay
// are injected here, not held in package variables.
type PlaceOption func(*placer)

// WithClock sets the time source used for the settle delay (default: the SetClock hook, else time.Now).
func WithClock(now func() time.Time) PlaceOption {
	return func(p *placer) {
		if now != nil {
			p.now = now
		}
	}
}

// WithReconcileSettleDelay overrides ReconcileSettleDelay. Negative is zero.
func WithReconcileSettleDelay(d time.Duration) PlaceOption {
	return func(p *placer) {
		if d < 0 {
			d = 0
		}
		p.settle = d
	}
}

// WithOutcome receives the outcome of a successful call and the client order ID
// used ("" when the adapter is not capable).
func WithOutcome(f func(PlaceOutcome, string)) PlaceOption {
	return func(p *placer) { p.onOutcome = f }
}

type placer struct {
	now       func() time.Time
	settle    time.Duration
	slack     time.Duration
	limit     int
	onOutcome func(PlaceOutcome, string)
}

func newPlacer(opts []PlaceOption) *placer {
	p := &placer{now: processNow, settle: ReconcileSettleDelay, slack: ReconcileWindowSlack, limit: reconcileListLimit}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *placer) report(o PlaceOutcome, cid string) {
	if p.onOutcome != nil {
		p.onOutcome(o, cid)
	}
}

// PlaceOrderIdempotent places req once per idemKey. p is the provider the caller
// got from TradingProviderFor (spot) or the futures provider; this function never
// resolves a provider itself, so in paper mode the simulation is what it places
// and reconciles in, and no call reaches a live exchange.
//
// idemKey is journal Ctx.IdempotencyKey(step); attempt and firstStartedAt are the
// step function's attempt number and Ctx.StepStartedAt() (the time the FIRST
// attempt began). The caller's step is declared with journal.Retries, a
// journal.Backoff of at least ReconcileSettleDelay and
// journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) }).
//
// # Decision table
//
//	attempt  adapter                  what the lookups return                       action
//	1        any                      (no lookup is made)                            place; attach the client order ID if capable
//	>1       capable, by-ID lookup    lookup fails                                   ErrReconcileUnavailable, nothing placed
//	>1       capable, by-ID lookup    found (symbol and side agree)                  return it, nothing placed
//	>1       capable, by-ID lookup    found, symbol or side differ                   ErrReconcileAmbiguous
//	>1       capable, by-ID lookup    not found, settle delay not passed             ErrReconcileUnavailable
//	>1       capable, by-ID lookup    not found, settle delay passed                 place again with the SAME client order ID
//	>1       capable, no by-ID        open+closed listing searched by client ID: same rules (a listing call that fails,
//	                                  or is truncated, is ErrReconcileUnavailable)
//	>1       not capable              any listing call fails                         ErrReconcileUnavailable (retryable: a retry can only find it)
//	>1       not capable              exactly one shape match                        return it
//	>1       not capable              two or more shape matches                      ErrReconcileAmbiguous
//	>1       not capable              none, settle delay not passed                  ErrReconcileUnavailable (the order may still appear)
//	>1       not capable              none, settle delay passed (or start unknown)   ErrReconcileUnproven: NOTHING IS EVER PLACED
//	>1       futures, any             no open match; position moved by ~Amount       ErrReconcileAmbiguous
//	>1       futures, any             no open match; no such move (or no baseline)   ErrReconcileUnproven
//
// A request the adapter cannot tag with a client order ID never produces a second
// order from this function: a shape match cannot prove an absence (a listing can
// be cut after the exchange's limit, ignore the window, lack timestamps, or show
// our own order with another type or price, as Settrade's odd-lot rewrite does),
// so on attempt > 1 it can only find the earlier order, report two candidates, or
// stop with ErrReconcileUnproven, which is not retryable: the workflow fails and
// its owner checks the exchange. Only the capable path places on a retry.
//
// The capable path's residual: a by-ID "not found" for an order that does exist,
// after the settle delay, places a second order unless the exchange refuses a
// repeated client order ID. That the exchange refuses it is NOT established from
// the pinned ccxt source for Binance or OKX (Binance documents uniqueness among
// open orders only), so the lookup and the delay are all that prevent it.
//
// After a place call that fails on a retry of a capable adapter the client order
// ID is looked up once more: an exchange that refuses the repeated ID answers
// with a plain rejection (binance -2010 and okx 51016 are both InvalidOrder in the
// pinned ccxt, not DuplicateOrderId), and the order then exists. Found: returned.
//
// A place call that fails is classified (see ErrOutcomeUnknown): the exchange's
// own definite rejection is returned unchanged; anything else, and every failure
// of a call whose context ended, is ErrOutcomeUnknown, so a journaled step with
// Retries retries it and reconciles instead of recording a rejection for an
// order that may exist. An error carrying no ccxt type (Bitkub, Binance TH,
// Settrade and Webull return untyped strings) is therefore unknown too, even when
// it is a plain validation failure: it is retried, up to the step's Retries and
// the journal's MaxStepAttempts, and refused each time by the same validation.
//
// # What it does not do
//
// No gate lives here. Trade permission, CheckRiskNotional, the per-account daily
// loss check, CheckLeverage and DefaultLimiter.Allow belong to the caller's step
// and run, in that order, before this call. The rate limiter token is the
// caller's decision: spend it right before calling on attempt 1; on a retry, call
// Allow only if WithOutcome later reports a placement and accept that the token
// is spent after the fact, or skip it for a retry — a reconcile that finds the
// order places nothing and costs no token.
//
// # Limits, in the open
//
//   - Adapters ignore the context (ccxt takes none), so cancelling it cannot abort
//     a call already in flight. The call is waited for, never abandoned: an
//     abandoned call could still land after the next attempt's reconcile looked.
//   - Shape matching mistakes the owner's own identical manual order for ours
//     (it then returns it and places nothing: the safe direction) and cannot tell
//     two of our own identical orders apart (ambiguous: nothing placed).
//   - Listings can hide truncation: Binance TH and ccxt Binance filter "closed"
//     rows after the exchange applied its limit, and Bitkub ignores since and
//     caps at 100. That is why "none" from a shape match is never acted on.
//     An adapter with a paged listing (PagedListings, Webull) reports the cut
//     itself, from the raw page, for the open and the closed listing alike.
//   - In paper mode the simulation is in memory and forgets, and is not
//     capable: its listings are empty, so a retry ends in ErrReconcileUnproven.
//     Nothing is placed, in the simulation or anywhere, and no live call is made.
//   - A repeated client order ID is refused by the exchange only if it enforces
//     uniqueness; the pinned source does not show that it does (Binance documents
//     uniqueness among open orders only), so the lookup, not the exchange, is
//     what prevents the second order.
func PlaceOrderIdempotent(ctx context.Context, p Provider, req OrderRequest, idemKey string, attempt int, firstStartedAt time.Time, opts ...PlaceOption) (ccxt.Order, error) {
	pl := newPlacer(opts)
	return pl.place(ctx, p, &req, idemKey, attempt, firstStartedAt)
}

// target is the order surface of the provider for the request's market.
type target struct {
	spot TradingProvider
	fut  FuturesProvider
}

func (t target) create(ctx context.Context, req *OrderRequest, params map[string]any) (ccxt.Order, error) {
	if t.fut != nil {
		return t.fut.CreateFuturesOrder(ctx, FuturesOrderRequest{
			Symbol: req.Symbol, OrderType: req.Type, Side: req.Side, Amount: req.Amount, Price: req.Price,
			MarginMode: req.MarginMode, PositionSide: req.PositionSide, ReduceOnly: req.ReduceOnly, Params: params,
		})
	}
	return t.spot.CreateOrder(ctx, req.Symbol, req.Type, req.Side, req.Amount, req.Price, params)
}

func (t target) openOrders(ctx context.Context, symbol string) ([]ccxt.Order, error) {
	if t.fut != nil {
		return t.fut.FetchFuturesOpenOrders(ctx, symbol)
	}
	return t.spot.FetchOpenOrders(ctx, symbol)
}

func (pl *placer) place(ctx context.Context, p Provider, req *OrderRequest, idemKey string, attempt int, first time.Time) (ccxt.Order, error) {
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.Market = strings.ToLower(strings.TrimSpace(req.Market))
	if err := validateRequest(p, req, idemKey, attempt); err != nil {
		return ccxt.Order{}, err
	}
	tgt, err := resolveTarget(p, req)
	if err != nil {
		return ccxt.Order{}, err
	}
	key, maxLen, capable := clientIDSpec(p, req)
	cid := ""
	if capable {
		cid = ClientOrderID(idemKey, maxLen)
	}

	reconciled := false
	if attempt > 1 {
		order, found, err := pl.reconcile(ctx, p, tgt, req, cid, first)
		if err != nil {
			return ccxt.Order{}, err
		}
		if found {
			pl.report(OutcomeFoundExisting, cid)
			return order, nil
		}
		reconciled = true
	}

	// Nothing has been sent yet: a context that ended is a plain refusal.
	if err := ctx.Err(); err != nil {
		return ccxt.Order{}, fmt.Errorf("broker: order not sent: %w", err)
	}

	params := make(map[string]any, len(req.Params)+1)
	for k, v := range req.Params {
		params[k] = v
	}
	if capable {
		params[key] = cid
	}
	order, err := tgt.create(ctx, req, params)
	if err == nil {
		if reconciled {
			pl.report(OutcomePlacedAfterReconcile, cid)
		} else {
			pl.report(OutcomePlaced, cid)
		}
		return order, nil
	}
	o, found, perr := pl.afterPlaceError(ctx, p, tgt, req, cid, attempt, err)
	if found {
		return o, nil
	}
	return ccxt.Order{}, perr
}

// afterPlaceError turns the failure of a place call into the error the journal
// should see.
func (pl *placer) afterPlaceError(ctx context.Context, p Provider, tgt target, req *OrderRequest, cid string, attempt int, err error) (ccxt.Order, bool, error) {
	if cid != "" && attempt > 1 {
		// The exchange may have refused a repeat of an ID it already holds.
		if o, found, ok := pl.findByClientID(ctx, p, tgt, req, cid); ok && found {
			pl.report(OutcomeFoundExisting, cid)
			return o, true, nil
		}
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ccxt.Order{}, false, newOrderError(ErrOutcomeUnknown, req, cid, 0, "the context ended during the place call; the order may exist ("+causeLabel(err)+")", err)
	}
	if typ, ok := CCXTErrorType(err); ok && typ == "DuplicateOrderId" && cid != "" {
		return ccxt.Order{}, false, newOrderError(ErrReconcileAmbiguous, req, cid, 0,
			"the exchange reports a duplicate client order ID but no order with it was found", err)
	}
	if isDefiniteRejection(err) {
		return ccxt.Order{}, false, err
	}
	return ccxt.Order{}, false, newOrderError(ErrOutcomeUnknown, req, cid, 0, "the place call failed and the order may exist ("+causeLabel(err)+")", err)
}

func validateRequest(p Provider, req *OrderRequest, idemKey string, attempt int) error {
	bad := func(reason string) error { return newOrderError(ErrInvalidOrderRequest, req, "", 0, reason, nil) }
	switch {
	case p == nil:
		return bad("no provider")
	case strings.TrimSpace(idemKey) == "":
		return bad("an idempotency key is required")
	case attempt < 1:
		return bad("attempt must be at least 1")
	case req.Market != OrderMarketSpot && req.Market != OrderMarketFutures:
		return bad("market must be spot or futures")
	case req.Symbol == "":
		return bad("symbol is required")
	case req.Side != "buy" && req.Side != "sell":
		return bad("side must be buy or sell")
	case req.Type == "":
		return bad("order type is required")
	case !(finite(req.Amount) && req.Amount > 0):
		return bad("amount must be a positive, finite number")
	case req.Price != nil && !(finite(*req.Price) && *req.Price > 0):
		return bad("price must be a positive, finite number")
	case req.Type == "limit" && req.Price == nil:
		return bad("a limit order needs a price")
	case !finite(req.PriceTick) || req.PriceTick < 0:
		return bad("price tick must be a finite, non-negative number")
	case req.PositionBefore != nil && !finite(*req.PositionBefore):
		return bad("position baseline must be finite")
	case req.Market == OrderMarketSpot && (req.ReduceOnly || req.MarginMode != "" || req.PositionSide != "" || req.PositionBefore != nil):
		return bad("futures-only fields set on a spot order")
	}
	for _, k := range clientIDKeys {
		if v, ok := req.Params[k]; ok && v != nil {
			return bad("params must not carry a client order ID (" + k + "): it is derived from the idempotency key")
		}
	}
	return nil
}

func resolveTarget(p Provider, req *OrderRequest) (target, error) {
	if req.Market == OrderMarketFutures {
		if _, sim := p.(*SimulationProvider); sim {
			return target{}, fmt.Errorf("%w: futures orders have no simulation", ErrPaperModeUnsupported)
		}
		fp, ok := p.(FuturesProvider)
		if !ok {
			return target{}, newOrderError(ErrInvalidOrderRequest, req, "", 0, "provider "+p.ID()+" does not trade futures", nil)
		}
		return target{fut: fp}, nil
	}
	tp, ok := p.(TradingProvider)
	if !ok {
		return target{}, newOrderError(ErrInvalidOrderRequest, req, "", 0, "provider "+p.ID()+" cannot place orders", nil)
	}
	return target{spot: tp}, nil
}

// clientIDSpec reports whether req can carry a client order ID on p, and how.
func clientIDSpec(p Provider, req *OrderRequest) (key string, maxLen int, ok bool) {
	c, isCap := p.(ClientIDCapable)
	if !isCap || !plainOrder(req) {
		return "", 0, false
	}
	key, maxLen, ok = c.ClientOrderIDKey(req.Market)
	if !ok || key == "" || maxLen < MinClientIDLen {
		return "", 0, false
	}
	return key, maxLen, true
}

// plainOrder: a market or limit order with no trigger parameter, the only kinds
// whose client-ID field and fetch endpoint are the ones established.
func plainOrder(req *OrderRequest) bool {
	if req.Type != "market" && req.Type != "limit" {
		return false
	}
	for _, k := range triggerKeys {
		if v, ok := req.Params[k]; ok && v != nil {
			return false
		}
	}
	return true
}

// ---- reconcile ----

func (pl *placer) unavailable(req *OrderRequest, cid, reason string, cause error) error {
	return newOrderError(ErrReconcileUnavailable, req, cid, 0, reason, cause)
}

// settled reports whether enough time has passed since the first attempt for an
// empty answer to be believed.
func (pl *placer) settled(req *OrderRequest, cid string, first time.Time) error {
	if first.IsZero() {
		return pl.unavailable(req, cid, "the time the first attempt began is unknown", nil)
	}
	if elapsed := pl.now().Sub(first); elapsed < pl.settle {
		return pl.unavailable(req, cid, fmt.Sprintf("only %s since the first attempt began, %s needed before an empty answer is believed",
			elapsed.Round(time.Millisecond), pl.settle), nil)
	}
	return nil
}

// reconcile looks for the order an earlier attempt may have placed. found true:
// return that order. found false with a nil error: nothing was found and placing
// again is safe.
func (pl *placer) reconcile(ctx context.Context, p Provider, tgt target, req *OrderRequest, cid string, first time.Time) (ccxt.Order, bool, error) {
	if cid != "" {
		if lk, ok := p.(ClientIDLookup); ok {
			o, found, err := lk.FetchOrderByClientID(ctx, req.Market, req.Symbol, cid)
			if err != nil {
				return ccxt.Order{}, false, pl.unavailable(req, cid, "the lookup by client order ID failed ("+causeLabel(err)+")", err)
			}
			if found {
				if disagrees(req, o) {
					return ccxt.Order{}, false, newOrderError(ErrReconcileAmbiguous, req, cid, 1,
						"an order with this client order ID has a different symbol or side", nil)
				}
				return o, true, nil
			}
			if err := pl.settled(req, cid, first); err != nil {
				return ccxt.Order{}, false, err
			}
			return ccxt.Order{}, false, nil
		}
	}
	return pl.reconcileByListing(ctx, tgt, req, cid, first)
}

// findByClientID is the single lookup used after a failed place call. ok is false
// when the lookup could not be made or failed.
func (pl *placer) findByClientID(ctx context.Context, p Provider, tgt target, req *OrderRequest, cid string) (ccxt.Order, bool, bool) {
	if lk, ok := p.(ClientIDLookup); ok {
		o, found, err := lk.FetchOrderByClientID(ctx, req.Market, req.Symbol, cid)
		if err != nil {
			return ccxt.Order{}, false, false
		}
		return o, found, true
	}
	lst, err := pl.list(ctx, tgt, req, time.Time{}, false)
	if err != nil {
		return ccxt.Order{}, false, false
	}
	for _, o := range lst.orders {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, true
		}
	}
	return ccxt.Order{}, false, true
}

// disagrees reports whether an order found by client order ID is plainly not the
// requested one.
func disagrees(req *OrderRequest, o ccxt.Order) bool {
	if o.Symbol != nil && !strings.EqualFold(*o.Symbol, req.Symbol) {
		return true
	}
	return o.Side != nil && !strings.EqualFold(*o.Side, req.Side)
}

type listing struct {
	orders    []ccxt.Order
	trades    []ccxt.Trade
	truncated bool
}

// list reads the open orders, then the closed orders, then (when wanted) the
// trades since the window start. Open first: an order that fills between the two
// reads then shows in both (deduplicated), never in neither.
func (pl *placer) list(ctx context.Context, tgt target, req *OrderRequest, windowStart time.Time, wantTrades bool) (listing, error) {
	var l listing
	paged, isPaged := tgt.spot.(PagedListings)
	if tgt.fut != nil {
		isPaged = false
	}
	var open []ccxt.Order
	var err error
	if isPaged {
		var cut bool
		open, cut, err = paged.FetchOpenOrdersPage(ctx, req.Symbol)
		l.truncated = l.truncated || cut
	} else {
		open, err = tgt.openOrders(ctx, req.Symbol)
	}
	if err != nil {
		return l, fmt.Errorf("open orders: %w", err)
	}
	l.orders = append(l.orders, open...)
	if tgt.spot != nil {
		var since *int64
		if !windowStart.IsZero() {
			ms := windowStart.UnixMilli()
			since = &ms
		}
		var closed []ccxt.Order
		var err error
		if isPaged {
			var cut bool
			closed, cut, err = paged.FetchClosedOrdersPage(ctx, req.Symbol, since, pl.limit)
			l.truncated = l.truncated || cut
		} else {
			closed, err = tgt.spot.FetchClosedOrders(ctx, req.Symbol, since, pl.limit)
		}
		if err != nil {
			return l, fmt.Errorf("closed orders: %w", err)
		}
		l.orders = append(l.orders, closed...)
		if len(closed) >= pl.limit {
			l.truncated = true
		}
		if wantTrades {
			trades, err := tgt.spot.FetchMyTrades(ctx, req.Symbol, since, pl.limit)
			if err != nil {
				return l, fmt.Errorf("trades: %w", err)
			}
			l.trades = trades
			if len(trades) >= pl.limit {
				l.truncated = true
			}
		}
	}
	l.orders = dedupeOrders(l.orders)
	return l, nil
}

func dedupeOrders(in []ccxt.Order) []ccxt.Order {
	seen := make(map[string]struct{}, len(in))
	out := make([]ccxt.Order, 0, len(in))
	for _, o := range in {
		if o.Id != nil {
			if _, dup := seen[*o.Id]; dup {
				continue
			}
			seen[*o.Id] = struct{}{}
		}
		out = append(out, o)
	}
	return out
}

func (pl *placer) reconcileByListing(ctx context.Context, tgt target, req *OrderRequest, cid string, first time.Time) (ccxt.Order, bool, error) {
	byShape := cid == ""
	var windowStart time.Time
	if !first.IsZero() {
		windowStart = first.Add(-pl.slack)
	} else if byShape {
		return ccxt.Order{}, false, newOrderError(ErrReconcileUnproven, req, cid, 0,
			"the time the first attempt began is unknown, so no window can be searched; nothing was placed", nil)
	}
	lst, err := pl.list(ctx, tgt, req, windowStart, byShape)
	if err != nil {
		return ccxt.Order{}, false, pl.unavailable(req, cid, "a listing call failed ("+causeLabel(err)+")", err)
	}

	var matches []ccxt.Order
	for _, o := range lst.orders {
		if byShape {
			if shapeMatches(req, o, windowStart) {
				matches = append(matches, o)
			}
		} else if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			matches = append(matches, o)
		}
	}
	if byShape && len(lst.trades) > 0 {
		matches = append(matches, tradeMatches(req, lst, windowStart)...)
	}

	switch {
	case len(matches) == 1:
		if !byShape && disagrees(req, matches[0]) {
			return ccxt.Order{}, false, newOrderError(ErrReconcileAmbiguous, req, cid, 1,
				"an order with this client order ID has a different symbol or side", nil)
		}
		return matches[0], true, nil
	case len(matches) > 1:
		return ccxt.Order{}, false, newOrderError(ErrReconcileAmbiguous, req, cid, len(matches),
			"more than one order matches; a person must decide which, if any, is this order", nil)
	}

	// Nothing matched.
	if req.Market == OrderMarketFutures {
		// No closed-orders listing: an order that filled is nowhere to be seen.
		if evidence, err := pl.positionMoved(ctx, tgt, req); err != nil {
			return ccxt.Order{}, false, pl.unavailable(req, cid, "the position lookup failed ("+causeLabel(err)+")", err)
		} else if evidence {
			return ccxt.Order{}, false, newOrderError(ErrReconcileAmbiguous, req, cid, 0,
				"no open order matches but the position changed by about the order amount since the first attempt; a person must decide", nil)
		}
		return ccxt.Order{}, false, newOrderError(ErrReconcileUnproven, req, cid, 0,
			"futures have no closed-orders listing here, so the absence of the order cannot be shown; nothing was placed", nil)
	}
	if byShape {
		// A shape match cannot prove absence (see ErrReconcileUnproven): never place.
		// While the settle delay has not passed an order may still appear, so that
		// one answer stays retryable; after it, "none" is unproven.
		if err := pl.settled(req, cid, first); err != nil {
			return ccxt.Order{}, false, err
		}
		reason := "no order of this shape was found; the earlier attempt may or may not have reached the exchange; nothing was placed"
		if lst.truncated {
			reason = "no order of this shape was found in a listing that may be truncated; the earlier attempt may or may not have reached the exchange; nothing was placed"
		}
		return ccxt.Order{}, false, newOrderError(ErrReconcileUnproven, req, cid, 0, reason, nil)
	}
	// Capable adapter, listing search: "none" is believed when it is complete and settled.
	if lst.truncated {
		return ccxt.Order{}, false, pl.unavailable(req, cid, "a listing may be truncated, so the order may be beyond it", nil)
	}
	if err := pl.settled(req, cid, first); err != nil {
		return ccxt.Order{}, false, err
	}
	return ccxt.Order{}, false, nil
}

// ---- matching ----

func within(a, b, rel float64) bool {
	return b > 0 && math.Abs(a-b) <= rel*b
}

func windowMs(windowStart time.Time) int64 {
	if windowStart.IsZero() {
		return math.MinInt64
	}
	return windowStart.UnixMilli()
}

// shapeMatches: same symbol, side and type; amount within 0.5%; a limit order at
// the same price within one tick or 0.1%; not older than the window. A field the
// listing leaves empty cannot rule the order out, so it does not.
func shapeMatches(req *OrderRequest, o ccxt.Order, windowStart time.Time) bool {
	if o.Symbol != nil && !strings.EqualFold(*o.Symbol, req.Symbol) {
		return false
	}
	if o.Side != nil && !strings.EqualFold(*o.Side, req.Side) {
		return false
	}
	if o.Type != nil && !strings.EqualFold(*o.Type, req.Type) {
		return false
	}
	if o.Timestamp != nil && *o.Timestamp < windowMs(windowStart) {
		return false
	}
	if o.Amount != nil && !within(*o.Amount, req.Amount, amountTolerance) {
		return false
	}
	if req.Type == "limit" && req.Price != nil && o.Price != nil {
		tol := math.Max(req.PriceTick, priceTolerance**req.Price)
		if math.Abs(*o.Price-*req.Price) > tol {
			return false
		}
	}
	return true
}

// tradeMatches turns the trades that no listed order accounts for into candidate
// orders: trades of one order are summed; a trade naming no order stands alone.
func tradeMatches(req *OrderRequest, l listing, windowStart time.Time) []ccxt.Order {
	listed := make(map[string]struct{}, len(l.orders))
	for _, o := range l.orders {
		if o.Id != nil {
			listed[*o.Id] = struct{}{}
		}
	}
	type group struct {
		id     string
		amount float64
		cost   float64
		ts     int64
	}
	var groups []*group
	byOrder := map[string]*group{}
	for _, t := range l.trades {
		if t.Symbol != nil && !strings.EqualFold(*t.Symbol, req.Symbol) {
			continue
		}
		if t.Side != nil && !strings.EqualFold(*t.Side, req.Side) {
			continue
		}
		if t.Timestamp != nil && *t.Timestamp < windowMs(windowStart) {
			continue
		}
		if t.Amount == nil {
			continue
		}
		var g *group
		if t.Order != nil && *t.Order != "" {
			if _, covered := listed[*t.Order]; covered {
				continue
			}
			g = byOrder[*t.Order]
			if g == nil {
				g = &group{id: *t.Order}
				byOrder[*t.Order] = g
				groups = append(groups, g)
			}
		} else {
			g = &group{}
			if t.Id != nil {
				g.id = *t.Id
			}
			groups = append(groups, g)
		}
		g.amount += *t.Amount
		if t.Cost != nil {
			g.cost += *t.Cost
		} else if t.Price != nil {
			g.cost += *t.Price * *t.Amount
		}
		if t.Timestamp != nil && *t.Timestamp > g.ts {
			g.ts = *t.Timestamp
		}
	}
	var out []ccxt.Order
	for _, g := range groups {
		if !within(g.amount, req.Amount, amountTolerance) {
			continue
		}
		id, sym, side := g.id, req.Symbol, req.Side
		amt, zero, closed := g.amount, 0.0, "closed"
		o := ccxt.Order{
			Id: &id, Symbol: &sym, Side: &side, Amount: &amt, Filled: &amt, Remaining: &zero, Status: &closed,
			Info: map[string]any{"reconciled_from": "trades"},
		}
		if g.cost > 0 && g.amount > 0 {
			avg := g.cost / g.amount
			o.Average, o.Cost = &avg, &g.cost
		}
		if g.ts > 0 {
			ts := g.ts
			o.Timestamp = &ts
		}
		out = append(out, o)
	}
	return out
}

// positionMoved reports whether the position in req.Symbol changed by about the
// order amount since the baseline. Without a baseline there is no evidence.
func (pl *placer) positionMoved(ctx context.Context, tgt target, req *OrderRequest) (bool, error) {
	if req.PositionBefore == nil || tgt.fut == nil {
		return false, nil
	}
	positions, err := tgt.fut.FetchFuturesPositions(ctx, []string{req.Symbol})
	if err != nil {
		return false, err
	}
	var now float64
	for _, pos := range positions {
		if pos.Symbol == nil || !strings.EqualFold(*pos.Symbol, req.Symbol) || pos.Contracts == nil {
			continue
		}
		if req.PositionSide != "" && (pos.Side == nil || !strings.EqualFold(*pos.Side, req.PositionSide)) {
			continue
		}
		now += math.Abs(*pos.Contracts)
	}
	delta := math.Abs(now - math.Abs(*req.PositionBefore))
	return within(delta, req.Amount, amountTolerance), nil
}
