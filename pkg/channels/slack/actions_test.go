package slack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

var _ channels.ActionRenderer = (*SlackChannel)(nil)

func TestSlack_SupportsActions(t *testing.T) {
	assert.True(t, (&SlackChannel{}).SupportsActions())
}

// ---------------------------------------------------------------------------
// Sending: the buttons.
// ---------------------------------------------------------------------------

func TestSlack_ActionsBlockPayloadFormat(t *testing.T) {
	r := newRig(t, nil, nil)
	ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
		ChatID: ownerDM, Content: "Sell 0.01 BTC?", Actions: approvalActions(),
	})
	require.NoError(t, err)
	require.Len(t, ids, 1)

	posts := r.api.callsTo("chat.postMessage")
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].Form.Get("text"), "/approve 7F3K", "the text fallback stays in the body (and is the notification)")
	blocks := posts[0].blocks(t)
	require.Len(t, blocks, 2, "a section and an actions block")
	assert.Equal(t, "section", blocks[0]["type"])
	text := blocks[0]["text"].(map[string]any)
	assert.Equal(t, "mrkdwn", text["type"])
	assert.Contains(t, text["text"], "Sell 0.01 BTC?")
	assert.Contains(t, text["text"], "/reject 7F3K")

	assert.Equal(t, "actions", blocks[1]["type"])
	assert.Equal(t, "kq1_actions", blocks[1]["block_id"])
	elems := blocks[1]["elements"].([]any)
	require.Len(t, elems, 2)
	approve, reject := elems[0].(map[string]any), elems[1].(map[string]any)
	assert.Equal(t, "button", approve["type"])
	assert.Equal(t, "kq1_apv", approve["action_id"])
	assert.Equal(t, approveVal, approve["value"], "the value is the whole codec string")
	assert.Equal(t, "primary", approve["style"])
	assert.Equal(t, "Approve", approve["text"].(map[string]any)["text"])
	assert.Equal(t, "plain_text", approve["text"].(map[string]any)["type"])
	assert.Equal(t, "kq1_rej", reject["action_id"])
	assert.Equal(t, rejectVal, reject["value"])
	assert.Equal(t, "danger", reject["style"])
}

func TestSlack_ButtonLimits(t *testing.T) {
	// The longest string the codec makes fits Slack's limits.
	for id := range channels.CallbackCodes() {
		payload := strings.Repeat("a", bus.MaxActionPayloadBytes)
		if id == "approve" || id == "reject" {
			payload = "7F3K." + strings.Repeat("a", bus.MaxActionPayloadBytes-5)
		}
		blk, err := buildActionsBlock([]bus.Action{{ID: id, Label: strings.Repeat("é", 200), Payload: payload}})
		require.NoError(t, err, id)
		btn := blk.Elements.ElementSet[0].(*slack.ButtonBlockElement)
		assert.LessOrEqual(t, len(btn.Value), maxButtonValueBytes, id)
		assert.LessOrEqual(t, len(btn.ActionID), maxActionIDBytes, id)
		assert.LessOrEqual(t, len([]rune(btn.Text.Text)), maxButtonTextRunes, id)
	}

	// Over the limit is an error, never a truncation.
	_, err := channels.EncodeCallbackData(bus.Action{ID: "approve", Payload: "7F3K.abcd1234"}, 20)
	assert.Error(t, err)
	_, err = buildActionsBlock(make([]bus.Action, maxActionsPerBlock+1))
	assert.Error(t, err)
	_, err = buildActionsBlock(append(approvalActions(), approvalActions()[0]))
	assert.Error(t, err, "Slack refuses an actions block whose action_ids repeat")

	// An action that cannot be encoded: the message goes out with no buttons, and
	// its text fallback is still in the body.
	r := newRig(t, nil, nil)
	for name, bad := range map[string]bus.Action{
		"payload over 48 bytes": {ID: "approve", Label: "Approve", Payload: "7F3K." + strings.Repeat("a", 60), TextCommand: "/approve 7F3K"},
		"unknown action":        {ID: "explode", Label: "Boom", Payload: "x", TextCommand: "/explode"},
		"bad payload byte":      {ID: "snooze_1h", Label: "Snooze", Payload: "a b", TextCommand: "/snooze 1h"},
	} {
		before := len(r.api.callsTo("chat.postMessage"))
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: "alert", Actions: append(approvalActions(), bad),
		})
		require.NoError(t, err, name)
		posts := r.api.callsTo("chat.postMessage")
		require.Len(t, posts, before+1, name)
		last := posts[len(posts)-1]
		assert.Empty(t, last.Form.Get("blocks"), name)
		assert.Contains(t, last.Form.Get("text"), "/approve 7F3K", name)
	}
}

