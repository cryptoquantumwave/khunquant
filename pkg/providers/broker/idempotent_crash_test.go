package broker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The acceptance test of MR-304: a real journal engine on a temp database, a fake
// exchange that fills, and a process killed before the place call, during it
// (after the exchange accepted the order, before the response came back) and
// after it (before the step's output was journaled). Whatever the point, the
// workflow ends with exactly ONE order on the exchange, for a capable adapter
// and for one that is not.

const (
	crashLease = 300 * time.Millisecond
	crashGrace = 100 * time.Millisecond
	// settle is how long a retry waits before it believes an empty answer; the
	// step's Backoff is at least that long, as Retryable's doc requires.
	crashSettle  = 40 * time.Millisecond
	crashBackoff = 60 * time.Millisecond
)

type placed struct {
	ID string `json:"id"`
}

// proc is one "process": an engine on its own handle of the shared database.
type proc struct {
	db     *sql.DB
	eng    *journal.Engine
	cancel context.CancelFunc
	done   chan struct{}
}

func startProc(t *testing.T, path string, def journal.Definition) *proc {
	t.Helper()
	db, err := sqlitex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := journal.New(db, []journal.Definition{def}, nil,
		journal.WithLease(crashLease), journal.WithGrace(crashGrace), journal.WithTick(10*time.Millisecond),
		journal.WithWaitPoll(10*time.Millisecond), journal.WithShutdownWait(200*time.Millisecond), journal.WithWorkers(1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &proc{db: db, eng: eng, cancel: cancel, done: make(chan struct{})}
	go func() { defer close(p.done); _ = eng.Run(ctx) }()
	t.Cleanup(p.stop)
	return p
}

func (p *proc) stop() {
	p.cancel()
	<-p.done
	_ = p.db.Close()
}

// kill makes the process die as far as the database can tell: its handle closes,
// so nothing more it does is journaled and it renews no lease.
func (p *proc) kill() {
	p.cancel()
	_ = p.db.Close()
}

type adapterKind struct {
	name    string
	capable bool
	// blind: the exchange's listings never show the order we placed, as when an
	// adapter reports it with another type or price than we sent.
	blind bool
	prov  func(ex *exch) broker.Provider
}

var adapterKinds = []adapterKind{
	{"capable", true, false, func(ex *exch) broker.Provider { return newCap(ex) }},
	{"not capable", false, false, func(ex *exch) broker.Provider { return newPlain(ex) }},
	{"not capable, blind listings", false, true, func(ex *exch) broker.Provider { return newPlain(ex) }},
}

// expect is what a crash at point must end in. Capable: the order is found or
// placed once, the workflow succeeds. Not capable: a retry never places, so a
// crash before the request was sent ends in a failed workflow with NO order, and a
// crash after the exchange accepted it ends in one order, found by shape (the
// workflow succeeds) or, when the listings cannot show it, in a failed workflow
// that still holds that one order. Never two.
func (k adapterKind) expect(point string) (success bool, orders int) {
	switch {
	case k.capable:
		return true, 1
	case point == "before_place":
		return false, 0
	default:
		return !k.blind, 1
	}
}

// placeDef is a workflow of one step that places an order the way MR-305, 309 and
// 310 will: the journal's idempotency key and first-start time go in, and the
// step is declared with Retries, a Backoff and the Retryable predicate.
func placeDef(prov broker.Provider, req broker.OrderRequest, before func(attempt int), onAttempt func(attempt int)) journal.Definition {
	return journal.Definition{
		Kind: "place_order",
		Run: func(c *journal.Ctx, _ json.RawMessage) error {
			var out placed
			return c.Step("place", &out, func(ctx context.Context, attempt int) (any, error) {
				onAttempt(attempt)
				before(attempt)
				o, err := broker.PlaceOrderIdempotent(ctx, prov, req, c.IdempotencyKey("place"), attempt, c.StepStartedAt(),
					broker.WithReconcileSettleDelay(crashSettle))
				if err != nil {
					return nil, err
				}
				return placed{ID: *o.Id}, nil
			}, journal.Retries(6), journal.Backoff(crashBackoff),
				journal.NonRetryable(func(e error) bool { return !broker.Retryable(e) }))
		},
	}
}

func runCrash(t *testing.T, kind adapterKind, point string) {
	path := filepath.Join(t.TempDir(), "proactive.db")
	ex := newExch(&clock{t: time.Now()})
	ex.hideListings = kind.blind
	prov := kind.prov(ex)

	var a *proc
	var killed atomic.Bool
	var mu sync.Mutex
	attempts := map[string][]int{}
	record := func(who string) func(int) {
		return func(n int) { mu.Lock(); attempts[who] = append(attempts[who], n); mu.Unlock() }
	}
	killA := func() {
		if killed.CompareAndSwap(false, true) {
			a.kill()
		}
	}

	switch point {
	case "during_place":
		ex.afterAccept = func(n int, _ ccxt.Order) error {
			if n == 1 {
				killA()
				runtime.Goexit() // the process is gone: the response is never read
			}
			return errors.New("read tcp: connection reset by peer")
		}
	}
	before := func(attempt int) {
		if point == "before_place" && attempt == 1 {
			killA()
			runtime.Goexit() // the process is gone: nothing is sent
		}
	}

	defA := placeDef(prov, spotReq(0.5), before, record("A"))
	a = startProc(t, path, defA)
	id, err := a.eng.Start(context.Background(), "place_order", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the process to die.
	deadline := time.Now().Add(10 * time.Second)
	for !killed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !killed.Load() {
		t.Fatal("the crash point was never reached")
	}

	// A new process takes over after the lease and the grace period.
	b := startProc(t, path, placeDef(prov, spotReq(0.5), func(int) {}, record("B")))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w, err := b.eng.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	checkOutcome(t, kind, point, w, ex)
	mu.Lock()
	defer mu.Unlock()
	if len(attempts["B"]) == 0 || attempts["B"][0] < 2 && point != "before_place" {
		t.Errorf("the resumed step should see attempt > 1, saw %v", attempts["B"])
	}
}

func checkOutcome(t *testing.T, kind adapterKind, point string, w journal.Workflow, ex *exch) {
	t.Helper()
	wantOK, wantOrders := kind.expect(point)
	if got := len(ex.book()); got != wantOrders || ex.placed() != wantOrders {
		t.Fatalf("the exchange holds %d orders after %d place calls, want %d (workflow %s: %s)", got, ex.placed(), wantOrders, w.Status, w.Error)
	}
	if ok := w.Status == journal.StatusSucceeded; ok != wantOK {
		t.Fatalf("workflow %s (%s), want success=%v", w.Status, w.Error, wantOK)
	}
	if !wantOK && !strings.Contains(w.Error, "may or may not have placed") {
		t.Errorf("the failure should say the outcome is unproven: %s", w.Error)
	}
}

func TestCrashInjection_OneOrderPerWorkflow(t *testing.T) {
	for _, kind := range adapterKinds {
		for _, point := range []string{"before_place", "during_place"} {
			t.Run(kind.name+"/"+point, func(t *testing.T) { runCrash(t, kind, point) })
		}
		t.Run(kind.name+"/after_place", func(t *testing.T) { runCrashAfterPlace(t, kind) })
	}
}

// afterHookCap and afterHookPlain wrap an adapter so the process is killed right
// after the place call returned successfully (the order exists; the step's output
// is never journaled). Everything else is the wrapped adapter's.
type afterHookCap struct {
	capProv
	after func()
}

func (h afterHookCap) CreateOrder(ctx context.Context, s, ty, sd string, amt float64, pr *float64, p map[string]interface{}) (ccxt.Order, error) {
	o, err := h.capProv.CreateOrder(ctx, s, ty, sd, amt, pr, p)
	if err == nil {
		h.after()
	}
	return o, err
}

type afterHookPlain struct {
	*plainProv
	after func()
}

func (h afterHookPlain) CreateOrder(ctx context.Context, s, ty, sd string, amt float64, pr *float64, p map[string]interface{}) (ccxt.Order, error) {
	o, err := h.plainProv.CreateOrder(ctx, s, ty, sd, amt, pr, p)
	if err == nil {
		h.after()
	}
	return o, err
}

func runCrashAfterPlace(t *testing.T, kind adapterKind) {
	path := filepath.Join(t.TempDir(), "proactive.db")
	ex := newExch(&clock{t: time.Now()})
	ex.hideListings = kind.blind
	base := kind.prov(ex)

	var a *proc
	var killed atomic.Bool
	kill := func() {
		if killed.CompareAndSwap(false, true) {
			a.kill()
		}
	}
	var provA broker.Provider
	switch b := base.(type) {
	case capProv:
		provA = afterHookCap{b, kill}
	case *plainProv:
		provA = afterHookPlain{b, kill}
	}

	a = startProc(t, path, placeDef(provA, spotReq(0.5), func(int) {}, func(int) {}))
	id, err := a.eng.Start(context.Background(), "place_order", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !killed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !killed.Load() {
		t.Fatal("the crash point was never reached")
	}
	if len(ex.book()) != 1 {
		t.Fatalf("setup: %d orders", len(ex.book()))
	}

	b := startProc(t, path, placeDef(base, spotReq(0.5), func(int) {}, func(int) {}))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w, err := b.eng.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	checkOutcome(t, kind, "after_place", w, ex)
	if w.Status != journal.StatusSucceeded {
		return
	}
	steps, err := b.eng.Steps(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var out placed
	if len(steps) != 1 || json.Unmarshal(steps[0].Output, &out) != nil || out.ID != *ex.book()[0].Id {
		t.Errorf("the journaled output should name the one order: %+v", steps)
	}
}

// An unknown outcome without any crash: the journal retries the step, the retry
// reconciles, and there is one order. A definite rejection is not retried.
func TestJournalRetry_UnknownOutcomeIsReconciledNotRecordedAsRejection(t *testing.T) {
	for _, kind := range adapterKinds {
		if kind.blind {
			continue // covered by the crash test: it fails as unproven
		}
		t.Run(kind.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proactive.db")
			ex := newExch(&clock{t: time.Now()})
			ex.afterAccept = func(n int, _ ccxt.Order) error {
				if n == 1 {
					return ccxtErr("RequestTimeout", "binance POST https://x/api?signature=abc "+ex.secret)
				}
				return nil
			}
			var seen []int
			var mu sync.Mutex
			p := startProc(t, path, placeDef(kind.prov(ex), spotReq(0.5), func(int) {}, func(n int) { mu.Lock(); seen = append(seen, n); mu.Unlock() }))
			id, err := p.eng.Start(context.Background(), "place_order", nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			w, err := p.eng.Wait(ctx, id)
			if err != nil || w.Status != journal.StatusSucceeded {
				t.Fatalf("%v %+v", err, w)
			}
			if len(ex.book()) != 1 {
				t.Fatalf("%d orders", len(ex.book()))
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) < 2 || seen[0] != 1 || seen[1] != 2 {
				t.Errorf("attempts = %v, want 1 then 2", seen)
			}
		})
	}
	t.Run("a definite rejection fails the workflow at once", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "proactive.db")
		ex := newExch(&clock{t: time.Now()})
		ex.createErr = func(int) error { return ccxtErr("InsufficientFunds", "binance {\"code\":-2010}") }
		p := startProc(t, path, placeDef(newCap(ex), spotReq(0.5), func(int) {}, func(int) {}))
		id, _ := p.eng.Start(context.Background(), "place_order", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		w, err := p.eng.Wait(ctx, id)
		if err != nil || w.Status != journal.StatusFailed {
			t.Fatalf("%v %+v", err, w)
		}
		if ex.placed() != 1 || ex.total("lookup", "open", "closed") != 0 {
			t.Errorf("a rejection was retried or reconciled: place %d", ex.placed())
		}
	})
}
