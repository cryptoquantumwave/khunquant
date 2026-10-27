package simchat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func TestFactoryRefusesWhenUnlocked(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if ch, err := New(testConfig(), bus.NewMessageBus()); err == nil || ch != nil {
		t.Fatalf("New on an unlocked process = %v, %v, want an error and no channel", ch, err)
	}
	lockForTest(t)
	cfg := testConfig()
	cfg.Channels.SimChat.Enabled = false
	if ch, err := New(cfg, bus.NewMessageBus()); err == nil || ch != nil {
		t.Fatalf("New with simchat disabled = %v, %v, want an error", ch, err)
	}
}

func TestStartFailsWithoutWorld(t *testing.T) {
	lockForTest(t)
	ch, err := New(testConfig(), bus.NewMessageBus())
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Start(context.Background()); err == nil || ch.IsRunning() {
		t.Fatalf("Start without a world = %v (running %v), want an error", err, ch.IsRunning())
	}
}

func TestSendOwnerDMReachesBus(t *testing.T) {
	r := newRig(t)
	res := r.mustRun("send", `{"as":"owner","text":"hello"}`)
	if res["chat"] != "dm:owner" || res["sender"] != "simchat:owner" {
		t.Fatalf("send result = %v", res)
	}
	m, ok := r.inbound()
	if !ok {
		t.Fatal("nothing reached the bus")
	}
	if m.Channel != "simchat" || m.SenderID != "simchat:owner" || m.Peer.Kind != "direct" || m.ChatID != "dm:owner" ||
		m.Peer.ID != "dm:owner" || m.Content != "hello" || m.Sender.Platform != "simchat" || m.Sender.PlatformID != "owner" {
		t.Fatalf("inbound = %+v", m)
	}
	if es := r.entries("chat.in"); len(es) != 1 || es[0].Subject != "dm:owner" || payloadOf(t, es[0])["sender"] != "simchat:owner" {
		t.Fatalf("chat.in entries = %+v", es)
	}
	if _, err := r.run("send", `{"as":"nobody","text":"x"}`); err == nil {
		t.Fatal("an unknown persona was accepted")
	}
	if _, err := r.run("send", `{"as":"stranger","chat":"dm:owner","text":"x"}`); err == nil {
		t.Fatal("a persona spoke in another persona's DM")
	}
}

func TestSendRefusedByAllowList(t *testing.T) {
	r := newRig(t, func(c *config.SimChatConfig) { c.AllowFrom = config.FlexibleStringSlice{"simchat:owner"} })
	if res := r.mustRun("send", `{"as":"stranger","text":"let me in"}`); res["delivered"] != false {
		t.Fatalf("a refused sender's send = %v, want delivered:false", res)
	}
	if m, ok := r.inbound(); ok {
		t.Fatalf("a sender outside allow_from reached the agent: %+v", m)
	}
	r.mustRun("send", `{"as":"owner","text":"hi"}`)
	if _, ok := r.inbound(); !ok {
		t.Fatal("the allowed sender was refused")
	}
}

func TestGroupPeerKindAndOwnerInGroup(t *testing.T) {
	r := newRig(t, func(c *config.SimChatConfig) { c.GroupTrigger = config.GroupTriggerConfig{MentionOnly: true} })
	r.mustRun("send", `{"as":"owner@group:team","text":"hello all","mention":true}`)
	m, ok := r.inbound()
	if !ok || m.SenderID != "simchat:owner" || m.Peer.Kind != "group" || m.ChatID != "group:team" || m.Peer.ID != "group:team" {
		t.Fatalf("inbound = %+v (%v), want owner in group:team with peer kind group", m, ok)
	}

	// The group trigger applies as for a real channel: no mention, no agent.
	res := r.mustRun("send", `{"as":"alice","chat":"group:team","text":"just chatting"}`)
	if res["delivered"] != false {
		t.Fatalf("an unaddressed group message = %v, want delivered:false", res)
	}
	if m, ok := r.inbound(); ok {
		t.Fatalf("an unaddressed group message reached the agent: %+v", m)
	}
	if _, err := r.run("send", `{"as":"alice","chat":"group:nowhere","text":"x"}`); err == nil {
		t.Fatal("a message to an undefined group was accepted")
	}
}

