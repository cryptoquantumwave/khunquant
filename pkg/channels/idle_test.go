package channels

import (
	"context"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

// The manager is busy while an outbound message waits in a worker queue, while a
// worker holds it inside Send, and while the dispatcher is handing it from the bus to a
// worker; it is idle otherwise, and asking changes nothing.
func TestChannelManager_IdleDuringSend(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	ch := &mockChannel{sendFn: func(context.Context, bus.OutboundMessage) error {
		entered <- struct{}{}
		<-release
		return nil
	}}
	m := newTestManager()
	w := &channelWorker{
		ch: ch, queue: make(chan bus.OutboundMessage, 4), mediaQueue: make(chan bus.OutboundMediaMessage, 4),
		done: make(chan struct{}), mediaDone: make(chan struct{}), limiter: rate.NewLimiter(1000, 10),
	}
	m.channels["test"], m.workers["test"] = ch, w
	if !m.Idle() {
		t.Fatal("a manager with empty queues is idle")
	}

	w.queue <- bus.OutboundMessage{Channel: "test", ChatID: "1", Content: "a"}
	if m.Idle() || m.Idle() {
		t.Fatal("idle with a message in a worker queue")
	}
	if len(w.queue) != 1 {
		t.Fatal("Idle took a message off the queue")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runWorker(ctx, "test", w)
	<-entered // taken off the queue, held inside Send
	if len(w.queue) != 0 {
		t.Fatal("the worker did not take the message")
	}
	if m.Idle() {
		t.Fatal("idle while a message is dequeued but still being sent")
	}
	close(release)
	waitIdle(t, m)

	w.mediaQueue <- bus.OutboundMediaMessage{Channel: "test"}
	if m.Idle() {
		t.Fatal("idle with a media message queued")
	}
	<-w.mediaQueue
	if !m.Idle() {
		t.Fatal("busy after the media queue was emptied")
	}
}

// A message the dispatcher has taken off the bus is counted until it is in a queue.
func TestChannelManager_IdleWhileRouting(t *testing.T) {
	m := newTestManager()
	block := make(chan struct{})
	entered := make(chan struct{})
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		dispatchLoop(ctx, m, m.bus.OutboundChan(),
			func(msg bus.OutboundMessage) string { return msg.Channel },
			func(context.Context, *channelWorker, bus.OutboundMessage) bool {
				close(entered)
				<-block
				return true
			}, "start", "stop", "unknown", "noworker")
	}()
	w := &channelWorker{queue: make(chan bus.OutboundMessage, 1), mediaQueue: make(chan bus.OutboundMediaMessage, 1)}
	m.channels["test"], m.workers["test"] = &mockChannel{}, w
	if err := m.bus.PublishOutbound(ctx, bus.OutboundMessage{Channel: "test", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	<-entered // off the bus (which is empty) and not yet in a worker queue
	if !m.bus.Idle() {
		t.Fatal("the bus should be empty")
	}
	if m.Idle() {
		t.Fatal("idle while the dispatcher holds a message between the bus and a worker")
	}
	close(block)
	waitIdle(t, m)
	cancel()
	<-done
}

func waitIdle(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !m.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("manager did not go idle")
		}
		time.Sleep(time.Millisecond)
	}
}
