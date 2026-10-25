package broker

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"strings"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

// The two markets an order can be placed in.
const (
	OrderMarketSpot    = "spot"
	OrderMarketFutures = "futures"
)

// clientIDPrefix starts every client order ID this package derives.
const clientIDPrefix = "kq"

// MinClientIDLen is the shortest client order ID PlaceOrderIdempotent will use. A
// shorter limit leaves too little of the hash to tell two keys apart.
const MinClientIDLen = 16

var clientIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ClientOrderID derives the client order ID for an idempotency key:
// "kq" + base32(sha256(key)), lower case, no padding, cut to maxLen. The alphabet
// is a-z and 2-7, so it is letters and digits only and valid wherever an exchange
// takes alphanumerics. The same key and maxLen always give the same ID. It
// returns "" for maxLen <= 0; maxLen above 54 yields the whole 54 characters.
//
// Different keys give different IDs for any practical purpose: the hash is 256
// bits and a maxLen of 32 keeps 150 of them.
func ClientOrderID(idemKey string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(idemKey))
	id := clientIDPrefix + strings.ToLower(clientIDEncoding.EncodeToString(sum[:]))
	if len(id) > maxLen {
		id = id[:maxLen]
	}
	return id
}

// ClientIDCapable is implemented ONLY by an adapter whose use of a client order ID
// was established from the pinned client source (below). Capability is opt-in, per
// adapter and per market; what could not be established is "not capable", and
// PlaceOrderIdempotent then reconciles by the order's shape.
//
// # What was established, and from where (ccxt/go/v4@v4.5.76, read, never run)
//
// Binance spot and USDT-M futures (binance.go). The unified params key
// "clientOrderId" (also "newClientOrderId", "clientAlgoId") is read by
// CreateOrderRequest (SafeStringN on those names) and sent as the request field
// "newClientOrderId"; when none is given ccxt generates one from its broker id
// plus a uuid. For a conditional order the field becomes "clientAlgoId" (linear
// swap) or "newClientStrategyId" (portfolio margin), so only market and limit
// orders are treated as capable. Fetching one order by client ID is a params
// key, not a unified call: fetchOrderBody reads "origClientOrderId",
// "clientOrderId" or "clientAlgoId" from params and then ignores the id
// argument (request field "origClientOrderId"); it needs the symbol. The
// unified order carries it back as Order.ClientOrderId (parseOrder:
// SafeStringN of clientOrderId, newClientStrategyId, clientAlgoId), for open
// and closed listings alike. "No such order" is the OrderNotFound type
// (-2013, -2011). fetchClosedOrders is fetchOrders filtered to status "closed":
// a cancelled or expired order is not in it, which is why the by-ID fetch is the
// lookup. Length and charset of the ID: NOT ESTABLISHED in the source (it states
// neither); the conservative intersection is used: letters and digits, at most 32.
//
// OKX spot and USDT swap (okx.go). createOrderRequest reads "clOrdId" or
// "clientOrderId" (SafeString2) and sends "clOrdId" (the unified name is the one
// used here); without one ccxt generates brokerId plus a uuid. fetchOrder reads
// the same two names and sends "clOrdId" instead of "ordId" (the id argument is
// ignored); symbol required. Open/closed listings return it as
// Order.ClientOrderId (parseOrder: clientOrderId from "clOrdId"). "No such
// order" is OrderNotFound (51603, 51400-51402). Length and charset: NOT
// ESTABLISHED in the source (the only limit it quotes is for the unrelated "tag"
// param); same conservative choice.
//
// Not capable, and why:
//
//   - Bitkub (pkg/exchanges/bitkub): the place payload is sym/amt/rat/typ only and
//     CreateOrder discards params; the response model has a "ci" field, but that
//     the place endpoint accepts a client ID is NOT ESTABLISHED.
//   - Binance TH (pkg/exchanges/binanceth): createOrder sets symbol, side, type,
//     quantity, price, timeInForce; params discarded. NOT ESTABLISHED.
//   - Settrade (pkg/exchanges/settrade): createOrderRequest has no client ID
//     field; params discarded.
//   - MT5: every call returns ErrNotImplemented.
//   - Webull (pkg/exchanges/webull): the API takes a client_order_id of at most 32
//     characters (the adapter's own comment; the charset is not stated, so
//     letters and digits). Orders are listed with it. It is capable for spot
//     (equity) orders WITHOUT a by-ID lookup: FetchOrder(clientOrderID) exists,
//     but its failures are untyped strings, so "no such order" cannot be told from
//     an outage; the open and closed listings are searched instead. That the
//     exchange refuses a repeated client_order_id is NOT ESTABLISHED. It has no
//     futures.
//   - SimulationProvider (paper mode): never. Its FetchOrder returns an "open"
//     order for any id, so a lookup would find a ghost, and its listings are empty.
//
// A futures market has no closed-orders listing in FuturesProvider
// (FetchFuturesOrder and FetchFuturesOpenOrders only), so a futures adapter that
// is capable must also implement ClientIDLookup.
type ClientIDCapable interface {
	// ClientOrderIDKey reports, for market OrderMarketSpot or OrderMarketFutures,
	// the params key under which the adapter forwards a client order ID and the
	// longest ID the exchange accepts. ok is false when the adapter cannot.
	ClientOrderIDKey(market string) (paramKey string, maxLen int, ok bool)
}