// A forwarded /stop is not a control command; the router still sees it (and says no),
// and the agent gets it. The same text typed is answered by the control path, and that
// answer comes back through the channel's own Send into the outbox.
func TestForwardedNeverControl(t *testing.T) {
	r := newRig(t)
	r.router.handle = func(in channels.ControlInput) (channels.ControlReply, bool) {
		if in.Forwarded || in.Text != "/stop" {
			return channels.ControlReply{}, false
		}
		return channels.ControlReply{Text: "Stopped."}, true
	}
	r.mustRun("send", `{"as":"owner","text":"/stop","forwarded":true}`)
	m, ok := r.inbound()
	if !ok || m.Content != "/stop" {
		t.Fatalf("a forwarded /stop did not reach the agent: %+v (%v)", m, ok)
	}
	if _, has := m.Metadata[channels.MetaForwarded]; has {
		t.Errorf("the forwarded hint leaked to the agent: %v", m.Metadata)
	}
	if seen := r.router.seen(); len(seen) != 1 || !seen[0].Forwarded {
		t.Fatalf("router inputs = %+v, want one forwarded input", seen)
	}

	r.mustRun("send", `{"as":"owner","text":"/stop"}`)
	if m, ok := r.inbound(); ok {
		t.Fatalf("a typed /stop reached the agent: %+v", m)
	}
	out := r.mustRun("outbox", `{"to":"dm:owner"}`)["messages"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["text"] != "Stopped." || out[0].(map[string]any)["reply_to"] != "in-2" {
		t.Fatalf("outbox = %v, want the control path's reply to the second message", out)
	}
}

func TestReplyToCardReachesControl(t *testing.T) {
	r := newRig(t)
	r.mustRun("send", `{"as":"owner","text":"yes","reply_to":"sc-3","quote":"Buy 0.1 BTC?"}`)
	seen := r.router.seen()
	if len(seen) != 1 || seen[0].ReplyToMessageID != "sc-3" || seen[0].Text != "yes" || seen[0].PeerKind != "direct" {
		t.Fatalf("router inputs = %+v, want ReplyToMessageID sc-3 and Text = the sender's own words", seen)
	}
	m, ok := r.inbound()
	if !ok || !strings.Contains(m.Content, "Buy 0.1 BTC?") || !strings.HasSuffix(m.Content, "yes") ||
		m.Metadata["reply_to_message_id"] != "sc-3" {
		t.Fatalf("inbound = %+v (%v), want the quote in front of the reply", m, ok)
	}
}

func TestOutMsgCarriesProactiveMetadata(t *testing.T) {
	r := newRig(t)
	ids, err := r.ch.SendWithIDs(context.Background(), card("dm:owner"))
	if err != nil || len(ids) != 1 || ids[0] != "sc-1" {
		t.Fatalf("SendWithIDs = %v, %v", ids, err)
	}
	if _, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "dm:owner", Content: "plain"}); err != nil {
		t.Fatal(err)
	}
	msgs := r.mustRun("outbox", `{}`)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("outbox = %v", msgs)
	}
	c := msgs[0].(map[string]any)
	if c["id"] != "sc-1" || c["severity"] != "warning" || c["dedup_key"] != "order:BTC" || c["intervention_id"] != "I-1" ||
		c["silent"] != true || c["buttons_live"] != true || c["delivered"] != true {
		t.Fatalf("card = %v", c)
	}
	btns := c["buttons"].([]any)
	if len(btns) != 2 {
		t.Fatalf("buttons = %v", btns)
	}
	for i, want := range []string{"approve", "reject"} {
		b := btns[i].(map[string]any)
		id, payload, err := channels.ParseCallbackData(b["data"].(string), 64)
		if b["action"] != want || err != nil || id != want || payload != "AB12.nonce1" {
			t.Errorf("button %d = %v (parsed %q %q %v)", i, b, id, payload, err)
		}
	}
	if p := msgs[1].(map[string]any); p["severity"] != "info" || p["intervention_id"] != "" || p["dedup_key"] != "" || p["silent"] != false {
		t.Errorf("an ordinary reply carries proactive metadata: %v", p)
	}

	es := r.entries("chat.out")
	if len(es) != 2 || es[0].Subject != "dm:owner" {
		t.Fatalf("chat.out entries = %+v, want 2 with the chat as subject", es)
	}
	p := payloadOf(t, es[0])
	if p["sandbox"] != true || p["severity"] != "warning" || p["dedup_key"] != "order:BTC" || p["intervention_id"] != "I-1" ||
		p["silent"] != true || len(p["buttons"].([]any)) != 2 {
		t.Fatalf("chat.out payload = %v", p)
	}
}

