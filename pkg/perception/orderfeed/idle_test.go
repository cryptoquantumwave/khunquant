package orderfeed

import (
	"context"
	"testing"
)

// The feed is busy while an event waits in its queue and while the publisher holds one,
// and idle with no publisher at all or once the publisher returned.
func TestOrderFeed_Idle(t *testing.T) {
	reset(t)
	if !Idle() {
		t.Fatal("no publisher: nothing to wait for")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	SetPublisher(func(OrderEvent) {
		if first {
			first = false
			close(entered)
			<-release
		}
	})
	if !Idle() {
		t.Fatal("a publisher with nothing queued is idle")
	}

	Publish(context.Background(), OrderEvent{Symbol: "BTC/USDT"})
	<-entered // taken off the queue, inside the publisher
	stats := ReadStats()
	if Idle() || Idle() {
		t.Fatal("idle while the publisher holds an event")
	}
	Publish(context.Background(), OrderEvent{Symbol: "ETH/USDT"}) // queued behind it
	if Idle() {
		t.Fatal("idle with an event queued")
	}
	if got := ReadStats(); got.Delivered != stats.Delivered || got.Dropped != stats.Dropped {
		t.Fatalf("Idle moved the counters: %+v -> %+v", stats, got)
	}
	close(release)
	waitFor(t, "idle after the publisher returned", Idle)
}
