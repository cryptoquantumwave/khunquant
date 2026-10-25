package tools

import (
	"context"
	"math"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// orderReport is what a trading tool knows about an order the exchange has just
// accepted, in the plain values orderfeed.Publish takes.
type orderReport struct {
	tool                                  string // the tool that placed it (OrderEvent.Origin)
	provider, account, symbol, side, kind string
	amount                                float64 // requested size, in unit
	unit                                  string  // an orderfeed.Unit* constant

	order ccxt.Order // the exchange's answer (any field may be nil)

	// status and filled, when set, replace what order says: they come from a
	// verification fetch the tool made anyway.
	status      string
	filled      float64
	filledKnown bool

	// notionalUSD is the value the tool had already computed; it never triggers a
	// new fetch. notionalKnown is false when the tool did not have one.
	notionalUSD   float64
	notionalKnown bool

	paper bool // simulated: nothing reached an exchange
}

// publishOrder reports an accepted order to the perception order feed. It is called
// AFTER the exchange's answer, on the tool's own goroutine, and it must not change
// anything the tool returns or does: it reads values the tool already holds, never
// touches the network, the rate limiter or the stores, and recovers any panic of
// its own. Dry runs, refusals and failed orders never reach it.
func publishOrder(ctx context.Context, r orderReport) {
	defer func() { _ = recover() }()

	ev := orderfeed.OrderEvent{
		Provider:   r.provider,
		Account:    r.account,
		Symbol:     r.symbol,
		Side:       strings.ToLower(r.side),
		Type:       strings.ToLower(r.kind),
		Amount:     cleanOrderNumber(r.amount),
		AmountUnit: r.unit,
		Origin:     r.tool,
		TurnKind:   orderTurnKind(ctx),
		Paper:      r.paper,
		At:         time.Now(),
	}
	if r.order.Id != nil {
		ev.OrderID = *r.order.Id
	}
	if r.order.ClientOrderId != nil {
		ev.ClientOrderID = *r.order.ClientOrderId
	}
	ev.Status = r.status
	if ev.Status == "" && r.order.Status != nil {
		ev.Status = strings.ToLower(*r.order.Status)
	}
	switch {
	case r.filledKnown:
		ev.Filled, ev.FilledKnown = r.filled, validOrderNumber(r.filled)
		if !ev.FilledKnown {
			ev.Filled = 0
		}
	case r.order.Filled != nil && validOrderNumber(*r.order.Filled):
		ev.Filled, ev.FilledKnown = *r.order.Filled, true
	}
	if r.order.Average != nil && validOrderNumber(*r.order.Average) && *r.order.Average > 0 {
		ev.AvgPrice, ev.AvgPriceKnown = *r.order.Average, true
	}
	if r.notionalKnown && validOrderNumber(r.notionalUSD) {
		ev.NotionalUSD, ev.NotionalKnown = r.notionalUSD, true
	}
	orderfeed.Publish(ctx, ev)
}

// orderTurnKind is the kind of turn an order was placed in: the proactive turn's
// own kind, else by the origin of the turn.
func orderTurnKind(ctx context.Context) proactive.TurnKind {
	if k := ProactiveTurnKind(ctx); k != "" {
		return k
	}
	switch ToolOrigin(ctx) {
	case OriginCron:
		return proactive.TurnCronLegacy
	case OriginHeartbeat:
		return proactive.TurnHeartbeatLegacy
	}
	return proactive.TurnUser
}

func validOrderNumber(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }

func cleanOrderNumber(v float64) float64 {
	if validOrderNumber(v) {
		return v
	}
	return 0
}

// publishFuturesExit reports a futures close, reduce or flatten order. filled and
// status come from the fill verification the tool already ran; when that failed
// (verifyErr) they mean nothing, and only what the exchange answered is reported.
// An exit acts on the real exchange even in paper mode, so it is never marked paper.
// The notional is not known without a mark price the tool did not fetch.
func publishFuturesExit(ctx context.Context, tool, provider, account, symbol, side, kind string, amount float64, order ccxt.Order, filled float64, status string, verifyErr error) {
	r := orderReport{
		tool: tool, provider: provider, account: account, symbol: symbol, side: side, kind: kind,
		amount: amount, unit: orderfeed.UnitContracts, order: order,
	}
	if verifyErr == nil {
		if status == "unknown" { // verifyFuturesFill's word for "the exchange sent none"
			status = ""
		}
		r.status, r.filled, r.filledKnown = status, filled, true
	}
	publishOrder(ctx, r)
}

// usdQuoted reports whether a symbol is quoted in a USD-like currency, so that a cost
// the exchange reports for it is a USD amount. A delta-neutral plan does not restrict
// its spot symbol to such a quote (BTC/THB is accepted), and an order.Cost in THB is
// not USD; an unknown quote is not USD either.
func usdQuoted(symbol, providerID string) bool {
	return exchanges.USDLike(broker.SymbolQuote(symbol, providerID))
}
