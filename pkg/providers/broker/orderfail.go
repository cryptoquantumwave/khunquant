package broker

import (
	"context"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// The failures of an idempotent placement (see PlaceOrderIdempotent). They are
// distinct sentinels so a workflow can tell them apart with errors.Is; the errors
// that wrap them carry the order's symbol, side, amount, client order ID and the
// number of matches, and never a credential or an exchange response body.
var (
	// ErrOutcomeUnknown means the place request may have reached the exchange: a
	// timeout, a dropped connection, a 5xx, a cancelled context mid-call, or any
	// error that cannot be classified. The order may exist. It is retryable: the
	// next attempt reconciles before it places anything.
	ErrOutcomeUnknown = errors.New("broker: order outcome unknown")

	// ErrReconcileUnavailable means a retry could not establish whether an earlier
	// attempt placed the order (a lookup failed, a listing may be truncated, too
	// little time has passed, nothing can prove an absence), so it placed nothing.
	// It is retryable: a later attempt may be able to tell.
	ErrReconcileUnavailable = errors.New("broker: cannot establish whether the order already exists")

	// ErrReconcileAmbiguous means the lookup found more than one candidate, or
	// evidence that an order filled that it cannot return, so it placed nothing. It
	// is NOT retryable: asking again gives the same answer and a human must decide.
	ErrReconcileAmbiguous = errors.New("broker: the order may already exist more than once")

	// ErrReconcileUnproven means the earlier attempt may or may not have reached the
	// exchange and nothing found it; nothing was placed, and nothing will be placed
	// by asking again. It is returned for a request the adapter cannot tag with a
	// client order ID: a match by the order's shape cannot prove an absence (a
	// listing can be cut after the exchange's limit, ignore the window, lack
	// timestamps, or show our own order with another type or price), and a wrong
	// "none" is a second real order. NOT retryable: the owner is told to check the
	// exchange and decide.
	ErrReconcileUnproven = errors.New("broker: the earlier attempt may or may not have placed the order; nothing was placed")

	// ErrInvalidOrderRequest means the request itself is unusable (no idempotency
	// key, a non-finite amount, a client order ID the caller set itself...). Nothing
	// was sent.
	ErrInvalidOrderRequest = errors.New("broker: invalid idempotent order request")
)

// OrderError is the error PlaceOrderIdempotent returns for its own refusals. Kind
// is one of the sentinels above (errors.Is matches it). Error() holds only values
// this package built: never the text of the underlying error, which for a ccxt
// error embeds the exchange's full HTTP response. Unwrap returns that underlying
// error for errors.As and errors.Is (a *ccxt.Error, context.Canceled...); do not
// print it.
type OrderError struct {
	Kind          error
	Market        string
	Symbol        string
	Side          string
	Amount        float64
	ClientOrderID string
	// Matches is the number of candidates a reconcile found (0 when it looked
	// and found none, or did not get to look).
	Matches int
	// Reason says why, in words of this package. For a failed call it ends with the
	// class of the failure ("ccxt RequestTimeout", "deadline exceeded"...).
	Reason string

	cause error
}

func (e *OrderError) Error() string {
	var b strings.Builder
	b.WriteString(e.Kind.Error())
	b.WriteString(": ")
	b.WriteString(e.Market)
	b.WriteByte(' ')
	b.WriteString(e.Symbol)
	b.WriteByte(' ')
	b.WriteString(e.Side)
	b.WriteString(" amount=")
	b.WriteString(strconv.FormatFloat(e.Amount, 'g', -1, 64))
	if e.ClientOrderID != "" {
		b.WriteString(" client_order_id=")
		b.WriteString(e.ClientOrderID)
	}
	if e.Matches > 0 {
		b.WriteString(" matches=")
		b.WriteString(strconv.Itoa(e.Matches))
	}
	if e.Reason != "" {
		b.WriteString(": ")
		b.WriteString(e.Reason)
	}
	return b.String()
}

// Is makes errors.Is(err, ErrOutcomeUnknown) and the like work.
func (e *OrderError) Is(target error) bool { return target == e.Kind }

// Unwrap returns the underlying failure, or nil when there was none.
func (e *OrderError) Unwrap() error { return e.cause }

// Retryable reports whether a workflow step that placed an order should be run
// again after err: true for ErrOutcomeUnknown and ErrReconcileUnavailable (the
// next attempt reconciles first), false for ErrReconcileAmbiguous,
// ErrReconcileUnproven, ErrInvalidOrderRequest and for an exchange's own definite
// rejection. Declare the
// step as
//
//	journal.Retries(n), journal.Backoff(d),
//	journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) })
//
// with d at least the settle delay given to PlaceOrderIdempotent, otherwise the
// attempts a "not yet settled" refusal uses up are all spent within milliseconds.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, ErrReconcileAmbiguous) || errors.Is(err, ErrReconcileUnproven) || errors.Is(err, ErrInvalidOrderRequest) {
		return false
	}
	return errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrReconcileUnavailable)
}

