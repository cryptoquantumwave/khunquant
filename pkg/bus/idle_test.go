package bus

import (
	"context"
	"testing"
)

// Idle is false while a message waits in any of the three queues, true once each is
// taken, and reading it takes nothing.
func TestMessageBus_Idle(t *testing.T) {
	ctx := context.Background()
	mb := NewMessageBus()
	defer mb.Close()
	if !mb.Idle() {
		t.Fatal("a new bus is idle")
	}

	pubs := map[string]func() error{
		"inbound":  func() error { return mb.PublishInbound(ctx, InboundMessage{Content: "x"}) },
		"outbound": func() error { return mb.PublishOutbound(ctx, OutboundMessage{Content: "x"}) },
		"media":    func() error { return mb.PublishOutboundMedia(ctx, OutboundMediaMessage{}) },
	}
	takes := map[string]func(){
		"inbound":  func() { <-mb.InboundChan() },
		"outbound": func() { <-mb.OutboundChan() },
		"media":    func() { <-mb.OutboundMediaChan() },
	}
	for name, pub := range pubs {
		if err := pub(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if mb.Idle() || mb.Idle() { // asking twice takes nothing
			t.Fatalf("%s: idle with a message queued", name)
		}
		takes[name]()
		if !mb.Idle() {
			t.Fatalf("%s: busy after the message was taken", name)
		}
	}
}
