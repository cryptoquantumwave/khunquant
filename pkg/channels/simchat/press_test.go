package simchat

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func (r *rig) sendCard(chat string) string {
	r.t.Helper()
	ids, err := r.ch.SendWithIDs(context.Background(), card(chat))
	if err != nil || len(ids) != 1 {
		r.t.Fatalf("SendWithIDs = %v, %v", ids, err)
	}
	return ids[0]
}

// The payload the router gets is the one encoded at send time, from the card's own
// button, with the persona's canonical sender and the card as the message tapped.
func TestPressUsesEncodedPayload(t *testing.T) {
	r := newRig(t)
	id := r.sendCard("dm:owner")
	want, err := channels.EncodeCallbackData(card("dm:owner").Actions[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	wantAction, wantPayload, err := channels.ParseCallbackData(want, 64)
	if err != nil {
		t.Fatal(err)
	}

	res := r.mustRun("press", `{"message":"`+id+`","action":"reject"}`)
	seen := r.router.seen()
	if len(seen) != 1 {
		t.Fatalf("router inputs = %+v, want one tap", seen)
	}
	in := seen[0]
	if in.Callback == nil || in.Callback.Action != wantAction || in.Callback.Payload != wantPayload || in.Callback.CallbackID != "cb-1" {
		t.Errorf("callback = %+v, want %s %s cb-1", in.Callback, wantAction, wantPayload)
	}
	if in.Channel != "simchat" || in.ChatID != "dm:owner" || in.MessageID != id || in.ReplyToMessageID != id ||
		in.SenderID != "simchat:owner" || in.PeerKind != "direct" || in.Text != "" {
		t.Errorf("tap input = %+v", in)
	}
	if _, ok := r.inbound(); ok {
		t.Error("a tap reached the agent")
	}
	if res["as"] != "simchat:owner" || res["toast"] != channels.TapInvalidButton {
		t.Errorf("press result = %v (the fake router handles nothing, so the tap is dropped as invalid)", res)
	}
}

// A reply that asks to edit the card replaces it and removes the buttons; any other
// full reply is said beside the card unless the toast already said it.
func TestPressEditsCardOrReplies(t *testing.T) {
	r := newRig(t)
	r.router.handle = func(in channels.ControlInput) (channels.ControlReply, bool) {
		if in.Callback.Action == "reject" {
			return channels.ControlReply{Text: "Rejected the order.", EditOriginal: true, AnswerCallback: "Rejected"}, true
		}
		return channels.ControlReply{Text: "Muted for 30 days.\nUndo with /unmute.", AnswerCallback: "Muted"}, true
	}
	reject, mute := r.sendCard("dm:owner"), r.sendCard("dm:owner")

	res := r.mustRun("press", `{"message":"`+reject+`","action":"reject"}`)
	if res["edit"] != true || res["toast"] != "Rejected" {
		t.Fatalf("reject result = %v", res)
	}
	msgs := r.mustRun("outbox", `{"to":"dm:owner"}`)["messages"].([]any)
	if c := msgs[0].(map[string]any); c["text"] != "Rejected the order." || c["buttons_live"] != false {
		t.Fatalf("the card = %v, want edited with its buttons removed", c)
	}
	if es := r.entries("chat.edit"); len(es) != 1 || payloadOf(t, es[0])["buttons_removed"] != true {
		t.Fatalf("chat.edit entries = %+v", es)
	}
	if es := r.entries("chat.press"); len(es) != 1 || es[0].Subject != "dm:owner" || payloadOf(t, es[0])["action"] != "reject" {
		t.Fatalf("chat.press entries = %+v", es)
	}

	// The same "reject" reply cannot be edited a second time into a new message: a
	// card whose buttons are gone is refused unless the client is stale.
	if _, err := r.run("press", `{"message":"`+reject+`","action":"reject"}`); err == nil {
		t.Fatal("a removed button was tapped")
	}

	r.mustRun("press", `{"on":{"to":"dm:owner","severity":"warning","contains":["BTC"]},"action":"mute"}`)
	msgs = r.mustRun("outbox", `{"to":"dm:owner"}`)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("outbox = %v, want the two cards and the reply", msgs)
	}
	if reply := msgs[2].(map[string]any); reply["reply_to"] != mute || !strings.HasPrefix(reply["text"].(string), "Muted for 30 days.") {
		t.Fatalf("reply = %v, want a reply to the card %s", reply, mute)
	}
}

func TestPressStrangerNotAuthorized(t *testing.T) {
	r := newRig(t, func(c *config.SimChatConfig) { c.AllowFrom = config.FlexibleStringSlice{"simchat:owner"} })
	r.router.handle = func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "done"}, true
	}
	group := r.sendCard("group:team")
	res := r.mustRun("press", `{"message":"`+group+`","action":"approve","as":"alice"}`)
	if res["toast"] != channels.TapNotAuthorized {
		t.Fatalf("a sender outside allow_from got %v, want %q", res, channels.TapNotAuthorized)
	}
	if n := len(r.router.seen()); n != 0 {
		t.Fatalf("the router saw %d taps from a sender the allow-list refuses", n)
	}
	// Nobody else can tap a button in the owner's DM at all.
	dm := r.sendCard("dm:owner")
	if _, err := r.run("press", `{"message":"`+dm+`","action":"approve","as":"stranger"}`); err == nil {
		t.Fatal("a stranger tapped a button in the owner's DM")
	}
	// The owner can, in the group too (a tap in a chat the owner shares).
	if res := r.mustRun("press", `{"message":"`+group+`","action":"approve","as":"owner"}`); res["toast"] != "done" {
		t.Fatalf("owner tap = %v", res)
	}
	if seen := r.router.seen(); len(seen) != 1 || seen[0].PeerKind != "group" {
		t.Fatalf("router inputs = %+v, want one group tap", seen)
	}
}

