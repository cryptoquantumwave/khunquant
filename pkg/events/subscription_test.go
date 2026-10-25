package events

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscribeOnceClosesAfterFirstEvent(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	var handled atomic.Uint64
	sub, err := bus.Channel().SubscribeOnce(
		context.Background(),
		SubscribeOptions{Name: "once", Buffer: 2},
		func(context.Context, Event) error {
			handled.Add(1)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("SubscribeOnce failed: %v", err)
	}

	bus.Publish(context.Background(), Event{Kind: KindAgentTurnStart})
	waitForSubscriptionDone(t, sub)
	bus.Publish(context.Background(), Event{Kind: KindAgentTurnEnd})

	if got := handled.Load(); got != 1 {
		t.Fatalf("handled = %d, want 1", got)
	}
	if got := sub.Stats().Handled; got != 1 {
		t.Fatalf("subscription handled = %d, want 1", got)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	sub, ch, err := bus.Channel().SubscribeChan(context.Background(), SubscribeOptions{Name: "chan"})
	if err != nil {
		t.Fatalf("SubscribeChan failed: %v", err)
	}
	if err := sub.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel is open, want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close")
	}
	waitForSubscriptionDone(t, sub)
}

func TestBlockBackpressureCloseUnblocksPublisher(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	sub, _, err := bus.Channel().SubscribeChan(context.Background(), SubscribeOptions{
		Name:         "block-close",
		Buffer:       1,
		Backpressure: Block,
	})
	if err != nil {
		t.Fatalf("SubscribeChan failed: %v", err)
	}

	first := bus.Publish(context.Background(), Event{Kind: Kind("test.first")})
	if first.Delivered != 1 {
		t.Fatalf("first Publish = %+v, want one delivered event", first)
	}

	publishStarted := make(chan struct{})
	publishReturned := make(chan PublishResult, 1)
	go func() {
		close(publishStarted)
		publishReturned <- bus.Publish(context.Background(), Event{Kind: Kind("test.second")})
	}()

	<-publishStarted
	waitForStat(t, func() uint64 {
		return sub.Stats().Received
	}, 2)
	select {
	case result := <-publishReturned:
		t.Fatalf("blocking Publish returned before close: %+v", result)
	default:
	}

	closeReturned := make(chan error, 1)
	go func() {
		closeReturned <- sub.Close()
	}()

	select {
	case err := <-closeReturned:
		if err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Close to unblock")
	}

	select {
	case <-publishReturned:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocking Publish to return after close")
	}
	waitForSubscriptionDone(t, sub)
}

func TestHandlerPanicRecovered(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	sub, err := bus.Channel().Subscribe(
		context.Background(),
		SubscribeOptions{Name: "panic", Buffer: 1},
		func(context.Context, Event) error {
			panic("boom")
		},
	)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	bus.Publish(context.Background(), Event{Kind: KindAgentError})
	waitForStat(t, func() uint64 {
		return sub.Stats().Panicked
	}, 1)
}

// TestConcurrentHandlerPanicRecovered exercises the Concurrent-dispatch
// goroutine (subscription.go dispatch, Concurrent case) under the default
// RecoverAndLog policy: the process must survive and Panicked must reach 1.
func TestConcurrentHandlerPanicRecovered(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	sub, err := bus.Channel().Subscribe(
		context.Background(),
		SubscribeOptions{Name: "concurrent-panic", Buffer: 1, Concurrency: Concurrent, PanicPolicy: RecoverAndLog},
		func(context.Context, Event) error {
			panic("boom-concurrent")
		},
	)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	bus.Publish(context.Background(), Event{Kind: KindAgentError})
	waitForStat(t, func() uint64 {
		return sub.Stats().Panicked
	}, 1)
}

// TestTimeoutHandlerPanicRecovered exercises the Timeout>0 handler goroutine
// (subscription.go handle, Timeout>0 branch) under the default RecoverAndLog
// policy: the process must survive and Panicked must reach 1.
func TestTimeoutHandlerPanicRecovered(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	sub, err := bus.Channel().Subscribe(
		context.Background(),
		SubscribeOptions{Name: "timeout-panic", Buffer: 1, Timeout: 50 * time.Millisecond, PanicPolicy: RecoverAndLog},
		func(context.Context, Event) error {
			panic("boom-timeout")
		},
	)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	bus.Publish(context.Background(), Event{Kind: KindAgentError})
	waitForStat(t, func() uint64 {
		return sub.Stats().Panicked
	}, 1)
}

// TestConcurrentHandlerPanicCrashesProcess verifies that PanicPolicy: Crash
// is honored by the Concurrent-dispatch goroutine (subscription.go dispatch,
// Concurrent case): a handler panic must propagate and crash the process,
// not be silently swallowed and logged. A panic in a background goroutine
// can't be safely recovered from the test's own goroutine (recover only
// works within the panicking goroutine's call stack), so this runs the
// panicking subscription in a subprocess (the standard Go "crasher" test
// pattern) and asserts the subprocess actually crashed.
func TestConcurrentHandlerPanicCrashesProcess(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		bus := NewBus()
		_, err := bus.Channel().Subscribe(
			context.Background(),
			SubscribeOptions{Name: "concurrent-crash", Buffer: 1, Concurrency: Concurrent, PanicPolicy: Crash},
			func(context.Context, Event) error {
				panic("boom-concurrent-crash")
			},
		)
		if err != nil {
			fmt.Printf("setup failed: %v", err)
			os.Exit(0)
		}

		bus.Publish(context.Background(), Event{Kind: KindAgentError})
		// Give the background dispatch goroutine time to panic and crash the
		// process. If we reach the end of this sleep, the panic did NOT
		// propagate — exit cleanly so the parent test can tell the difference.
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestConcurrentHandlerPanicCrashesProcess")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the subprocess to crash under PanicPolicy: Crash, but it exited cleanly; output:\n%s", out)
	}
	if !strings.Contains(string(out), "panic:") {
		t.Fatalf("expected a panic trace in the crashed subprocess output, got:\n%s", out)
	}
	if !strings.Contains(string(out), "boom-concurrent-crash") {
		t.Fatalf("expected the panic message in the crashed subprocess output, got:\n%s", out)
	}
}

