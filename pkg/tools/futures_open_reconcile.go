package tools

// futures_open_reconcile.go: what futures_open_position does when the exchange may
// have created the entry order but the answer never arrived.
//
// Before, any error from the entry call was reported as "entry order failed" and the
// tool stopped. When the order did exist that was wrong twice over: the person (or the
// model) retried and the position doubled, and the tool had returned before step 11,
// so a real position stood with no stop-loss and nothing said so.
//
// Now the entry carries a client order ID (when the adapter takes one) and a failure
// is classified: the exchange's own definite rejection stays a plain failure, and
// anything else is an unknown outcome. An unknown outcome only ever LOOKS, after the
// exchange has had time to show the order. It never places again inside the tool:
//
//	found                     the normal flow carries on (order feed, fill check, SL/TP)
//	not found / look failed   an error result that says the outcome is unknown, that no
//	                          protection was placed, and that positions must be checked
//
// and the account, symbol and position side are marked, so the next open on them is
// refused until a look shows that nothing changed.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
	"github.com/google/uuid"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const (
	// futuresUnknownEntryWindow is how long an unresolved entry keeps its marker. It is
	// long enough to outlast a model's immediate retries and a person reading the
	// result, and short enough that an owner who has checked is not locked out for long.
	// A marker also dies with the process (see openUnknowns).
	futuresUnknownEntryWindow = 10 * time.Minute

	// futuresUnknownEntryClearAfter is the least age at which a later call may clear a
	// marker, even when its look finds nothing. 12 times the in-tool settle delay: an
	// order the exchange accepted and still does not show a minute later is not a lag
	// that a look can be expected to see through, and the calls that matter most (an
	// immediate retry) come seconds after the first.
	futuresUnknownEntryClearAfter = time.Minute

	// futuresUnknownEntryMax bounds the markers held. Past it the oldest is dropped to
	// make room: recording an unknown outcome must never be refused.
	futuresUnknownEntryMax = 256
)

// Overridden in tests only.
var (
	// futuresOpenSettle is how long to wait, in wall time, before looking for an
	// order whose answer was lost: the exchange's view of it lags. The other direct
	// order tools wait the same (order_place_once.go), and futuresOpenNow is their
	// clock too.
	futuresOpenSettle = broker.ReconcileSettleDelay
	futuresOpenNow    = time.Now
)

// ---- position snapshot ----

// positionSnapshot is the contracts held in one symbol, by position side ("long",
// "short", "net" or ""). Every side is kept: in one-way mode an entry on the other
// side changes this side's size, which a snapshot of one side would not show.
type positionSnapshot map[string]float64

func snapshotPositions(ctx context.Context, fp broker.FuturesProvider, symbol string) (positionSnapshot, error) {
	positions, err := fp.FetchFuturesPositions(ctx, []string{symbol})
	if err != nil {
		return nil, err
	}
	snap := positionSnapshot{}
	for _, pos := range positions {
		if pos.Symbol == nil || !strings.EqualFold(*pos.Symbol, symbol) || pos.Contracts == nil {
			continue
		}
		side := ""
		if pos.Side != nil {
			side = strings.ToLower(*pos.Side)
		}
		snap[side] += math.Abs(*pos.Contracts)
	}
	return snap, nil
}

func (s positionSnapshot) on(side string) float64 { return s[strings.ToLower(side)] }

// unchangedFrom reports whether no side differs from o. A value that is not a
// number counts as a difference.
func (s positionSnapshot) unchangedFrom(o positionSnapshot) bool {
	same := func(a, b float64) bool {
		return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	}
	for side, v := range s {
		if !same(v, o[side]) {
			return false
		}
	}
	for side, v := range o {
		if _, ok := s[side]; !ok && !same(0, v) {
			return false
		}
	}
	return true
}

// ---- the marker ----

// unknownEntry is an entry order whose outcome was not settled.
type unknownEntry struct {
	key     string
	since   time.Time // when the outcome became unknown
	first   time.Time // when the order was sent
	req     broker.OrderRequest
	idemKey string
	cid     string
	snap    positionSnapshot // the symbol's positions before the order; nil when unreadable
}

// openUnknowns holds the unresolved entries, in memory only: a restart forgets them,
// and the first open after a restart is not looked at. Guarded by its own mutex; pruned
// on every access and capped at futuresUnknownEntryMax, so it cannot grow without bound.
var openUnknowns = struct {
	sync.Mutex
	m map[string]*unknownEntry
}{m: map[string]*unknownEntry{}}

