package orderfeed

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

func goroutinesIn(substr string) int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, substr) {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func reset(t *testing.T) {
	t.Helper()
	SetPublisher(nil)
	t.Cleanup(func() { SetPublisher(nil) })
}

func TestPublish_NoPublisherIsANoOpAndStartsNothing(t *testing.T) {
	reset(t)
	before := goroutinesIn("orderfeed.(*feed).run")
	s := ReadStats()
	for i := 0; i < 100; i++ {
		Publish(context.Background(), OrderEvent{Symbol: "BTC/USDT"})
	}
	if after := ReadStats(); after != s {
		t.Fatalf("counters moved with no publisher: %+v -> %+v", s, after)
	}
	if goroutinesIn("orderfeed.(*feed).run") != before {
		t.Fatal("a goroutine runs with no publisher set")
	}
}

func TestPublish_DeliversInOrderAndStampsTime(t *testing.T) {
	reset(t)
	var mu sync.Mutex
	var got []OrderEvent
	SetPublisher(func(ev OrderEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	for _, id := range []string{"1", "2", "3"} {
		Publish(context.Background(), OrderEvent{OrderID: id, Paper: id == "2"})
	}
	waitFor(t, "three events", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	mu.Lock()
	defer mu.Unlock()
	for i, id := range []string{"1", "2", "3"} {
		if got[i].OrderID != id {
			t.Errorf("event %d = %q, want %q", i, got[i].OrderID, id)
		}
		if got[i].At.IsZero() {
			t.Errorf("event %d has no time", i)
		}
	}
	if !got[1].Paper || got[0].Paper {
		t.Error("the Paper flag did not travel with the event")
	}
}

func TestPublish_BlockedPublisherDoesNotBlockTheCaller(t *testing.T) {
	reset(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	SetPublisher(func(OrderEvent) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	})
	t.Cleanup(func() { close(release) })

	before := ReadStats()
	done := make(chan struct{})
	go func() {
		for i := 0; i < QueueSize*3; i++ {
			Publish(context.Background(), OrderEvent{OrderID: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked behind a publisher that does not return")
	}
	<-started
	after := ReadStats()
	if after.Dropped-before.Dropped == 0 {
		t.Fatal("a full queue dropped nothing")
	}
	if got := (after.Published - before.Published) + (after.Dropped - before.Dropped); got != QueueSize*3 {
		t.Fatalf("published+dropped = %d, want %d: every event is either queued or counted", got, QueueSize*3)
	}
}

func TestPublish_PanickingPublisherIsContained(t *testing.T) {
	reset(t)
	var mu sync.Mutex
	calls := 0
	SetPublisher(func(OrderEvent) {
		mu.Lock()
		calls++
		mu.Unlock()
		panic("boom")
	})
	before := ReadStats().Panics
	Publish(context.Background(), OrderEvent{})
	Publish(context.Background(), OrderEvent{})
	waitFor(t, "both calls", func() bool { mu.Lock(); defer mu.Unlock(); return calls == 2 })
	waitFor(t, "the panics counted", func() bool { return ReadStats().Panics-before == 2 })
}

func TestSetPublisher_ClearOnlyRemovesItsOwn(t *testing.T) {
	reset(t)
	clearOld := SetPublisher(func(OrderEvent) {})
	got := make(chan OrderEvent, 1)
	SetPublisher(func(ev OrderEvent) { got <- ev }) // the next generation
	clearOld()                                      // the old generation stops late
	Publish(context.Background(), OrderEvent{OrderID: "new"})
	select {
	case ev := <-got:
		if ev.OrderID != "new" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the old generation's clear removed the new publisher")
	}
}

func TestSetPublisher_NilStopsTheGoroutine(t *testing.T) {
	reset(t)
	base := goroutinesIn("orderfeed.(*feed).run")
	SetPublisher(func(OrderEvent) {})
	waitFor(t, "the worker", func() bool { return goroutinesIn("orderfeed.(*feed).run") == base+1 })
	SetPublisher(nil)
	waitFor(t, "the worker to end", func() bool { return goroutinesIn("orderfeed.(*feed).run") == base })
}

func TestOrderFeed_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/perception/orderfeed",
		Allowed: []string{"pkg/proactive"},
		// pkg/tools imports this package: it must not be able to reach back.
		Forbidden: []string{"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention", "pkg/config", "pkg/events", "pkg/perception"},
	})
}
