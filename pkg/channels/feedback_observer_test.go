package channels

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

// observingRouter handles nothing and records what the feedback side was told.
type observingRouter struct {
	stubRouter
	panics  bool
	replies []ControlInput
}

func (r *observingRouter) ObserveReply(_ context.Context, in ControlInput) {
	r.replies = append(r.replies, in)
	if r.panics {
		panic("boom")
	}
}
func (r *observingRouter) ObserveReaction(context.Context, ControlInput, string) {}

// A reply that is not a control command is offered to the feedback side with the
// sender's own words, and is published to the agent exactly as it is without one.
func TestHandleMessage_ReplyIsObservedAndStillPublished(t *testing.T) {
	say := func(r ControlRouter) (bus.InboundMessage, bool) {
		mb := bus.NewMessageBus()
		ch := NewBaseChannel("telegram", nil, mb, nil)
		ch.SetControlRouter(r)
		ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: "7"}, "31", "7", "7",
			"> quoted\nyes please", nil,
			map[string]string{"reply_to_message_id": "30", MetaOwnText: "yes please"})
		select {
		case m := <-mb.InboundChan():
			return m, true
		default:
			return bus.InboundMessage{}, false
		}
	}
	want, ok := say(stubRouter{})
	if !ok {
		t.Fatal("the golden message was not published")
	}

	for name, obs := range map[string]*observingRouter{"observer": {}, "observer that panics": {panics: true}} {
		got, ok := say(obs)
		if !ok || got.Content != want.Content || got.MessageID != want.MessageID || len(got.Metadata) != len(want.Metadata) ||
			got.Metadata["reply_to_message_id"] != "30" {
			t.Errorf("%s: published %+v (%v), want %+v", name, got, ok, want)
		}
		if len(obs.replies) != 1 || obs.replies[0].Text != "yes please" || obs.replies[0].ReplyToMessageID != "30" {
			t.Errorf("%s: observed %+v, want one reply carrying the sender's own words", name, obs.replies)
		}
	}
}

// A message that is not a reply is not offered to the feedback side.
func TestHandleMessage_NonReplyIsNotObserved(t *testing.T) {
	mb := bus.NewMessageBus()
	ch := NewBaseChannel("telegram", nil, mb, nil)
	obs := &observingRouter{}
	ch.SetControlRouter(obs)
	ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: "7"}, "31", "7", "7", "hello", nil, nil)
	if len(obs.replies) != 0 {
		t.Fatalf("observed %+v", obs.replies)
	}
}

// The mute button has a callback code and round-trips.
func TestCallbackCodec_MuteButton(t *testing.T) {
	data, err := EncodeCallbackData(bus.Action{ID: "mute", Label: "Mute"}, 64)
	if err != nil || data != "kq1|mute|" {
		t.Fatalf("encode = %q, %v", data, err)
	}
	if id, payload, err := ParseCallbackData(data, 64); err != nil || id != "mute" || payload != "" {
		t.Fatalf("parse = %q %q %v", id, payload, err)
	}
}