func pruneUnknownsLocked(now time.Time) {
	for k, e := range openUnknowns.m {
		if now.Sub(e.since) >= futuresUnknownEntryWindow {
			delete(openUnknowns.m, k)
		}
	}
}

func putUnknownEntry(e *unknownEntry, now time.Time) {
	openUnknowns.Lock()
	defer openUnknowns.Unlock()
	pruneUnknownsLocked(now)
	if _, replacing := openUnknowns.m[e.key]; !replacing {
		for len(openUnknowns.m) >= futuresUnknownEntryMax {
			var oldest *unknownEntry
			for _, o := range openUnknowns.m {
				if oldest == nil || o.since.Before(oldest.since) {
					oldest = o
				}
			}
			delete(openUnknowns.m, oldest.key)
		}
	}
	openUnknowns.m[e.key] = e
}

func getUnknownEntry(key string, now time.Time) *unknownEntry {
	openUnknowns.Lock()
	defer openUnknowns.Unlock()
	pruneUnknownsLocked(now)
	return openUnknowns.m[key]
}

// dropUnknownEntry removes e, but not a newer marker that replaced it meanwhile.
func dropUnknownEntry(e *unknownEntry) {
	openUnknowns.Lock()
	defer openUnknowns.Unlock()
	if openUnknowns.m[e.key] == e {
		delete(openUnknowns.m, e.key)
	}
}

func resetUnknownEntries() {
	openUnknowns.Lock()
	defer openUnknowns.Unlock()
	openUnknowns.m = map[string]*unknownEntry{}
}

func unknownEntryKey(cfg *config.Config, providerID, account, symbol, positionSide string) string {
	return broker.AccountKey(cfg, providerID, account) + "|" + strings.ToUpper(symbol) + "|" + strings.ToLower(positionSide)
}

// ---- params ----

// paramsCarryClientOrderID reports whether params name a client order ID. The tool sets
// its own, derived from a key unique to the call, so that it can find the order again;
// one from the caller would be refused downstream, after the rate limit token and the
// leverage change.
func paramsCarryClientOrderID(params map[string]interface{}) bool {
	for k := range params {
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "clientorderid", "clordid", "newclientorderid":
			return true
		}
	}
	return false
}

// ---- the next call ----