// ccxtErrorMarker starts the text of every ccxt error: (*ccxt.Error).Error()
// formats "[ccxtError]::[<Type>]::[<message>]" (ccxt/go/v4@v4.5.76 exchange.go).
// The binance adapter flattens errors to plain strings (compactCCXTError) and the
// okx adapter stringifies recovered panics, so the type survives only as this text.
const ccxtErrorMarker = "[ccxtError]::["

// CCXTErrorType returns the ccxt exception type of err ("InsufficientFunds",
// "OrderNotFound", "RequestTimeout"...), from a wrapped *ccxt.Error or from the
// text an adapter flattened it into. ok is false for any other error.
func CCXTErrorType(err error) (typ string, ok bool) {
	if err == nil {
		return "", false
	}
	var ce *ccxt.Error
	if errors.As(err, &ce) && ce != nil {
		return sanitizeErrType(string(ce.Type))
	}
	msg := err.Error()
	i := strings.Index(msg, ccxtErrorMarker)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(ccxtErrorMarker):]
	j := strings.Index(rest, "]")
	if j < 0 {
		return "", false
	}
	return sanitizeErrType(rest[:j])
}

// sanitizeErrType accepts only an identifier: the type goes into error text.
func sanitizeErrType(s string) (string, bool) {
	if s == "" || len(s) > 40 {
		return "", false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "", false
		}
	}
	return s, true
}

// IsOrderNotFound reports whether err says the exchange has no such order: the
// ccxt OrderNotFound type (binance -2013 and -2011, okx 51400-51402 and 51603 in
// the pinned source). An adapter's FetchOrderByClientID uses it to turn "no such
// order" into found=false.
func IsOrderNotFound(err error) bool {
	typ, ok := CCXTErrorType(err)
	return ok && typ == "OrderNotFound"
}

// definiteRejections are the ccxt exception types that mean the exchange (or ccxt,
// before sending) refused the order: nothing was placed. The list is deliberately
// short. Everything else, including ExchangeError, ExchangeNotAvailable,
// RequestTimeout, NetworkError, RateLimitExceeded, DDoSProtection, InvalidNonce,
// OnMaintenance, BadResponse, NullResponse, OperationFailed, OrderNotFound and
// the catch-all "Exception" ccxt gives a non-ccxt panic string, is unknown.
//
// Source: ccxt/go/v4@v4.5.76 exchange_errors.go (the types) and the exceptions
// tables of binance.go (-2010 InvalidOrder, -2011/-2013 OrderNotFound, -1021
// InvalidNonce, -1003 RateLimitExceeded, -1007 RequestTimeout, -2014/-2015
// AuthenticationError, -4015 BadRequest) and okx.go (51008 InsufficientFunds,
// 51000 BadRequest, 51001/51002 BadSymbol, 51016 InvalidOrder, 50004
// RequestTimeout, 50011 RateLimitExceeded, 51603 OrderNotFound); the HTTP table in
// exchange_generated.go (5xx, 400, 403 -> ExchangeNotAvailable; 408/504 ->
// RequestTimeout; 429 RateLimitExceeded; 401 AuthenticationError); and the
// transport failure in exchange_req.go (NetworkError).
var definiteRejections = map[string]struct{}{
	"InsufficientFunds":        {},
	"InvalidOrder":             {},
	"BadSymbol":                {},
	"BadRequest":               {},
	"PermissionDenied":         {},
	"AuthenticationError":      {},
	"AccountSuspended":         {},
	"AccountNotEnabled":        {},
	"ArgumentsRequired":        {},
	"OperationRejected":        {},
	"MarketClosed":             {},
	"RestrictedLocation":       {},
	"NotSupported":             {},
	"OrderImmediatelyFillable": {},
	"OrderNotFillable":         {},
	"ContractUnavailable":      {},
}

// isDefiniteRejection reports whether err, returned by a place call whose context
// was still live, is the exchange's own refusal.
func isDefiniteRejection(err error) bool {
	typ, ok := CCXTErrorType(err)
	if !ok {
		return false // not a ccxt error: unknown, whatever its text says
	}
	_, def := definiteRejections[typ]
	return def
}

// causeLabel names the class of a failure without quoting it.
func causeLabel(err error) string {
	switch {
	case err == nil:
		return "no error"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	}
	if typ, ok := CCXTErrorType(err); ok {
		return "ccxt " + typ
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "network timeout"
		}
		return "network error"
	}
	return "unclassified error"
}

// newOrderError builds an OrderError for req.
func newOrderError(kind error, req *OrderRequest, cid string, matches int, reason string, cause error) *OrderError {
	return &OrderError{
		Kind: kind, Market: req.Market, Symbol: req.Symbol, Side: strings.ToLower(req.Side),
		Amount: req.Amount, ClientOrderID: cid, Matches: matches, Reason: reason, cause: cause,
	}
}

// finite reports whether f is neither NaN nor infinite.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