func TestEditRemovesButtons(t *testing.T) {
	r := newRig(t)
	if _, err := r.ch.SendWithIDs(context.Background(), card("dm:owner")); err != nil {
		t.Fatal(err)
	}
	if err := r.ch.EditMessage(context.Background(), "dm:owner", "sc-9", "x"); err == nil {
		t.Fatal("editing an unknown message succeeded")
	}
	if err := r.ch.EditMessage(context.Background(), "dm:owner", "sc-1", "Rejected."); err != nil {
		t.Fatal(err)
	}
	m := r.mustRun("outbox", `{}`)["messages"].([]any)[0].(map[string]any)
	if m["text"] != "Rejected." || m["buttons_live"] != false {
		t.Fatalf("edited message = %v", m)
	}
	es := r.entries("chat.edit")
	if len(es) != 1 || es[0].Subject != "dm:owner" || payloadOf(t, es[0])["buttons_removed"] != true {
		t.Fatalf("chat.edit entries = %+v", es)
	}
}

func TestSpoofRequiresUnauthenticated(t *testing.T) {
	r := newRig(t, func(c *config.SimChatConfig) {
		c.Personas = []config.SimChatPersona{{Name: "owner"}, {Name: "stranger"}, {Name: "nick", Unauthenticated: true}}
	})
	if _, err := r.run("send", `{"as":"stranger","spoof_as":"owner","text":"/resume"}`); err == nil {
		t.Fatal("spoofing an authenticated persona was accepted")
	}
	if _, ok := r.inbound(); ok {
		t.Fatal("a refused spoof reached the bus")
	}
	r.mustRun("send", `{"as":"stranger","spoof_as":"nick","text":"/resume"}`)
	m, ok := r.inbound()
	if !ok || m.SenderID != "simchat:nick" || m.ChatID != "dm:nick" {
		t.Fatalf("spoofed inbound = %+v (%v), want the claimed persona's sender and DM", m, ok)
	}
	if es := r.entries("chat.in"); len(es) != 1 || payloadOf(t, es[0])["spoofed_by"] != "stranger" {
		t.Fatalf("chat.in entries = %+v, want spoofed_by stranger", es)
	}
}

func TestStateGates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rl := chanState{kind: stateRateLimited, perMinute: 2}
	for i := 0; i < 2; i++ {
		if _, err := rl.gate(t0); err != nil {
			t.Fatalf("send %d refused: %v", i, err)
		}
	}
	if _, err := rl.gate(t0.Add(30 * time.Second)); err == nil || !strings.Contains(err.Error(), channels.ErrRateLimit.Error()) {
		t.Fatalf("third send in a minute = %v, want rate limited", err)
	}
	if _, err := rl.gate(t0.Add(61 * time.Second)); err != nil {
		t.Fatalf("send after the minute = %v", err)
	}
	if w, err := (&chanState{kind: stateSlow, delay: time.Second}).gate(t0); w != time.Second || err != nil {
		t.Fatalf("slow = %v, %v", w, err)
	}
}

func TestFlakyIsSeeded(t *testing.T) {
	r := newRig(t)
	pattern := func() string {
		r.mustRun("set_state", `{"state":"flaky","p":0.5,"seed":7}`)
		var sb strings.Builder
		for i := 0; i < 20; i++ {
			if _, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{ChatID: "dm:owner", Content: "x"}); err != nil {
				sb.WriteByte('x')
			} else {
				sb.WriteByte('.')
			}
		}
		return sb.String()
	}
	a, b := pattern(), pattern()
	if a != b || !strings.Contains(a, "x") || !strings.Contains(a, ".") {
		t.Fatalf("flaky patterns %q and %q: want identical and mixed", a, b)
	}
	if _, err := r.run("set_state", `{"state":"flaky","p":2}`); err == nil {
		t.Fatal("p > 1 accepted")
	}
	if _, err := r.run("set_state", `{"state":"slow"}`); err == nil {
		t.Fatal("slow without a delay accepted")
	}
}