// refuseWhileUnknown is the marker's gate. It looks (never places, never spends a rate
// limit token) and returns a refusal, or nil once a look shows that nothing changed.
// Only an open goes through it: an exit is never gated by it.
func refuseWhileUnknown(ctx context.Context, fp broker.FuturesProvider, key, providerID, symbol, positionSide string) *ToolResult {
	e := getUnknownEntry(key, futuresOpenNow())
	if e == nil {
		return nil
	}
	refuse := func(why string) *ToolResult {
		until := e.since.Add(futuresUnknownEntryWindow).UTC().Format("15:04:05")
		return ErrorResult(fmt.Sprintf("futures_open_position refused: an earlier entry order for %s %s on %s (client order ID %s, outcome unknown since %s UTC) has not been settled, and %s. "+
			"A position may already exist: check futures_get_positions first. "+
			"Opens on this account, symbol and side stay refused until a check shows that nothing changed, and in any case until %s UTC. Nothing was sent.",
			positionSide, symbol, providerID, orDash(e.cid), e.since.UTC().Format("15:04:05"), why, until)).WithError(broker.ErrOutcomeUnknown)
	}

	order, found, lookErr := broker.FindOrder(ctx, fp, e.req, e.idemKey, e.first)
	switch {
	case found:
		return refuse(fmt.Sprintf("that order is now on the exchange (id %s, status %s), and no stop-loss or take-profit was placed for it: protect it with futures_modify_protection or close it with futures_close_position", orderID(order), futuresStrPtr(order.Status)))
	case errors.Is(lookErr, broker.ErrReconcileAmbiguous):
		return refuse("the exchange now shows a position change of about that order's size, or more than one matching order, and no stop-loss or take-profit was placed for it: protect the position with futures_modify_protection or close it with futures_close_position")
	case lookErr != nil && !errors.Is(lookErr, broker.ErrReconcileUnproven):
		return refuse("the check for it could not be completed just now")
	}
	// No order was shown. That alone proves nothing (a filled order may be nowhere in a
	// listing), so the position must also be exactly as it was before the order.
	if e.snap == nil {
		return refuse("the position could not be read before it, so nothing can show that it did not go through")
	}
	now, err := snapshotPositions(ctx, fp, symbol)
	if err != nil {
		return refuse("the position could not be read just now")
	}
	if !now.unchangedFrom(e.snap) {
		return refuse("the position has changed since, and no stop-loss or take-profit was placed for the earlier entry: if it is the entry's position, protect it with futures_modify_protection or close it with futures_close_position")
	}
	if age := futuresOpenNow().Sub(e.since); age < futuresUnknownEntryClearAfter {
		return refuse(fmt.Sprintf("only %d seconds have passed, and an order can take longer than that to show", int(age.Seconds())))
	}
	dropUnknownEntry(e)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- the entry ----

// waitSettle waits d in wall time, or until ctx ends.
func waitSettle(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// placeFuturesEntry sends the entry order once. It returns the order and, when the
// answer was lost but the order was found afterwards, a note for the result; or a
// result to return (a plain failure, or an unknown outcome).
//
// It calls PlaceOrderIdempotent at attempt 1 with a key unique to this call: a tool
// call is always attempt 1, and a retry by a person or the model is a new call, so
// the retry path of that function (which places again on "not found") must not be
// reached. Nothing here places a second time.
func placeFuturesEntry(ctx context.Context, fp broker.FuturesProvider, req broker.OrderRequest, key, providerID string, snap positionSnapshot) (ccxt.Order, string, *ToolResult) {
	idemKey := "futures_open:" + uuid.NewString()
	first := futuresOpenNow()
	order, err := broker.PlaceOrderIdempotent(ctx, fp, req, idemKey, 1, first)
	if err == nil {
		return order, "", nil
	}
	if !errors.Is(err, broker.ErrOutcomeUnknown) && !errors.Is(err, broker.ErrReconcileAmbiguous) {
		// The exchange's own refusal, or nothing was sent: a plain failure, no marker.
		return ccxt.Order{}, "", ErrorResult(fmt.Sprintf("entry order failed: %v", err)).WithError(err)
	}

	var oe *broker.OrderError
	cid, reason := "", "the place call did not return a clear answer"
	if errors.As(err, &oe) {
		cid, reason = oe.ClientOrderID, oe.Reason
	}

	// Only look, and only after the exchange has had time to show the order.
	var (
		found   bool
		lookErr = waitSettle(ctx, futuresOpenSettle)
		looked  ccxt.Order
	)
	waited := lookErr == nil
	if waited {
		looked, found, lookErr = broker.FindOrder(ctx, fp, req, idemKey, first)
	}
	if found {
		how := "among the open orders by its shape"
		if cid != "" {
			how = "by its client order ID " + cid
		}
		return looked, fmt.Sprintf("The exchange's answer to the entry order was lost (%s). This call then looked for the order and found it %s; it is treated as the one this call placed and nothing was placed twice.", reason, how), nil
	}

	// Not found, or the look could not tell. Mark the account, symbol and side.
	copied := req
	copied.Params = copyToolParams(req.Params)
	if req.Price != nil {
		p := *req.Price
		copied.Price = &p
	}
	now := futuresOpenNow()
	putUnknownEntry(&unknownEntry{key: key, since: now, first: first, req: copied, idemKey: idemKey, cid: cid, snap: snap}, now)

	var how string
	switch {
	case !waited:
		how = "the wait before looking for it was cut short"
	case errors.Is(lookErr, broker.ErrReconcileAmbiguous):
		how = "the position now appears to have changed by about the order's size, or more than one order matches, so the entry most likely went through"
	case lookErr != nil && !errors.Is(lookErr, broker.ErrReconcileUnproven):
		how = fmt.Sprintf("the look for it, made %s later, could not be completed", futuresOpenSettle)
	default:
		how = fmt.Sprintf("a look for it, made %s later, found nothing, which does not show that it was not placed", futuresOpenSettle)
	}
	return ccxt.Order{}, "", ErrorResult(fmt.Sprintf(
		"ENTRY OUTCOME UNKNOWN: the exchange did not give a clear answer to the %s entry order for %.8g contracts of %s on %s, and %s. "+
			"The order may exist, so a position may already be open.\n\n"+
			"NO stop-loss or take-profit was placed, and none will be placed unless you place one.\n"+
			"Check futures_get_positions before doing anything else. If a position exists, protect it with futures_modify_protection or close it with futures_close_position. "+
			"Opens on this account, symbol and side are refused for up to %d minutes unless a check shows that nothing changed.\n"+
			"Client order ID: %s",
		req.PositionSide, req.Amount, req.Symbol, providerID, how, int(futuresUnknownEntryWindow.Minutes()), orDash(cid))).
		WithError(errors.Join(err, lookErr))
}
