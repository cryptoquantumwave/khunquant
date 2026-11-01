package policy

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

type blockingValuer struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (b *blockingValuer) NotionalUSD(context.Context, ActionRequest) (float64, bool) {
	b.once.Do(func() { close(b.entered) })
	<-b.release // ignores the context on purpose: a valuer that hangs
	return 1, true
}

// Shadow never slows a tool call: a valuer that blocks does not delay Observe, and
// the verdict is recorded once it returns.
func TestRecorder_BlockingValuerDoesNotDelayTheCall(t *testing.T) {
	bv := &blockingValuer{release: make(chan struct{}), entered: make(chan struct{})}
	r := newRig(t, func(_ *rig, d *Deps) { d.Valuer = bv })
	r.cfg.TradingRisk.MaxOrderValueUSD = 100
	store := newStore(t)
	rc := NewRecorder(r.e, store, nil, 0)
	t.Cleanup(func() { rc.Close(time.Second) })

	req := owner(ClassTrade, "create_order")
	req.Order = &Order{Symbol: "BTC/USDT", Side: "buy", Amount: 1, Placing: true} // value unknown: the valuer is asked

	start := time.Now()
	for i := 0; i < 20; i++ {
		rc.Observe(context.Background(), req, true)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("20 observations took %s while the valuer blocked, want in-memory cost only", took)
	}
	select {
	case <-bv.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the valuer was never asked: the test does not exercise the slow path")
	}
	if n, _ := store.Count(context.Background()); n != 0 {
		t.Fatalf("%d rows recorded while the valuer is still blocked", n)
	}

	close(bv.release)
	if !rc.Flush(5 * time.Second) {
		t.Fatal("the queue did not drain after the valuer returned")
	}
	if n, _ := store.Count(context.Background()); n+int(rc.Dropped()) != 20 {
		t.Fatalf("%d rows + %d dropped, want 20 observations accounted for", n, rc.Dropped())
	}
}

type gateAuthorizer struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (g *gateAuthorizer) Authorize(context.Context, ActionRequest) Verdict {
	<-g.release
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return Verdict{Mode: ModeDeny}
}
func (g *gateAuthorizer) Note(ActionRequest) {}

// A full queue drops and counts; it never blocks the caller.
func TestRecorder_FullQueueDropsAndCounts(t *testing.T) {
	ga := &gateAuthorizer{release: make(chan struct{})}
	rc := NewRecorder(ga, nil, nil, 2)
	t.Cleanup(func() { rc.Close(time.Second) })

	req := ActionRequest{Tool: "get_ticker", Class: ClassRead, Trust: proactive.TrustSystem}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			rc.Observe(context.Background(), req, true)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Observe blocked on a full queue")
	}
	// One record in flight, two queued; the rest were dropped (the worker may have
	// picked up the first before or after the loop, so allow one either way).
	if d := rc.Dropped(); d < 46 || d > 48 {
		t.Fatalf("dropped = %d, want about 47 of 50", d)
	}

	close(ga.release)
	if !rc.Flush(2 * time.Second) {
		t.Fatal("queue did not drain")
	}
	ga.mu.Lock()
	defer ga.mu.Unlock()
	if int64(ga.calls)+rc.Dropped() != 50 {
		t.Fatalf("%d processed + %d dropped, want 50", ga.calls, rc.Dropped())
	}
}

// A record the store could not write is counted apart from a full queue, and the log
// keeps what the row would have said.
func TestRecorder_CountsAWriteTheStoreFailed(t *testing.T) {
	var buf bytes.Buffer
	prev := logger.GetLevel()
	logger.SetLevel(logger.INFO)
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil); logger.SetLevel(prev) })

	store := newStore(t)
	store.writeBudget = 0
	rc := NewRecorder(&spyAuthorizer{}, store, nil, 0)
	t.Cleanup(func() { rc.Close(time.Second) })

	rc.Record(context.Background(), ActionRequest{Tool: "create_order"}, Verdict{Mode: ModeDeny, Tier: T3Discretionary})
	if !rc.Flush(5 * time.Second) {
		t.Fatal("the queue did not drain")
	}
	if rc.StoreFailed() != 1 || rc.Dropped() != 0 {
		t.Fatalf("failed = %d, dropped = %d, want 1 and 0", rc.StoreFailed(), rc.Dropped())
	}
	if n, _ := store.Count(context.Background()); n != 0 {
		t.Fatalf("%d rows", n)
	}
	if out := buf.String(); !strings.Contains(out, "create_order") || !strings.Contains(out, "deny") || !strings.Contains(out, "T3") {
		t.Fatalf("the log lost the record: %q", out)
	}
}

// Close drains what is queued within its bound, and a closed recorder drops
// further records without blocking or panicking.
func TestRecorder_CloseDrainsWithinABound(t *testing.T) {
	spy := &spyAuthorizer{v: Verdict{Mode: ModeAct}}
	rc := NewRecorder(spy, nil, nil, 64)
	for i := 0; i < 10; i++ {
		rc.Observe(context.Background(), ActionRequest{Tool: "t"}, true)
	}
	rc.Close(2 * time.Second)
	if spy.calls != 10 {
		t.Fatalf("%d of 10 queued records were processed before Close returned", spy.calls)
	}
	rc.Observe(context.Background(), ActionRequest{Tool: "t"}, true) // after Close: dropped quietly
	rc.Close(time.Second)                                            // and Close twice is fine

	// A worker stuck in a call does not hold Close past its bound.
	ga := &gateAuthorizer{release: make(chan struct{})}
	stuck := NewRecorder(ga, nil, nil, 4)
	stuck.Observe(context.Background(), ActionRequest{Tool: "t"}, true)
	start := time.Now()
	stuck.Close(150 * time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Close took %s with a stuck worker", took)
	}
	close(ga.release)
}
