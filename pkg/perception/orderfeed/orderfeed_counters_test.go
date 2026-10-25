package orderfeed

import (
	"context"
	"testing"
	"time"
)

// The counters add up: every Publish is delivered or dropped, and an event still
// queued when the publisher is replaced or cleared is dropped, not forgotten.
func TestCounters_AddUpWhenThePublisherIsReplacedWhileEventsAreQueued(t *testing.T) {
	reset(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	SetPublisher(func(OrderEvent) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	})

	before := ReadStats()
	const attempts = QueueSize + 40
	Publish(context.Background(), OrderEvent{})
	<-entered // the worker holds the first event; the rest queue up behind it
	for i := 1; i < attempts; i++ {
		Publish(context.Background(), OrderEvent{})
	}

	SetPublisher(nil) // clears while the queue is full and the worker is stuck
	close(release)
	waitFor(t, "the stuck call to return", func() bool { return ReadStats().Delivered-before.Delivered >= 1 })
	time.Sleep(20 * time.Millisecond) // the worker sees the stop and ends

	after := ReadStats()
	delivered, dropped := after.Delivered-before.Delivered, after.Dropped-before.Dropped
	if delivered+dropped != attempts {
		t.Fatalf("delivered %d + dropped %d = %d, want %d attempts", delivered, dropped, delivered+dropped, attempts)
	}
	if delivered != 1 {
		t.Errorf("delivered = %d, want only the event the publisher already held", delivered)
	}
	if got := after.Published - before.Published; got != QueueSize+1 {
		t.Errorf("published = %d, want %d (one held plus a full queue)", got, QueueSize+1)
	}
}