func TestPressRemovedButtonRefused(t *testing.T) {
	r := newRig(t)
	id := r.sendCard("dm:owner")
	if err := r.ch.EditMessage(context.Background(), "dm:owner", id, "Expired."); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run("press", `{"message":"`+id+`","action":"approve"}`); err == nil {
		t.Fatal("a removed keyboard was tapped")
	}
	if n := len(r.router.seen()); n != 0 {
		t.Fatalf("router saw %d taps of a removed button", n)
	}
	r.mustRun("press", `{"message":"`+id+`","action":"approve","force":true}`)
	if n := len(r.router.seen()); n != 1 {
		t.Fatalf("a forced stale tap reached the router %d times, want 1", n)
	}
	if _, err := r.run("press", `{"message":"`+id+`","action":"snooze_1h","force":true}`); err == nil {
		t.Fatal("a button the message never had was tapped")
	}
}

func TestReactCallsFeedbackObserver(t *testing.T) {
	r := newRig(t, func(c *config.SimChatConfig) { c.AllowFrom = config.FlexibleStringSlice{"simchat:owner"} })
	id := r.sendCard("dm:owner")
	r.mustRun("react", `{"message":"`+id+`","emoji":"up"}`)
	r.mustRun("react", `{"on":{"intervention_id":"I-1"},"emoji":"down"}`)
	r.router.mu.Lock()
	reactions, ins := r.router.reactions, r.router.reactIn
	r.router.mu.Unlock()
	if len(reactions) != 2 || reactions[0] != "\U0001F44D" || reactions[1] != "\U0001F44E" {
		t.Fatalf("reactions = %q, want thumbs up then down", reactions)
	}
	if in := ins[0]; in.MessageID != id || in.ReplyToMessageID != id || in.SenderID != "simchat:owner" || in.ChatID != "dm:owner" || in.PeerKind != "direct" {
		t.Errorf("reaction input = %+v", in)
	}

	// The allow-list applies to a reaction as to a message.
	grp := r.sendCard("group:team")
	if res := r.mustRun("react", `{"message":"`+grp+`","emoji":"up","as":"alice"}`); res["allowed"] != false {
		t.Errorf("a reaction by a sender outside allow_from = %v", res)
	}
	if n := len(r.router.reactions); n != 2 {
		t.Errorf("observer was told %d reactions, want 2", n)
	}
	if es := r.entries("chat.react"); len(es) != 3 || es[0].Subject != "dm:owner" {
		t.Errorf("chat.react entries = %+v", es)
	}
	if _, err := r.run("react", `{"message":"sc-99","emoji":"up"}`); err == nil {
		t.Error("a reaction to an unknown message was accepted")
	}
}