func TestSlack_ButtonsOnLastMessageOnly(t *testing.T) {
	r := newRig(t, nil, nil)

	t.Run("long text is laid out in sections of at most 3000 characters", func(t *testing.T) {
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: strings.Repeat("word ", 1400), Actions: approvalActions(),
		})
		require.NoError(t, err)
		posts := r.api.callsTo("chat.postMessage")
		blocks := posts[len(posts)-1].blocks(t)
		require.GreaterOrEqual(t, len(blocks), 3)
		for _, b := range blocks[:len(blocks)-1] {
			assert.Equal(t, "section", b["type"])
			assert.LessOrEqual(t, len([]rune(b["text"].(map[string]any)["text"].(string))), maxSectionTextRunes)
		}
		assert.Equal(t, "actions", blocks[len(blocks)-1]["type"])
	})

	t.Run("a footer that pushes the text past the limit makes a second message; only it has buttons", func(t *testing.T) {
		before := len(r.api.callsTo("chat.postMessage"))
		ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: strings.Repeat("word ", 7998), Actions: approvalActions(),
		})
		require.NoError(t, err)
		posts := r.api.callsTo("chat.postMessage")[before:]
		require.Len(t, posts, 2)
		require.Len(t, ids, 2)
		assert.Empty(t, posts[0].Form.Get("blocks"))
		assert.Contains(t, posts[1].Form.Get("blocks"), `"kq1_actions"`)
	})

	t.Run("a message without actions has none", func(t *testing.T) {
		before := len(r.api.callsTo("chat.postMessage"))
		require.NoError(t, r.ch.Send(context.Background(), bus.OutboundMessage{ChatID: ownerDM, Content: "plain"}))
		posts := r.api.callsTo("chat.postMessage")
		require.Len(t, posts, before+1)
		assert.Empty(t, posts[before].Form.Get("blocks"))
	})
}

// ---------------------------------------------------------------------------
// Taps.
// ---------------------------------------------------------------------------

func TestSlack_BlockActionBuildsTheControlInput(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "ok", AnswerCallback: "ok"}, true
	}}
	r := newRig(t, nil, router)

	r.press(tap{user: ownerID, value: "kq1|snz4h|01HZ"})
	r.press(tap{user: ownerID, channel: publicChan, value: "kq1|use|01HZ"})
	r.press(tap{user: ownerID, channel: publicChan, value: "kq1|nu|01HZ", threadTS: "1699999999.000001"})
	// The parent of a thread reports its own ts as thread_ts: that is not a thread
	// to answer into.
	r.press(tap{user: ownerID, value: "kq1|nu|01HZ", threadTS: cardTS})

	in := router.inputs()
	require.Len(t, in, 4)
	assert.Equal(t, "slack", in[0].Channel)
	assert.Equal(t, ownerDM, in[0].ChatID)
	assert.Equal(t, "slack:"+ownerID, in[0].SenderID)
	assert.Equal(t, "slack:"+ownerID, in[0].Sender.CanonicalID)
	assert.Equal(t, "direct", in[0].PeerKind)
	assert.Equal(t, cardTS, in[0].MessageID, "the message ts, which is what SendWithIDs reports")
	assert.Equal(t, cardTS, in[0].ReplyToMessageID)
	assert.Empty(t, in[0].Text)
	require.NotNil(t, in[0].Callback)
	assert.Equal(t, channels.ControlCallback{Action: "snooze_4h", Payload: "01HZ", CallbackID: "trg-1"}, *in[0].Callback)

	assert.Equal(t, publicChan, in[1].ChatID)
	assert.Equal(t, "channel", in[1].PeerKind, "a channel that is not a DM is a group")
	assert.Equal(t, publicChan+"/1699999999.000001", in[2].ChatID, "channel/thread, as the message path forms it")
	assert.Equal(t, ownerDM, in[3].ChatID)
}