// down makes the manager retry (temporary) or give up at once (send_failed), and every
// attempt is journaled as a refused send.
func TestDownStateManagerRetries(t *testing.T) {
	r := newRig(t)
	cfg := testConfig()
	m, err := channels.NewManager(cfg, r.b, nil) // builds the real simchat via its factory
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := m.GetChannel("simchat")
	if !ok {
		t.Fatal("the manager did not build simchat")
	}
	ch.(*Channel).Attach(r.w)
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll(context.Background()) })

	send := func(content string) {
		// A proactive message, as the issue's check words it: "the next proactive send".
		msg := bus.OutboundMessage{Channel: "simchat", ChatID: "dm:owner", Content: content,
			InterventionID: "I-2", Severity: proactive.SeverityNotice, DedupKey: "k"}
		if err := r.b.PublishOutbound(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	failed := func(want int) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for {
			n := 0
			for _, e := range r.entries("chat.out") {
				if p := payloadOf(t, e); p["delivered"] == false && p["error"] != nil {
					n++
				}
			}
			if n == want {
				return
			}
			if n > want || time.Now().After(deadline) {
				t.Fatalf("refused sends journaled = %d, want %d", n, want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	r.mustRun("set_state", `{"state":"down","error":"send_failed"}`)
	send("one")
	failed(1)

	r.mustRun("set_state", `{"state":"down"}`)
	send("two")
	failed(1 + 4) // the first attempt and three retries (0.5, 1 and 2 s of backoff)
	time.Sleep(300 * time.Millisecond)
	failed(1 + 4)
	for _, e := range r.entries("chat.out") {
		if p := payloadOf(t, e); p["delivered"] != false || p["id"] != nil {
			t.Errorf("a refused send was recorded as delivered: %v", p)
		}
	}
	if len(r.entries("chat.state")) != 2 {
		t.Errorf("chat.state entries = %d, want 2", len(r.entries("chat.state")))
	}

	r.mustRun("set_state", `{"state":"up"}`)
	send("three")
	if msgs := waitOutbox(t, r, 1); msgs[0].(map[string]any)["text"] != "three" {
		t.Fatalf("outbox after up = %v", msgs)
	}
}

func waitOutbox(t *testing.T, r *rig, n int) []any {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if msgs := r.mustRun("outbox", `{}`)["messages"].([]any); len(msgs) >= n {
			return msgs
		}
	}
	t.Fatalf("outbox has fewer than %d messages", n)
	return nil
}

func TestOutboxFilters(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, to := range []string{"dm:owner", "dm:alice", "dm:owner"} {
		if _, err := r.ch.SendWithIDs(ctx, bus.OutboundMessage{ChatID: to, Content: to}); err != nil {
			t.Fatal(err)
		}
	}
	r.mustRun("set_state", `{"state":"down","error":"send_failed"}`)
	_, _ = r.ch.SendWithIDs(ctx, bus.OutboundMessage{ChatID: "dm:owner", Content: "refused"})

	count := func(args string) int { return len(r.mustRun("outbox", args)["messages"].([]any)) }
	if got := count(`{}`); got != 3 {
		t.Errorf("default outbox = %d, want 3 (refused sends are hidden)", got)
	}
	if got := count(`{"include_failed":true}`); got != 4 {
		t.Errorf("with failed = %d, want 4", got)
	}
	if got := count(`{"to":"dm:owner"}`); got != 2 {
		t.Errorf("to dm:owner = %d, want 2", got)
	}
	if got := count(`{"limit":1}`); got != 1 {
		t.Errorf("limit 1 = %d", got)
	}
	res := r.mustRun("outbox", `{"after_seq":1}`)
	if got := len(res["messages"].([]any)); got != 2 || res["next_seq"].(float64) != 3 {
		t.Errorf("after_seq 1 = %v, want 2 messages and next_seq 3", res)
	}
}

// A restart keeps the journal; the next message must not reuse the ID of a card that
// was stored before it (approval matches a card by ID).
func TestWorldResumesMessageCounter(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.ch.SendWithIDs(ctx, bus.OutboundMessage{ChatID: "dm:owner", Content: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	r.mustRun("set_state", `{"state":"down","error":"send_failed"}`)
	for i := 0; i < 3; i++ { // refused sends have no ID
		_, _ = r.ch.SendWithIDs(ctx, bus.OutboundMessage{ChatID: "dm:owner", Content: "x"})
	}
	w2 := NewWorld(r.j, time.Now, config.SimChatConfig{})
	if w2.nextID != 2 {
		t.Fatalf("resumed counter = %d, want 2", w2.nextID)
	}
	if NewWorld(nil, time.Now, config.SimChatConfig{}).nextID != 0 {
		t.Fatal("a world with no journal does not start at 0")
	}
}

// An old generation stopping late must not undo the reload's attachment.
func TestStopOfOldChannelKeepsNewAttachment(t *testing.T) {
	r := newRig(t)
	cfg := testConfig()
	next, err := New(cfg, r.b)
	if err != nil {
		t.Fatal(err)
	}
	next.Attach(r.w)
	if err := next.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.ch.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.w.Attached() != next {
		t.Fatal("the old channel's Stop detached the new one")
	}
	if _, err := r.run("send", `{"as":"owner","text":"after reload"}`); err != nil {
		t.Fatal(err)
	}
	if err := next.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var le *lab.Error
	if _, err := r.run("send", `{"as":"owner","text":"x"}`); err == nil || !asLabError(err, &le) || le.Code != lab.CodeUnavailable {
		t.Fatalf("send with no running channel = %v, want unavailable", err)
	}
}

func asLabError(err error, target **lab.Error) bool {
	e, ok := err.(*lab.Error)
	*target = e
	return ok
}
