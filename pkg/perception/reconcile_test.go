package perception_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
)

func reconcileManager(srcs ...*fakeSource) *perception.Manager {
	m := perception.NewManager(&nullEmitter{})
	for _, s := range srcs {
		m.Add(s)
	}
	return m
}

// Every source is reconciled; one that fails or panics is named and the others
// still report ok.
func TestReconcileReport_PartialFailure(t *testing.T) {
	ok := &fakeSource{name: "a-ok"}
	bad := &fakeSource{name: "b-bad", reconcile: func(context.Context, perception.Emitter) error {
		return errors.New("portfolio: partial snapshot,\naccounts failed: [okx/main]")
	}}
	boom := &fakeSource{name: "c-boom", reconcile: func(context.Context, perception.Emitter) error { panic("kaboom") }}
	fine := &fakeSource{name: "d-fine"}
	m := reconcileManager(fine, boom, bad, ok)

	rep := m.ReconcileReport(context.Background())
	if got := strings.Join(rep.OK, ","); got != "a-ok,d-fine" {
		t.Fatalf("OK = %q", got)
	}
	if len(rep.Failed) != 2 || rep.Failed[0].Source != "b-bad" || rep.Failed[1].Source != "c-boom" {
		t.Fatalf("Failed = %+v", rep.Failed)
	}
	if strings.Contains(rep.Failed[0].Err, "\n") || !strings.Contains(rep.Failed[0].Err, "okx/main") {
		t.Fatalf("failure text = %q (want one line, with the account)", rep.Failed[0].Err)
	}
	if len(rep.Unfinished) != 0 || len(rep.Busy) != 0 || rep.Complete() {
		t.Fatalf("report = %+v", rep)
	}
	if st := m.Stats(); st.Errors != 2 || st.Panics != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if h := m.Health()["b-bad"]; h.ConsecutiveErrors == 0 {
		t.Fatalf("the failure was not noted on its source: %+v", h)
	}
}

// A source that waits on a lock of its own and never looks at its context cannot
// hold the caller past the budget: it is named Unfinished, the others still
// report, and a second call does not start it again while it is stuck.
func TestReconcileReport_DeadlineHoldsAgainstASourceThatIgnoresItsContext(t *testing.T) {
	var lock sync.Mutex
	lock.Lock() // held by "the poll in progress"
	stuck := &fakeSource{name: "market", reconcile: func(context.Context, perception.Emitter) error {
		lock.Lock() // not interruptible by ctx
		defer lock.Unlock()
		return nil
	}}
	quick := &fakeSource{name: "clock"}
	m := reconcileManager(stuck, quick)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	began := time.Now()
	rep := m.ReconcileReport(ctx)
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("the call took %v against a 40ms budget", d)
	}
	if strings.Join(rep.OK, ",") != "clock" || strings.Join(rep.Unfinished, ",") != "market" || rep.Complete() {
		t.Fatalf("report = %+v", rep)
	}

	// Still stuck: not started again, and said so.
	rep = m.ReconcileReport(context.Background())
	if strings.Join(rep.Busy, ",") != "market" || strings.Join(rep.OK, ",") != "clock" {
		t.Fatalf("second report = %+v", rep)
	}
	if n := stuck.reconciles.Load(); n != 1 {
		t.Fatalf("the stuck source was started %d times", n)
	}

	// Once it returns it is reconciled again.
	lock.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rep = m.ReconcileReport(context.Background())
		if len(rep.Busy) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the source stayed busy after it returned: %+v", rep)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !rep.Complete() {
		t.Fatalf("report = %+v", rep)
	}
}

// The sources run side by side: a slow one does not use up the time of the next.
func TestReconcileReport_SourcesRunInParallel(t *testing.T) {
	release := make(chan struct{})
	var started sync.WaitGroup
	started.Add(2)
	mk := func(name string) *fakeSource {
		return &fakeSource{name: name, reconcile: func(ctx context.Context, _ perception.Emitter) error {
			started.Done()
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}}
	}
	m := reconcileManager(mk("one"), mk("two"))
	done := make(chan perception.ReconcileReport, 1)
	go func() { done <- m.ReconcileReport(context.Background()) }()
	started.Wait() // both are inside Reconcile at once, or this never returns
	close(release)
	select {
	case rep := <-done:
		if !rep.Complete() || len(rep.OK) != 2 {
			t.Fatalf("report = %+v", rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReconcileReport did not return")
	}
}

// With no source, there is nothing to report and nothing to wait for.
func TestReconcileReport_NoSources(t *testing.T) {
	rep := reconcileManager().ReconcileReport(context.Background())
	if !rep.Complete() || len(rep.OK) != 0 {
		t.Fatalf("report = %+v", rep)
	}
}
