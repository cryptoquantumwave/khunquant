// Package orderfeed carries "an order was placed" from the trading tools to the
// perception pipeline.
//
// A trading tool calls Publish after the exchange has accepted an order. Publish
// runs inside a user's turn, so it is built to be impossible to get wrong at the call
// site: it returns nothing, it is a no-op when nobody listens, it never blocks (the
// event goes into a bounded queue, and a full queue drops the event and counts it),
// it recovers its own panics, and it takes plain values only. The publisher itself
// runs on a goroutine of this package, never on the caller's, so a publisher that
// blocks or panics cannot delay or break an order.
//
// With no publisher set (the default) nothing runs: no goroutine and no queue.
//
// Dependency rule: this package imports pkg/proactive and the standard library,
// nothing else from this module. pkg/tools imports it, so it must stay a leaf.
package orderfeed

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Units of OrderEvent.Amount. An amount without its unit is how a futures order
// gets valued wrong by the contract size.
const (
	// UnitBase is a quantity of the base asset (BTC, a share).
	UnitBase = "base"
	// UnitContracts is a futures contract count (OKX BTC/USDT:USDT: 0.01 BTC each).
	UnitContracts = "contracts"
	// UnitOptionContracts is a count of option contracts.
	UnitOptionContracts = "option_contracts"
)

// QueueSize is the number of events that wait for the publisher. A burst beyond it
// is dropped (counted), never waited for.
const QueueSize = 256

// OrderEvent is one order as the tool that placed it saw it. Every field is a plain
// value. A number the exchange did not report is zero with its Known flag false;
// it is never a guess.
type OrderEvent struct {
	Provider, Account, Symbol, Side, Type string
	OrderID, ClientOrderID                string

	// Amount is the requested size, in AmountUnit.
	Amount     float64
	AmountUnit string

	// Filled is the filled size in AmountUnit and AvgPrice the average fill price in
	// the symbol's quote currency, as far as the response said.
	Filled, AvgPrice           float64
	FilledKnown, AvgPriceKnown bool

	// NotionalUSD is the order's value in USD as the tool had already computed it.
	NotionalUSD   float64
	NotionalKnown bool

	// Status is the ccxt status ("open", "closed", ...), or "" when not reported.
	Status string

	// Origin is the tool that placed the order, or "journal:<workflow>".
	Origin string
	// TurnKind is the kind of turn the tool ran in.
	TurnKind proactive.TurnKind

	// Paper is true for an order that was only simulated (paper_trading_mode): it
	// never reached an exchange and nothing may treat it as a fill.
	Paper bool

	At time.Time
}

// Stats are the package's counters since the process started. They add up: every
// call of Publish that found a publisher is either refused at once (queue full:
// Dropped), or taken (Published) and then either handed to the publisher (Delivered)
// or thrown away when the publisher was replaced or cleared (Dropped). So, once
// the queue is quiet, attempts = Delivered + Dropped, less the events the pipeline's
// own inbox refused, which the gateway adds to Dropped as well (NoteDropped) after
// they were Delivered.
type Stats struct {
	Published uint64 // events taken into the queue
	Delivered uint64 // events the publisher was called with (a panic counts)
	Dropped   uint64 // events lost: queue full, discarded on replace/clear, or refused by the pipeline's inbox
	Panics    uint64 // panics recovered in Publish or in the publisher
}

var (
	cur       atomic.Pointer[feed]
	published atomic.Uint64
	delivered atomic.Uint64
	dropped   atomic.Uint64
	panics    atomic.Uint64

	clock atomic.Pointer[func() time.Time] // SetClock; nil is time.Now
)

// SetClock makes Publish stamp events that carry no time from now. It is process-wide,
// set once by the gateway at start-up; a nil now restores time.Now.
func SetClock(now func() time.Time) {
	if now == nil {
		clock.Store(nil)
		return
	}
	clock.Store(&now)
}

// feed is one publisher with its queue and the goroutine that drains it.
type feed struct {
	fn   func(OrderEvent)
	ch   chan OrderEvent
	stop chan struct{}
	once sync.Once
}

// close stops the feed and counts as dropped every event still waiting in it.
func (f *feed) close() {
	if f == nil {
		return
	}
	f.once.Do(func() { close(f.stop) })
	f.drain()
}

// drain throws away what waits in the queue, counting each event as dropped. It is
// safe beside the worker: an event is taken by exactly one of them.
func (f *feed) drain() {
	for {
		select {
		case <-f.ch:
			dropped.Add(1)
		default:
			return
		}
	}
}

func (f *feed) stopped() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

func (f *feed) run() {
	for {
		select {
		case <-f.stop:
			return
		case ev := <-f.ch:
			if f.stopped() { // taken after the stop: not delivered
				dropped.Add(1)
				return
			}
			f.call(ev)
		}
	}
}

func (f *feed) call(ev OrderEvent) {
	delivered.Add(1)
	defer func() {
		if recover() != nil {
			panics.Add(1)
		}
	}()
	f.fn(ev)
}

// SetPublisher sets who receives the events, replacing (and stopping) the previous
// publisher; nil removes it. A non-nil publisher gets a goroutine of its own, which
// the returned function (or the next SetPublisher) ends.
//
// The returned function clears the publisher only if p is still the current one, so
// a generation of services that is being stopped cannot remove the publisher of the
// generation that replaced it. It is safe to call more than once.
//
// The publisher is called one event at a time, in order, and must not assume it is
// on the caller's goroutine: it should return quickly (the gateway's hands the event
// to the pipeline without waiting), but if it does not, the queue fills and events
// are dropped. A publisher that blocks forever holds its goroutine until it returns.
func SetPublisher(p func(OrderEvent)) (clear func()) {
	if p == nil {
		cur.Swap(nil).close()
		return func() {}
	}
	f := &feed{fn: p, ch: make(chan OrderEvent, QueueSize), stop: make(chan struct{})}
	cur.Swap(f).close()
	go f.run()
	return func() {
		if cur.CompareAndSwap(f, nil) {
			f.close()
		}
	}
}

// Publish hands ev to the publisher without waiting for it. It does nothing when no
// publisher is set, drops (and counts) the event when the queue is full, and
// recovers any panic. It returns nothing and must not change what its caller does.
// ctx is accepted for symmetry with the tools and is deliberately not consulted: an
// order that has been placed is reported even if the turn that placed it was
// cancelled.
func Publish(_ context.Context, ev OrderEvent) {
	defer func() {
		if recover() != nil {
			panics.Add(1)
		}
	}()
	f := cur.Load()
	if f == nil {
		return
	}
	if ev.At.IsZero() {
		if f := clock.Load(); f != nil {
			ev.At = (*f)()
		} else {
			//simclock:wall the default when no clock is injected
			ev.At = time.Now()
		}
	}
	select {
	case f.ch <- ev:
		published.Add(1)
		if f.stopped() { // the feed was stopped while this event was being queued
			f.drain()
		}
	default:
		dropped.Add(1)
	}
}

// NoteDropped counts an event that the publisher could not hand on (the pipeline's
// inbox was full). The gateway's publisher calls it.
func NoteDropped() { dropped.Add(1) }

// ReadStats returns the counters.
func ReadStats() Stats {
	return Stats{Published: published.Load(), Delivered: delivered.Load(), Dropped: dropped.Load(), Panics: panics.Load()}
}
