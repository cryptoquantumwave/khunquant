package broker

import (
	"context"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// OrderRef names an order that code (not the owner's hand) placed, or may have
// placed, for a journaled workflow: what the kill switch needs to find it again and
// to withdraw it if it is still open. A workflow package builds one from its own
// step records; nothing here touches an exchange.
type OrderRef struct {
	WorkflowID string
	// Kind is the workflow kind, for the owner's summary.
	Kind              string
	Provider, Account string
	Market, Symbol    string
	Side              string
	// OrderID is the exchange's id for the order; empty when the place step never
	// finished (then Request, IdemKey and FirstStartedAt are what a lookup has).
	OrderID string
	// Done: the recorded fill says nothing more will change (filled, cancelled,
	// expired), so there is nothing to withdraw.
	Done bool
	// Exit: the order reduces risk (reduce-only, or a proposal that only reduces
	// it). The kill switch never withdraws one: it is getting the owner out.
	Exit bool
	// Paper: the order was placed in paper mode, so it lives in the simulation only.
	Paper bool

	// Request, IdemKey and FirstStartedAt describe the order a cut place step may
	// have sent, to look it up by its client order ID or, failing that, its shape.
	Request        *OrderRequest
	IdemKey        string
	FirstStartedAt time.Time
}

// FindOrder looks for the order an earlier place attempt may have sent, WITHOUT
// ever placing one. It runs the same lookup PlaceOrderIdempotent runs before it
// retries (the client order ID where the adapter was established to take one, else
// the order's shape in the open and closed listings) but returns where that would
// place again: found false with a nil error means the exchange showed nothing, and
// an error means the lookup could not tell. Neither proves the order does not exist,
// and the caller must say so.
//
// p is the provider the order was placed with (TradingProviderFor for spot: in
// paper mode the simulation, whose listings are empty).
func FindOrder(ctx context.Context, p Provider, req OrderRequest, idemKey string, firstStartedAt time.Time) (ccxt.Order, bool, error) {
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.Market = strings.ToLower(strings.TrimSpace(req.Market))
	if err := validateRequest(p, &req, idemKey, 2); err != nil {
		return ccxt.Order{}, false, err
	}
	tgt, err := resolveTarget(p, &req)
	if err != nil {
		return ccxt.Order{}, false, err
	}
	cid := ""
	if _, maxLen, capable := clientIDSpec(p, &req); capable {
		cid = ClientOrderID(idemKey, maxLen)
	}
	// The settle delay is about whether an empty answer may be believed enough to
	// place again. Nothing is placed here, so it is zero.
	return newPlacer([]PlaceOption{WithReconcileSettleDelay(0)}).reconcile(ctx, p, tgt, &req, cid, firstStartedAt)
}

// FlowOrders is what a workflow's journal says about the orders it placed: the
// answer of a workflow package to the kill switch.
type FlowOrders struct {
	// Orders are the orders the workflow placed, or a cut place step may have placed.
	Orders []OrderRef
	// Settle: a place step has finished (or, for a workflow that cannot be cut safely,
	// begun). The workflow is let to run to its settling steps instead of being
	// cancelled at the next boundary: cancelling would skip the steps that record the
	// order, tell the owner or start the recovery of a half-open position. Every
	// place step re-reads the kill flag, so it places nothing more.
	Settle bool
	// Protect: the workflow only reduces risk (an approved exit), so it is never cancelled.
	Protect bool
	// Note is appended to the workflow's kind in the owner's summary, for a protective
	// workflow whose behaviour under the kill switch needs saying.
	Note string
}