// ClientIDLookup is implemented by a ClientIDCapable adapter that can fetch one
// order by its client order ID and tell "the exchange has no such order" (found
// false, nil error) from a failure (error). An adapter that cannot tell them
// apart must not implement it; the reconcile then searches the open and closed
// listings for the ID.
type ClientIDLookup interface {
	FetchOrderByClientID(ctx context.Context, market, symbol, clientOrderID string) (order ccxt.Order, found bool, err error)
}

// PagedListings is implemented by an adapter whose open-orders and closed-orders
// listings are ONE page of a paged API (Webull: 10 rows by default, 100 at most,
// continued by a cursor). The reconcile searches those listings for an order it
// may have placed, and "not in the listing" proves nothing when the listing was
// cut. The adapter alone can say so, from the rows the exchange returned before
// any symbol or status filter: truncated is true when that raw page was full.
// A truncated listing makes "none" ambiguous, so the order is not placed again.
//
// An adapter that does not implement it keeps the generic rule: a closed-orders
// listing that returns reconcileListLimit rows or more is truncated.
type PagedListings interface {
	// FetchOpenOrdersPage is FetchOpenOrders plus the truncation flag.
	FetchOpenOrdersPage(ctx context.Context, symbol string) (orders []ccxt.Order, truncated bool, err error)
	// FetchClosedOrdersPage is FetchClosedOrders plus the truncation flag.
	FetchClosedOrdersPage(ctx context.Context, symbol string, since *int64, limit int) (orders []ccxt.Order, truncated bool, err error)
}

// clientIDKeys are the params names that carry a caller-chosen client order ID on
// any of the exchanges above. PlaceOrderIdempotent derives the ID itself and
// refuses a request that already sets one.
var clientIDKeys = []string{
	"clientOrderId", "newClientOrderId", "origClientOrderId", "clientAlgoId",
	"newClientStrategyId", "clOrdId", "algoClOrdId", "client_order_id",
}

// triggerKeys turn an order into a conditional one on the exchanges above, which
// changes the client-ID field and the endpoint a lookup must use.
var triggerKeys = []string{
	"stopPrice", "triggerPrice", "stopLossPrice", "takeProfitPrice", "trailingPercent",
	"trailingTriggerPrice", "trailingPrice", "callbackRatio", "callbackSpread",
	"stop", "trigger", "conditional", "stopLoss", "takeProfit", "tpTriggerPx", "slTriggerPx",
	"triggerPx", "oco",
}
