package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// The write-ahead mark (journal.Effect) through a real approval.order workflow on a real
// journal engine and the real ApprovalOrderBackend, against a venue that takes a client order
// ID and can lose the answer and hide the order from its lookup (a lag longer than anything
// the broker could wait out).

// lagVenue is a priced, client-ID-capable spot venue. A create books the order; with lost set
// the answer is a timeout. hidden makes the by-ID lookup and the listings show nothing.
type lagVenue struct {
	*pricedVenue
	lmu     sync.Mutex
	lost    bool
	hidden  bool
	creates int
	book    []ccxt.Order
}

func (v *lagVenue) ClientOrderIDKey(string) (string, int, bool) { return "clientOrderId", 32, true }

func (v *lagVenue) CreateOrder(_ context.Context, symbol, typ, side string, amount float64, _ *float64, params map[string]interface{}) (ccxt.Order, error) {
	v.lmu.Lock()
	defer v.lmu.Unlock()
	v.creates++
	id := fmt.Sprintf("lag-%d", v.creates)
	cid, _ := params["clientOrderId"].(string)
	status, filled := "closed", amount
	o := ccxt.Order{Id: &id, ClientOrderId: &cid, Symbol: &symbol, Side: &side, Type: &typ, Amount: &amount, Filled: &filled, Status: &status}
	v.book = append(v.book, o)
	if v.lost {
		return ccxt.Order{}, &ccxt.Error{Type: "RequestTimeout", Message: "read tcp: i/o timeout"}
	}
	return o, nil
}

func (v *lagVenue) FetchOrderByClientID(_ context.Context, _, _, cid string) (ccxt.Order, bool, error) {
	v.lmu.Lock()
	defer v.lmu.Unlock()
	if v.hidden {
		return ccxt.Order{}, false, nil
	}
	for _, o := range v.book {
		if o.ClientOrderId != nil && *o.ClientOrderId == cid {
			return o, true, nil
		}
	}
	return ccxt.Order{}, false, nil
}

func (v *lagVenue) FetchOrder(_ context.Context, id, _ string) (ccxt.Order, error) {
	v.lmu.Lock()
	defer v.lmu.Unlock()
	for _, o := range v.book {
		if o.Id != nil && *o.Id == id {
			return o, nil
		}
	}
	return ccxt.Order{}, errors.New("no such order")
}

func (v *lagVenue) FetchClosedOrders(context.Context, string, *int64, int) ([]ccxt.Order, error) {
	v.lmu.Lock()
	defer v.lmu.Unlock()
	if v.hidden {
		return nil, nil
	}
	return append([]ccxt.Order(nil), v.book...), nil
}

func (v *lagVenue) count() int {
	v.lmu.Lock()
	defer v.lmu.Unlock()
	return v.creates
}

// premarkWorld is an approved buy waiting for its workflow to run.
func premarkWorld(t *testing.T, id string) (*e2e, *lagVenue, approval.Proposal) {
	t.Helper()
	e := newE2E(t, id, nil)
	lv := &lagVenue{pricedVenue: e.venue}
	broker.RegisterFactory(id, func(*config.Config) (broker.Provider, error) { return lv, nil })
	broker.ResetInstanceCache()
	t.Cleanup(broker.ResetInstanceCache)
	if res := e.propose(ownerTurn(), venueArgs(id, "buy", 0.01)); res.IsError {
		t.Fatal(res.ForLLM)
	}
	p, err := e.tg.tap(t, e.svc, "approve")
	if err != nil {
		t.Fatal(err)
	}
	return e, lv, p
}

