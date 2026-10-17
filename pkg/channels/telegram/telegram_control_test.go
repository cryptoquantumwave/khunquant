package telegram

import (
	"context"
	"sync"
	"testing"

	"github.com/mymmrac/telego"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// recordingRouter remembers what the control path was offered and handles nothing.
type recordingRouter struct {
	mu  sync.Mutex
	got []channels.ControlInput
}

func (r *recordingRouter) TryHandle(_ context.Context, in channels.ControlInput) (channels.ControlReply, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, in)
	return channels.ControlReply{}, false
}
func (r *recordingRouter) Recognises(channels.ControlInput) bool { return false }

func newControlTestChannel() (*TelegramChannel, *recordingRouter, *bus.MessageBus) {
	mb := bus.NewMessageBus()
	ch := &TelegramChannel{
		BaseChannel: channels.NewBaseChannel("telegram", nil, mb, nil),
		chatIDs:     make(map[string]int64),
		ctx:         context.Background(),
		config:      &config.Config{},
	}
	r := &recordingRouter{}
	ch.SetControlRouter(r)
	return ch, r, mb
}

// A reply to a card carries the quoted message in front of what the sender typed;
// the control path is given only the sender's own words, the reply target, and
// none of the hints reach the agent.
func TestHandleMessage_ReplyGivesTheControlPathTheSendersOwnText(t *testing.T) {
	ch, router, mb := newControlTestChannel()
	msg := &telego.Message{
		Text: "yes", MessageID: 31,
		Chat: telego.Chat{ID: 123, Type: "private"},
		From: &telego.User{ID: 42, FirstName: "Alice"},
		ReplyToMessage: &telego.Message{
			MessageID: 30, Text: "Proposal 7F3K: buy 1 ETH", From: &telego.User{ID: 99, IsBot: true},
		},
	}
	if err := ch.handleMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	if len(router.got) != 1 {
		t.Fatalf("%d inputs offered to the control path", len(router.got))
	}
	in := router.got[0]
	if in.Text != "yes" || in.ReplyToMessageID != "30" || in.Forwarded || in.ChatID != "123" || in.PeerKind != "direct" ||
		in.SenderID != "telegram:42" || in.Channel != "telegram" || in.MessageID != "31" {
		t.Errorf("input = %+v", in)
	}

	inbound := <-mb.InboundChan()
	if inbound.Content == "yes" || len(inbound.Content) < 10 {
		t.Errorf("the agent's text lost the quote: %q", inbound.Content)
	}
	for _, k := range []string{channels.MetaOwnText, channels.MetaForwarded} {
		if _, ok := inbound.Metadata[k]; ok {
			t.Errorf("hint %q reached the agent", k)
		}
	}
	if inbound.Metadata["reply_to_message_id"] != "30" {
		t.Errorf("metadata = %+v", inbound.Metadata)
	}
}

func TestHandleMessage_ForwardedIsMarkedForTheControlPath(t *testing.T) {
	ch, router, mb := newControlTestChannel()
	msg := &telego.Message{
		Text: "/approve 7F3K", MessageID: 32,
		Chat:          telego.Chat{ID: 123, Type: "private"},
		From:          &telego.User{ID: 42, FirstName: "Alice"},
		ForwardOrigin: &telego.MessageOriginUser{Type: "user", SenderUser: telego.User{ID: 5}},
	}
	if err := ch.handleMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(router.got) != 1 || !router.got[0].Forwarded || router.got[0].Text != "/approve 7F3K" {
		t.Fatalf("input = %+v", router.got)
	}
	inbound := <-mb.InboundChan()
	if inbound.Content != "/approve 7F3K" {
		t.Errorf("content = %q", inbound.Content)
	}
	if _, ok := inbound.Metadata[channels.MetaForwarded]; ok {
		t.Error("the hint reached the agent")
	}
}

// An ordinary message is offered with its own text and no reply target.
func TestHandleMessage_PlainMessageOffersTheCommandText(t *testing.T) {
	ch, router, _ := newControlTestChannel()
	msg := &telego.Message{
		Text: "/stop", MessageID: 33,
		Chat: telego.Chat{ID: 123, Type: "private"},
		From: &telego.User{ID: 42, FirstName: "Alice"},
	}
	if err := ch.handleMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(router.got) != 1 || router.got[0].Text != "/stop" || router.got[0].ReplyToMessageID != "" || router.got[0].Forwarded {
		t.Fatalf("input = %+v", router.got)
	}
}