// TestTimeoutHandlerPanicCrashesProcess is the Timeout>0 counterpart of
// TestConcurrentHandlerPanicCrashesProcess: it verifies PanicPolicy: Crash
// is honored by the Timeout>0 handler goroutine (subscription.go handle,
// Timeout>0 branch) via the same subprocess "crasher" pattern.
func TestTimeoutHandlerPanicCrashesProcess(t *testing.T) {
	if os.Getenv("BE_CRASHER") == "1" {
		bus := NewBus()
		_, err := bus.Channel().Subscribe(
			context.Background(),
			SubscribeOptions{Name: "timeout-crash", Buffer: 1, Timeout: 50 * time.Millisecond, PanicPolicy: Crash},
			func(context.Context, Event) error {
				panic("boom-timeout-crash")
			},
		)
		if err != nil {
			fmt.Printf("setup failed: %v", err)
			os.Exit(0)
		}

		bus.Publish(context.Background(), Event{Kind: KindAgentError})
		// Give the timeout handler goroutine time to panic and crash the
		// process. If we reach the end of this sleep, the panic did NOT
		// propagate — exit cleanly so the parent test can tell the difference.
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestTimeoutHandlerPanicCrashesProcess")
	cmd.Env = append(os.Environ(), "BE_CRASHER=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the subprocess to crash under PanicPolicy: Crash, but it exited cleanly; output:\n%s", out)
	}
	if !strings.Contains(string(out), "panic:") {
		t.Fatalf("expected a panic trace in the crashed subprocess output, got:\n%s", out)
	}
	if !strings.Contains(string(out), "boom-timeout-crash") {
		t.Fatalf("expected the panic message in the crashed subprocess output, got:\n%s", out)
	}
}

func TestLockedHandlerProcessesSequentially(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	var active atomic.Int64
	var maxActive atomic.Int64
	sub, err := bus.Channel().Subscribe(
		context.Background(),
		SubscribeOptions{Name: "locked", Buffer: 8, Concurrency: Locked},
		func(context.Context, Event) error {
			current := active.Add(1)
			for {
				currentMax := maxActive.Load()
				if current <= currentMax || maxActive.CompareAndSwap(currentMax, current) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			active.Add(-1)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	for i := 0; i < 5; i++ {
		bus.Publish(context.Background(), Event{Kind: KindAgentLLMDelta})
	}
	waitForStat(t, func() uint64 {
		return sub.Stats().Handled
	}, 5)

	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max active handlers = %d, want 1", got)
	}
}

func TestHandlerTimeoutDoesNotWedgeLockedSubscription(t *testing.T) {
	t.Parallel()

	bus := NewBus()
	defer closeBus(t, bus)

	releaseFirst := make(chan struct{})
	defer close(releaseFirst)

	var calls atomic.Uint64
	sub, err := bus.Channel().Subscribe(
		context.Background(),
		SubscribeOptions{Name: "timeout", Buffer: 2, Concurrency: Locked, Timeout: 20 * time.Millisecond},
		func(context.Context, Event) error {
			if calls.Add(1) == 1 {
				<-releaseFirst
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	bus.Publish(context.Background(), Event{Kind: Kind("test.first")})
	waitForStat(t, func() uint64 {
		return sub.Stats().TimedOut
	}, 1)

	bus.Publish(context.Background(), Event{Kind: Kind("test.second")})
	waitForStat(t, func() uint64 {
		return sub.Stats().Handled
	}, 1)

	if got := sub.Stats().Failed; got != 1 {
		t.Fatalf("subscription failed = %d, want timeout failure", got)
	}
}

func waitForSubscriptionDone(t *testing.T, sub Subscription) {
	t.Helper()

	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription to stop")
	}
}

func waitForStat(t *testing.T, stat func() uint64, want uint64) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if got := stat(); got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatalf("timed out waiting for stat >= %d", want)
		}
	}
}