func TestSlack_BlockActionRoutesToControl(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, value: approveVal})

	got := appr.decisions()
	require.Len(t, got, 1)
	assert.Equal(t, decision{Code: "7F3K", Nonce: "abcd1234", Sender: "slack:" + ownerID, Approve: true}, got[0])
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))

	eph := r.lastEphemeral()
	assert.Equal(t, ownerDM, eph.Form.Get("channel"))
	assert.Equal(t, ownerID, eph.Form.Get("user"), "only the tapper sees it")
	assert.Contains(t, eph.Form.Get("text"), "Approved proposal 7F3K")

	// Reject takes the same road.
	appr2 := newFakeApprover("9ZZZ", "n0nce000")
	r2 := newRigWithApprover(t, appr2, nil)
	r2.press(tap{user: ownerID, value: "kq1|rej|9ZZZ.n0nce000"})
	require.Len(t, appr2.decisions(), 1)
	assert.False(t, appr2.decisions()[0].Approve)
	assert.Contains(t, r2.lastEphemeral().Form.Get("text"), "Rejected proposal 9ZZZ")
}

func TestSlack_BlockActionNonOwnerDenied(t *testing.T) {
	t.Run("allow-listed, but not the owner", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, []string{ownerID, otherID})
		r.press(tap{user: otherID, value: approveVal})
		assert.Equal(t, "Not authorized.", r.lastEphemeral().Form.Get("text"))
		assert.Empty(t, appr.decisions(), "the approval service was never asked")
		assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
		assert.Empty(t, r.api.callsTo("chat.update"))
	})

	t.Run("not on the allow-list at all", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "should not happen"}, true
		}}
		r := newRig(t, []string{ownerID}, router)
		// Valid data, malformed data and garbage all get the same answer.
		for _, v := range []string{approveVal, "kq1|bogus|x", "junk", ""} {
			r.press(tap{user: otherID, value: v, actionID: "kq1_apv"})
			assert.Equal(t, "Not authorized.", r.lastEphemeral().Form.Get("text"), "value %q", v)
			assert.Equal(t, otherID, r.lastEphemeral().Form.Get("user"))
		}
		assert.Empty(t, router.inputs(), "the control path never saw it")
		assert.Equal(t, 4, r.api.total(), "nothing but the four refusals")
	})
}

func TestSlack_ForgedPayloadIsRefusedByDecide(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, value: "kq1|apv|7F3K.deadbeef"})
	require.Len(t, appr.decisions(), 1, "it reached Decide, which is where it is refused")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, r.lastEphemeral().Form.Get("text"), "No pending proposal 7F3K")
}

func TestSlack_ChannelTapFollowsTheGroupRule(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	// The owner, tapping in a public channel.
	r.press(tap{user: ownerID, channel: publicChan, value: approveVal})
	assert.Empty(t, appr.decisions(), "an owner command does not work outside the owner chat")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, r.lastEphemeral().Form.Get("text"), "Owner commands only work in your owner chat")
}

func TestSlack_EditOriginalUpdatesTheCard(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{
			Text: "Approved proposal 7F3K: sell 0.01 BTC on okx", EditOriginal: true, AnswerCallback: "Approved",
		}, true
	}}
	r := newRig(t, nil, router)
	r.press(tap{user: ownerID, value: approveVal})

	upd := r.api.callsTo("chat.update")
	require.Len(t, upd, 1)
	assert.Equal(t, ownerDM, upd[0].Form.Get("channel"))
	assert.Equal(t, cardTS, upd[0].Form.Get("ts"))
	assert.Contains(t, upd[0].Form.Get("text"), "Approved proposal 7F3K")
	assert.Equal(t, "[]", upd[0].Form.Get("blocks"), "an empty block list removes the buttons")
	assert.Empty(t, r.api.callsTo("chat.postEphemeral"), "the card was edited, not answered beside")

	t.Run("a card that cannot be updated gets the outcome as an ephemeral message", func(t *testing.T) {
		r := newRig(t, nil, router)
		r.api.failUpdate = true
		r.press(tap{user: ownerID, value: approveVal})
		assert.Contains(t, r.lastEphemeral().Form.Get("text"), "Approved proposal 7F3K")
	})
}

