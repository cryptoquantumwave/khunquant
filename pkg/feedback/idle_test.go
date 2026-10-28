package feedback

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// blockingPrices holds the first read until released.
type blockingPrices struct {
	*fakePrices
	entered chan struct{}
	gate    chan struct{}
	first   bool
}

func (b *blockingPrices) HourlyBars(ctx context.Context, provider, account, symbol string, n int) ([]Bar, error) {
	if !b.first {
		b.first = true
		close(b.entered)
		<-b.gate
	}
	return b.fakePrices.HourlyBars(ctx, provider, account, symbol, n)
}

func idleWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The subscriber is busy while an event is queued and while one is dequeued and still in
// the handler, idle once Received is covered by handled, failed, panicked and dropped
// events (a dropped event is received and never handled: it must not hold Idle false),
// and asking changes nothing.
func TestFeedback_IdleTracksSubscription(t *testing.T) {
	f := newFixture(t)
	if !f.fb.Idle() {
		t.Fatal("a service with no subscription is idle")
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	first := true
	sub, err := f.bus.Channel().OfKind(events.KindPerceptionOrderFilled).Subscribe(context.Background(),
		events.SubscribeOptions{Name: "feedback-test", Buffer: 1, Backpressure: events.DropNewest},
		func(context.Context, events.Event) error {
			if first {
				first = false
				close(entered)
				<-gate
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	f.fb.mu.Lock()
	f.fb.sub = sub
	f.fb.mu.Unlock()
	if !f.fb.Idle() {
		t.Fatal("idle subscription reads busy")
	}

	at := f.clock.Now()
	f.bus.PublishNonBlocking(fill("BTC/USDT", "o1", at))
	<-entered // dequeued, inside the handler
	before := sub.Stats()
	if f.fb.Idle() || f.fb.Idle() {
		t.Fatal("idle while an event is dequeued and unfinished")
	}
	// One fits the queue, the others are dropped: received, never handled.
	for i := range 5 {
		f.bus.PublishNonBlocking(fill("BTC/USDT", "x"+string(rune('a'+i)), at))
	}
	if sub.Stats().Dropped == 0 {
		t.Fatal("the scenario dropped nothing")
	}
	if f.fb.Idle() {
		t.Fatal("idle with an event queued")
	}
	if sub.Stats().Received < before.Received {
		t.Fatal("Idle changed the subscription")
	}
	close(gate)
	idleWait(t, "idle once the queue drained and the drops were counted", f.fb.Idle)
	st := sub.Stats()
	if st.Received != st.Handled+st.Failed+st.Panicked+st.Dropped {
		t.Fatalf("stats do not add up: %+v", st)
	}
}

// An Async recording in flight and a housekeeping pass each keep the service busy.
func TestFeedback_IdleCoversAsyncAndHousekeeping(t *testing.T) {
	f := newFixture(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	f.fb.Async(func(context.Context) { close(entered); <-gate })
	<-entered
	if f.fb.Idle() {
		t.Fatal("idle while a recording is in flight")
	}
	close(gate)
	idleWait(t, "idle after the recording", f.fb.Idle)

	f.fb.housekeeping.Add(1)
	if f.fb.Idle() {
		t.Fatal("idle during housekeeping")
	}
	f.fb.housekeeping.Add(-1)
	if _, err := f.fb.Housekeep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.fb.Idle() {
		t.Fatal("busy after housekeeping returned")
	}
	var nilSvc *Service
	if !nilSvc.Idle() || !nilSvc.HindsightIdle() {
		t.Fatal("a nil service is idle")
	}
}

// The hindsight job is busy while it reads prices, idle in the wait between two symbols
// (a timer on its clock) and when no job runs.
func TestHindsight_IdleSkipsPaceWait(t *testing.T) {
	f := newFixture(t)
	if !f.fb.HindsightIdle() {
		t.Fatal("no job running is idle")
	}
	f.deliver("SOL/USDT", "sol")
	f.deliver("ETH/USDT", "eth")
	d0 := f.clock.Now()
	f.fb.fillsSince = time.Time{}
	f.clock.Add(2 * time.Hour)
	prices := &blockingPrices{fakePrices: newPrices(), entered: make(chan struct{}), gate: make(chan struct{})}
	prices.bars["SOL/USDT"] = ramp(d0, 9)
	prices.bars["ETH/USDT"] = ramp(d0, 9)
	rc := &runClock{fakeClock: f.clock, timers: make(chan chan time.Time, 4)}
	h := f.fb.NewHindsight(HindsightOptions{Prices: prices, Clock: rc, Pace: time.Second})
	if !h.Idle() {
		t.Fatal("a job that is not running is idle")
	}

	done := make(chan struct{})
	go func() { defer close(done); _, _ = h.RunOnce(context.Background()) }()
	<-prices.entered
	if h.Idle() || h.Idle() {
		t.Fatal("idle while a price read is in flight")
	}
	close(prices.gate)
	var pace chan time.Time
	select {
	case pace = <-rc.timers: // the run reached the wait between the two symbols
	case <-time.After(5 * time.Second):
		t.Fatal("the run never waited between symbols")
	}
	if !h.Idle() {
		t.Fatal("busy in the pace wait: that is a timer")
	}
	pace <- time.Time{}
	<-done
	if !h.Idle() {
		t.Fatal("busy after the run returned")
	}
}
