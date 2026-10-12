package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
)

func TestParseSteeringMode_All(t *testing.T) {
	if got := parseSteeringMode("all"); got != SteeringAll {
		t.Errorf("parseSteeringMode(all) = %q, want %q", got, SteeringAll)
	}
}

func TestParseSteeringMode_Default(t *testing.T) {
	if got := parseSteeringMode("one-at-a-time"); got != SteeringOneAtATime {
		t.Errorf("parseSteeringMode(one-at-a-time) = %q, want %q", got, SteeringOneAtATime)
	}
}

func TestParseSteeringMode_Unknown(t *testing.T) {
	if got := parseSteeringMode("unknown"); got != SteeringOneAtATime {
		t.Errorf("parseSteeringMode(unknown) = %q, want %q", got, SteeringOneAtATime)
	}
}

func TestSteeringQueue_Empty(t *testing.T) {
	sq := newSteeringQueue(SteeringOneAtATime)
	if got := sq.dequeue(); got != nil {
		t.Errorf("dequeue on empty queue = %v, want nil", got)
	}
}

func TestSteeringQueue_PushDequeue_OneAtATime(t *testing.T) {
	sq := newSteeringQueue(SteeringOneAtATime)
	msg1 := providers.Message{Role: "user", Content: "first"}
	msg2 := providers.Message{Role: "user", Content: "second"}

	if err := sq.push(msg1); err != nil {
		t.Fatalf("push msg1: %v", err)
	}
	if err := sq.push(msg2); err != nil {
		t.Fatalf("push msg2: %v", err)
	}

	got := sq.dequeue()
	if len(got) != 1 {
		t.Fatalf("dequeue one-at-a-time: got %d messages, want 1", len(got))
	}
	if got[0].Content != "first" {
		t.Errorf("dequeue got %q, want first", got[0].Content)
	}

	// Second dequeue returns second message.
	got2 := sq.dequeue()
	if len(got2) != 1 || got2[0].Content != "second" {
		t.Errorf("second dequeue got %v, want second", got2)
	}
}

func TestSteeringQueue_PushDequeue_All(t *testing.T) {
	sq := newSteeringQueue(SteeringAll)
	_ = sq.push(providers.Message{Role: "user", Content: "a"})
	_ = sq.push(providers.Message{Role: "user", Content: "b"})
	_ = sq.push(providers.Message{Role: "user", Content: "c"})

	got := sq.dequeue()
	if len(got) != 3 {
		t.Fatalf("dequeue all: got %d messages, want 3", len(got))
	}
	if sq.len() != 0 {
		t.Error("queue should be empty after dequeue-all")
	}
}