func (e *e2e) status(t *testing.T, p approval.Proposal) approval.Status {
	t.Helper()
	got, err := e.svc.Get(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Status
}

// resume is a new process on the same database, after the dead one's lease and grace.
func (e *e2e) resume(t *testing.T, p approval.Proposal) {
	t.Helper()
	e.clock.Advance(time.Minute)
	eng, err := journal.New(e.db, e.svc.Definitions(), e.clock)
	if err != nil {
		t.Fatal(err)
	}
	eng.ExecuteNow(context.Background(), approval.WorkflowID(p.ID))
}

// An in-process retry after a lost answer, on a venue whose lookup lags past the retry: the
// step marked before it sent, so the empty lookup is not believed. One order, nothing placed
// again, the proposal not executed, and the owner is told it may exist.
func TestPremark_RetryAfterLostAnswer_LaggingVenue_IsUnknownAndOwnerIsTold(t *testing.T) {
	e, lv, p := premarkWorld(t, "premark-lag")
	lv.lost, lv.hidden = true, true
	e.run(p) // attempt 1: the answer is lost; the step retries after its backoff
	e.clock.Advance(time.Minute)
	e.run(p) // attempt 2: the lookup shows nothing
	if n := lv.count(); n != 1 {
		t.Fatalf("%d orders, want 1: the retry placed again", n)
	}
	if s := e.status(t, p); s != approval.StatusFailed {
		t.Fatalf("status %s, want failed (outcome unknown, nothing executed)", s)
	}
	if body := e.tg.last().Body; !strings.Contains(body, "MAY OR MAY NOT HAVE BEEN PLACED") {
		t.Errorf("the owner must be told the order may or may not have been placed: %q", body)
	}
}

// The same retry on a normal venue: the lookup finds the order, which is used. One order.
func TestPremark_RetryAfterLostAnswer_NormalVenue_FindsTheOrder(t *testing.T) {
	e, lv, p := premarkWorld(t, "premark-normal")
	lv.lost = true
	e.run(p)
	e.clock.Advance(time.Minute)
	e.run(p)
	if n := lv.count(); n != 1 {
		t.Fatalf("%d orders, want 1", n)
	}
	if s := e.status(t, p); s != approval.StatusExecuted {
		t.Fatalf("status %s, want executed", s)
	}
}

// A crash after the step began and before it reached the mark: nothing was sent. On a lagging
// venue (there is nothing for the lookup to hide) the resumed attempt places the order, once.
func TestPremark_CrashBeforeTheSend_PlacesOnceEvenOnALaggingVenue(t *testing.T) {
	e, lv, p := premarkWorld(t, "premark-before")
	lv.hidden = true
	k := e.eng.SimulateKill(func(point, step string, _ int) bool { return point == journal.CrashAfterStarted && step == "place" })
	e.run(p)
	if !k.Dead() || lv.count() != 0 {
		t.Fatalf("dead %v, orders %d: the crash point must be before the send", k.Dead(), lv.count())
	}
	e.resume(t, p)
	if n := lv.count(); n != 1 {
		t.Fatalf("%d orders, want exactly 1", n)
	}
	if s := e.status(t, p); s != approval.StatusExecuted {
		t.Fatalf("status %s, want executed", s)
	}
}

// A crash after the send, on a venue whose lookup hides the order: the resumed attempt knows
// the step marked, does not believe the empty lookup, and places nothing.
func TestPremark_CrashAfterTheSend_LaggingVenue_PlacesNothing(t *testing.T) {
	e, lv, p := premarkWorld(t, "premark-after")
	lv.hidden = true
	k := e.eng.SimulateKill(func(point, step string, _ int) bool { return point == journal.CrashAfterFn && step == "place" })
	e.run(p)
	if !k.Dead() || lv.count() != 1 {
		t.Fatalf("dead %v, orders %d: the crash point must be after the send", k.Dead(), lv.count())
	}
	e.resume(t, p)
	if n := lv.count(); n != 1 {
		t.Fatalf("%d orders, want 1: the resumed step placed again", n)
	}
	if s := e.status(t, p); s == approval.StatusExecuted {
		t.Fatalf("status %s: an order nobody could find must not be reported executed", s)
	}
}
