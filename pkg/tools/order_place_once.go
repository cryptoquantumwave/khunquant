package tools

// order_place_once.go: how the direct order tools (create_order, the scheduled DCA run and
// the futures exits close, reduce and flatten) send an order whose answer may be lost.
//
// They used to call the adapter directly, with no client order ID, and report any error
// as a plain failure. When the exchange had accepted the order and only the answer was
// lost, that was wrong: the person or the model placed the order again, and a spot buy
// doubled, or a second reduce-only close took more of the position than was asked.
//
// Now an order goes through broker.PlaceOrderIdempotent at attempt 1 with a key unique to
// the call, as futures_open_position's entry does (futures_open_reconcile.go): an adapter
// that takes one tags the order with a client order ID, and a failure is classified. The
// exchange's own definite rejection stays a plain failure; anything else is an unknown
// outcome. An unknown outcome only LOOKS, once, after the exchange has had time to show
// the order. It never places again:
//
//	found                     the order is used as if its answer had arrived, with a note
//	not found / look failed   an error result that says the outcome is unknown, that the
//	                          order may exist, and what to check before placing it again
//
// Unlike the futures entry, nothing holds back the next call: an exit is never gated, and
// a DCA run whose outcome is unknown leaves an "unknown" row that counts toward the plan's
// period cap.

import (
	"context"
	"errors"
	"fmt"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/google/uuid"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// pendingOrder is an order sent once whose outcome is unknown: the exchange may hold it.
type pendingOrder struct {
	p       broker.Provider
	req     broker.OrderRequest
	idemKey string
	first   time.Time
	cid     string // the client order ID sent, "" when the adapter takes none
	reason  string // why the place call's outcome is unknown
	err     error  // the place call's error; never printed (a ccxt error embeds the response)
	lookErr error
	how     string // set by look: why the outcome stays unknown, in words for the result
}

// sendOrderOnce sends req once, through broker.PlaceOrderIdempotent at attempt 1 with a key
// unique to the call. A tool call is always attempt 1: a retry by a person or the model is
// a new call, so the retry path of that function (which places again on "not found") is
// never reached. It returns the order; or, when the outcome is unknown, the pending order to
// look for; or the error of a call that placed nothing: the exchange's own definite
// rejection, a session that needs re-authentication (Webull refuses before it sends), or a
// request refused before it was sent (PlaceOrderIdempotent sends nothing once ctx has
// ended).
//
// An exit (reduce-only) is the exception: it is sent on a context that ctx's cancellation
// and deadline do not reach. The ccxt adapters ignore the context, so a cancelled turn or
// a sequence deadline never stopped a close before, and must not start to leave open a
// position the tool set out to close. Only the look after an unknown outcome is cut short.
func sendOrderOnce(ctx context.Context, p broker.Provider, req broker.OrderRequest, tool string) (ccxt.Order, *pendingOrder, error) {
	idemKey := tool + ":" + uuid.NewString()
	// The broker's own clock: the look compares this instant with the one the placer reads,
	// which a locked gateway points at its simulated clock (futuresOpenNow stays the wall
	// clock). Unlocked, broker.Now is time.Now.
	first := broker.Now()
	sendCtx := ctx
	if req.ReduceOnly {
		sendCtx = context.WithoutCancel(ctx)
	}
	order, err := broker.PlaceOrderIdempotent(sendCtx, p, req, idemKey, 1, first)
	if err == nil {
		return order, nil, nil
	}
	if errors.Is(err, exchanges.ErrNeedsReauth) || (!errors.Is(err, broker.ErrOutcomeUnknown) && !errors.Is(err, broker.ErrReconcileAmbiguous)) {
		return ccxt.Order{}, nil, err
	}
	po := &pendingOrder{p: p, req: req, idemKey: idemKey, first: first, reason: "the place call did not return a clear answer", err: err}
	var oe *broker.OrderError
	if errors.As(err, &oe) {
		po.cid, po.reason = oe.ClientOrderID, oe.Reason
	}
	return ccxt.Order{}, po, nil
}

// look looks for the pending order once and never places. waited is false when the wait
// before it was cut short, and nothing is looked at then. Found: the order and a note for
// the result. Not found: po.how says why the outcome stays unknown.
func (po *pendingOrder) look(ctx context.Context, waited bool) (ccxt.Order, string, bool) {
	if !waited {
		po.how = "the wait before looking for it was cut short"
		return ccxt.Order{}, "", false
	}
	order, found, lookErr := broker.FindOrder(ctx, po.p, po.req, po.idemKey, po.first)
	if found {
		how := "by its shape (symbol, side, type and amount)"
		if po.cid != "" {
			how = "by its client order ID " + po.cid
		}
		return order, fmt.Sprintf("The exchange's answer to this order was lost (%s). This call then looked for the order and found it %s; it is treated as the one this call placed and nothing was placed twice.", po.reason, how), true
	}
	po.lookErr = lookErr
	switch {
	case errors.Is(lookErr, broker.ErrReconcileAmbiguous):
		po.how = "the look found more than one matching order, or a position change of about the order's size, so the order most likely went through"
	case lookErr != nil && !errors.Is(lookErr, broker.ErrReconcileUnproven):
		po.how = fmt.Sprintf("the look for it, made %s later, could not be completed", futuresOpenSettle)
	default:
		po.how = fmt.Sprintf("a look for it, made %s later, found nothing, which does not show that it was not placed", futuresOpenSettle)
	}
	return ccxt.Order{}, "", false
}

// placeOrderOnce is sendOrderOnce and, for an unknown outcome, the wait and the one look.
// It returns the order (placed, or found after a lost answer, with a note for the result);
// or the pending order the look did not find; or the error of a call that placed nothing.
func placeOrderOnce(ctx context.Context, p broker.Provider, req broker.OrderRequest, tool string) (ccxt.Order, string, *pendingOrder, error) {
	order, po, err := sendOrderOnce(ctx, p, req, tool)
	if err != nil || po == nil {
		return order, "", nil, err
	}
	if looked, note, found := po.look(ctx, waitSettle(ctx, futuresOpenSettle) == nil); found {
		return looked, note, nil, nil
	}
	return ccxt.Order{}, "", po, nil
}

// unknownText starts the result for a pending order the look did not find: what was sent,
// why its outcome is unknown, and that it may exist.
func (po *pendingOrder) unknownText(providerID string) string {
	return fmt.Sprintf("ORDER OUTCOME UNKNOWN: the exchange did not give a clear answer to the %s %s order for %.8g %s on %s, and %s. The order may exist, and it may already have filled.",
		po.req.Side, po.req.Type, po.req.Amount, po.req.Symbol, providerID, po.how)
}

// clientIDLine names the order's client order ID for the result, "-" when it carries none.
func (po *pendingOrder) clientIDLine() string { return "Client order ID: " + orDash(po.cid) }

// unknownErr is the result's Err for a pending order the look did not find.
func (po *pendingOrder) unknownErr() error { return errors.Join(po.err, po.lookErr) }