// Whatever happens to a tap, it is answered, once.
func TestSlack_EveryBlockActionIsAnswered(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	handling := &scriptedRouter{}
	handling.fn = func(in channels.ControlInput) (channels.ControlReply, bool) {
		switch in.Callback.Action {
		case "snooze_1h":
			panic("boom")
		case "useful":
			return channels.ControlReply{}, false // a router that does not know it
		}
		return channels.ControlReply{Text: "fine", AnswerCallback: "fine"}, true
	}
	cases := []struct {
		name   string
		tap    tap
		want   string // a prefix
		router func() channels.ControlRouter
		allow  []string
	}{
		{"handled", tap{user: ownerID, value: "kq1|snz4h|x"}, "fine", nil, nil},
		{"router panics", tap{user: ownerID, value: "kq1|snz1h|x"}, "That command could not be handled", nil, nil},
		{"router does not know it", tap{user: ownerID, value: "kq1|use|x"}, "That button is not valid.", nil, nil},
		{"unknown version", tap{user: ownerID, value: "kq9|apv|7F3K.abcd1234", actionID: "kq1_apv"}, "That button is not valid.", nil, nil},
		{"unknown action", tap{user: ownerID, value: "kq1|zzz|x", actionID: "kq1_zzz"}, "That button is not valid.", nil, nil},
		{"malformed payload", tap{user: ownerID, value: "kq1|apv|7F3K"}, "That button is not valid.", nil, nil},
		{"no value", tap{user: ownerID, value: "", actionID: "kq1_apv"}, "That button is not valid.", nil, nil},
		{"action_id does not match the value", tap{user: ownerID, value: approveVal, actionID: "kq1_rej"}, "That button is not valid.", nil, nil},
		{"not allow-listed", tap{user: otherID, value: "kq1|snz4h|x"}, "Not authorized.", nil, []string{ownerID}},
		{"no router", tap{user: ownerID, value: "kq1|snz4h|x"}, "Buttons are not available right now.", func() channels.ControlRouter { return nil }, nil},
		{"real dispatcher, bad nonce", tap{user: ownerID, value: "kq1|apv|7F3K.00000000"}, "No pending proposal", func() channels.ControlRouter {
			return control.New(control.Deps{Cfg: ownerConfig(), Approvals: appr})
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var router channels.ControlRouter = handling
			if tc.router != nil {
				router = tc.router()
			}
			r := newRig(t, tc.allow, router)
			r.press(tc.tap)
			eph := r.api.callsTo("chat.postEphemeral")
			require.Len(t, eph, 1, "exactly one answer")
			assert.True(t, strings.HasPrefix(eph[0].Form.Get("text"), tc.want), "text %q, want prefix %q", eph[0].Form.Get("text"), tc.want)
			select {
			case m := <-r.mb.InboundChan():
				t.Fatalf("a tap became an inbound message: %+v", m)
			default:
			}
		})
	}

	t.Run("a block action of another app's button is not ours", func(t *testing.T) {
		router := &scriptedRouter{}
		r := newRig(t, nil, router)
		r.press(tap{user: ownerID, value: "x", actionID: "other_button"})
		assert.Zero(t, r.api.total())
		assert.Empty(t, router.inputs())
	})
}