func TestSteeringQueue_Full(t *testing.T) {
	sq := newSteeringQueue(SteeringOneAtATime)
	for i := 0; i < MaxQueueSize; i++ {
		if err := sq.push(providers.Message{Role: "user"}); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if err := sq.push(providers.Message{Role: "user"}); err == nil {
		t.Error("push to full queue should return error")
	}
}

func TestSteeringQueue_SetGetMode(t *testing.T) {
	sq := newSteeringQueue(SteeringOneAtATime)
	if sq.getMode() != SteeringOneAtATime {
		t.Errorf("initial mode = %q, want one-at-a-time", sq.getMode())
	}
	sq.setMode(SteeringAll)
	if sq.getMode() != SteeringAll {
		t.Errorf("after setMode(all) = %q, want all", sq.getMode())
	}
}

func TestAgentLoop_SteeringMode_NilSteering(t *testing.T) {
	al := &AgentLoop{}
	if al.SteeringMode() != SteeringOneAtATime {
		t.Error("nil steering should return default SteeringOneAtATime")
	}
}

func TestAgentLoop_SetSteeringMode_NilSteering(t *testing.T) {
	al := &AgentLoop{}
	al.SetSteeringMode(SteeringAll) // should not panic
}

func TestAgentLoop_DequeueSteeringMessages_NilSteering(t *testing.T) {
	al := &AgentLoop{}
	if got := al.dequeueSteeringMessages(); got != nil {
		t.Errorf("nil steering dequeue = %v, want nil", got)
	}
}

func TestAgentLoop_SteeringMode_WithQueue(t *testing.T) {
	al := &AgentLoop{steering: newSteeringQueue(SteeringAll)}
	if al.SteeringMode() != SteeringAll {
		t.Errorf("SteeringMode = %q, want all", al.SteeringMode())
	}
}

func TestAgentLoop_SetSteeringMode_WithQueue(t *testing.T) {
	al := &AgentLoop{steering: newSteeringQueue(SteeringOneAtATime)}
	al.SetSteeringMode(SteeringAll)
	if al.SteeringMode() != SteeringAll {
		t.Errorf("SteeringMode after set = %q, want all", al.SteeringMode())
	}
}

func TestAgentLoop_DequeueSteeringMessages_WithQueue(t *testing.T) {
	al := &AgentLoop{steering: newSteeringQueue(SteeringOneAtATime)}
	_ = al.steering.push(providers.Message{Role: "user", Content: "hi"})
	got := al.dequeueSteeringMessages()
	if len(got) != 1 {
		t.Fatalf("dequeue with queue: got %d, want 1", len(got))
	}
}

func TestAgentLoop_EnqueueSteeringMessage_NilSteering(t *testing.T) {
	al := &AgentLoop{}
	err := al.enqueueSteeringMessage("scope", "agent", providers.Message{Role: "user", Content: "hi"})
	if err == nil {
		t.Error("enqueueSteeringMessage with nil steering should return error")
	}
}

func TestAgentLoop_EnqueueSteeringMessage_WithQueue(t *testing.T) {
	al := &AgentLoop{steering: newSteeringQueue(SteeringOneAtATime)}
	err := al.enqueueSteeringMessage("scope", "agent", providers.Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Errorf("enqueueSteeringMessage with queue: %v", err)
	}
	if al.steering.len() != 1 {
		t.Errorf("expected 1 queued message, got %d", al.steering.len())
	}
}

func TestAgentLoop_ResolveSteeringTarget_SystemChannel(t *testing.T) {
	al := &AgentLoop{}
	msg := bus.InboundMessage{Channel: "system", Content: "ping"}
	_, _, ok := al.resolveSteeringTarget(msg)
	if ok {
		t.Error("system channel should return ok=false")
	}
}

func TestAgentLoop_RequeueInboundMessage_NilBus(t *testing.T) {
	al := &AgentLoop{}
	err := al.requeueInboundMessage(bus.InboundMessage{Channel: "telegram", Content: "hi"})
	if err != nil {
		t.Errorf("requeueInboundMessage nil bus should return nil, got %v", err)
	}
}

// A requeued message is one to be processed later, as a turn of its own. It
// has to go back on the inbound bus, whole. It used to be published outbound,
// which sent the user's own words back to them as if the agent had said them,
// and the message was never processed at all.
func TestRequeueInbound_GoesInbound(t *testing.T) {
	msgBus := bus.NewMessageBus()
	al := &AgentLoop{bus: msgBus}

	msg := bus.InboundMessage{
		Channel:    "telegram",
		SenderID:   "user-7",
		ChatID:     "chat-9",
		Content:    "process me later",
		Media:      []string{"media://abc"},
		SessionKey: "agent:main:requeue",
		MessageID:  "m-42",
		Metadata:   map[string]string{"account_id": "acct-1"},
	}
	if err := al.requeueInboundMessage(msg); err != nil {
		t.Fatalf("requeueInboundMessage: %v", err)
	}

	// requeueInboundMessage publishes before it returns and the bus is
	// buffered, so both channels can be read without waiting.
	select {
	case got := <-msgBus.InboundChan():
		if !reflect.DeepEqual(got, msg) {
			t.Errorf("requeued message was altered on the way:\n got %+v\nwant %+v", got, msg)
		}
	default:
		t.Fatal("the requeued message is not on the inbound bus")
	}

	select {
	case echoed := <-msgBus.OutboundChan():
		t.Fatalf("the requeued message was sent to the user as an agent reply: %+v", echoed)
	default:
	}
}

func TestAgentLoop_Continue_EmptyQueue(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	result, err := al.Continue(context.Background(), "session", "channel", "chatid")
	if err != nil {
		t.Errorf("Continue with empty queue should not error: %v", err)
	}
	if result != "" {
		t.Errorf("Continue with empty queue should return empty string, got %q", result)
	}
}