// The socket-mode envelope is acknowledged at once, before the control path has
// answered, whatever the event is.
func TestSlack_InteractiveEventsAreAcknowledged(t *testing.T) {
	release := make(chan struct{})
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		<-release
		return channels.ControlReply{Text: "late"}, true
	}}
	r := newRig(t, nil, router)

	ev := socketmode.Event{
		Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{EnvelopeID: "env-1"},
		Data: tap{user: ownerID, value: approveVal}.callback(t),
	}
	r.ch.handleInteractive(ev)
	assert.Equal(t, []string{"env-1"}, r.ackList(), "acknowledged before the control path answered")
	assert.Zero(t, r.api.total())
	close(release)
	eventually(t, "the answer", func() bool { return len(r.api.callsTo("chat.postEphemeral")) == 1 })

	// An interactive event that is not a block action is acknowledged and dropped.
	r.ch.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{EnvelopeID: "env-2"},
		Data: slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission},
	})
	r.ch.handleInteractive(socketmode.Event{Type: socketmode.EventTypeInteractive, Request: &socketmode.Request{EnvelopeID: "env-3"}})
	assert.Equal(t, []string{"env-1", "env-2", "env-3"}, r.ackList())
	assert.Len(t, router.inputs(), 1)
}

func TestSlack_DoubleTapApprovesOnce(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, value: approveVal})
	r.press(tap{user: ownerID, value: approveVal})
	r.press(tap{user: ownerID, value: rejectVal})

	require.Len(t, appr.decisions(), 3)
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"), "the first decision stands")
	eph := r.api.callsTo("chat.postEphemeral")
	require.Len(t, eph, 3)
	assert.Contains(t, eph[0].Form.Get("text"), "Approved proposal 7F3K")
	assert.Contains(t, eph[1].Form.Get("text"), "already approved")
	assert.Contains(t, eph[2].Form.Get("text"), "already approved")
}

// ---------------------------------------------------------------------------
// No model ever sees a tap.
// ---------------------------------------------------------------------------

// The channel and a real AgentLoop share one bus. Taps of every kind (handled,
// refused, malformed, from a stranger) publish nothing for the agent, and the
// model is never called; the same channel does feed the agent an ordinary
// message, so the check can fail.
func TestSlack_NoModelEverSeesABlockAction(t *testing.T) {
	mb := bus.NewMessageBus()
	prov := &countingProvider{}
	al := agent.NewAgentLoop(&config.Config{
		Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
			Workspace: t.TempDir(), Model: "count-model", MaxTokens: 4096, MaxToolIterations: 5,
		}},
		Session: config.SessionConfig{DMScope: "per-channel-peer"},
	}, mb, prov)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = al.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the agent loop did not stop")
		}
		al.Stop()
		al.Close()
	})

	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWith(t, mb, []string{ownerID, otherID}, nil)
	r.ch.SetControlRouter(control.New(control.Deps{Cfg: r.cfg, Approvals: appr}))

	for _, tp := range []tap{
		{user: ownerID, value: approveVal},                          // handled
		{user: ownerID, value: approveVal},                          // already decided
		{user: otherID, value: approveVal},                          // allow-listed, not the owner
		{user: "U0STRANGER", value: approveVal},                     // a stranger
		{user: ownerID, value: "kq1|zzz|7F3K.abcd1234"},             // unknown action
		{user: ownerID, value: "kq1|apv|7F3K"},                      // malformed
		{user: ownerID, value: "kq1|snz1h|7F3K abcd1234"},           // junk payload
		{user: ownerID, value: approveVal, channel: publicChan},     // in a channel
		{user: ownerID, value: "x", actionID: "somebody_elses_btn"}, // not ours
	} {
		r.press(tp)
	}
	time.Sleep(200 * time.Millisecond) // a (wrongly) queued message would be picked up by now

	calls, seen := prov.snapshot()
	assert.Zero(t, calls, "the model was called for a tap")
	assert.NotContains(t, seen, "7F3K")
	assert.Empty(t, mb.InboundChan(), "a tap was published as an inbound message")

	// Positive control: the same wiring does hand an ordinary message to the agent.
	r.ch.handleMessageEvent(&slackevents.MessageEvent{
		User: ownerID, Channel: ownerDM, Text: "how is the market today?", TimeStamp: "1700000001.000200",
	})
	eventually(t, "the model to be called for an ordinary message", func() bool { n, _ := prov.snapshot(); return n > 0 })
	n, seen := prov.snapshot()
	assert.Equal(t, 1, n)
	assert.Contains(t, seen, "how is the market today?")
}
