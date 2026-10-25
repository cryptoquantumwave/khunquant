package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

var (
	_ channels.ActionRenderer    = (*LINEChannel)(nil)
	_ channels.IDReportingSender = (*LINEChannel)(nil)
)

func TestLINE_SupportsActions(t *testing.T) {
	assert.True(t, (&LINEChannel{}).SupportsActions())
}

// ---------------------------------------------------------------------------
// Sending: the quick-reply buttons.
// ---------------------------------------------------------------------------

func TestLINE_QuickReplyPayloadFormat(t *testing.T) {
	qr, err := buildQuickReply(approvalActions())
	require.NoError(t, err)
	b, err := json.Marshal(qr)
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":[
		{"type":"action","action":{"type":"postback","label":"Approve","data":"kq1|apv|7F3K.abcd1234","displayText":"Approve"}},
		{"type":"action","action":{"type":"postback","label":"Reject","data":"kq1|rej|7F3K.abcd1234","displayText":"Reject"}}]}`, string(b))

	t.Run("a label is cut to LINE's 20 characters", func(t *testing.T) {
		qr, err := buildQuickReply([]bus.Action{{ID: "snooze_1h", Label: strings.Repeat("é", 40), Payload: "01HZX3K8V0F9Q2M4N6P8R0S2T4"}})
		require.NoError(t, err)
		act := qr["items"].([]map[string]any)[0]["action"].(map[string]any)
		assert.Len(t, []rune(act["label"].(string)), maxQuickReplyLabelRunes)
	})
}

func TestLINE_PostbackDataLimit(t *testing.T) {
	// The longest string the codec makes (every action, a full-length payload)
	// fits LINE's 300 bytes with room to spare.
	longest := 0
	for id := range channels.CallbackCodes() {
		payload := strings.Repeat("a", bus.MaxActionPayloadBytes)
		if id == "approve" || id == "reject" {
			payload = "7F3K." + strings.Repeat("a", bus.MaxActionPayloadBytes-5)
		}
		data, err := channels.EncodeCallbackData(bus.Action{ID: id, Payload: payload}, maxPostbackDataBytes)
		require.NoError(t, err, id)
		longest = max(longest, len(data))
	}
	assert.LessOrEqual(t, longest, maxPostbackDataBytes)

	// Over the limit is an error, never a truncation.
	_, err := channels.EncodeCallbackData(bus.Action{ID: "approve", Payload: "7F3K.abcd1234"}, 20)
	assert.Error(t, err)
	_, err = buildQuickReply(make([]bus.Action, maxQuickReplyItems+1))
	assert.Error(t, err)

	// An action that cannot be encoded: the message goes out with no buttons, and
	// its text fallback is still in the body.
	r := newRig(t, nil, nil)
	for name, bad := range map[string]bus.Action{
		"payload over 48 bytes": {ID: "approve", Label: "Approve", Payload: "7F3K." + strings.Repeat("a", 60), TextCommand: "/approve 7F3K"},
		"unknown action":        {ID: "explode", Label: "Boom", Payload: "x", TextCommand: "/explode"},
		"bad payload byte":      {ID: "snooze_1h", Label: "Snooze", Payload: "a b", TextCommand: "/snooze 1h"},
	} {
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerID, Content: "alert", Actions: append(approvalActions(), bad),
		})
		require.NoError(t, err, name)
		call := r.api.callsOf("push")
		last := call[len(call)-1].messages()
		require.Len(t, last, 1, name)
		assert.NotContains(t, last[0], "quickReply", name)
		assert.Contains(t, last[0]["text"], "/approve 7F3K", name)
	}
}

func TestLINE_QuickReplyOnLastMessageOnly(t *testing.T) {
	r := newRig(t, nil, nil)

	t.Run("one message", func(t *testing.T) {
		ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerID, Content: "Sell 0.01 BTC?", Actions: approvalActions(),
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"M1"}, ids, "the ID LINE gave the message")
		msgs := r.api.callsOf("push")[0].messages()
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0], "quickReply")
		assert.Contains(t, msgs[0]["text"], "/approve 7F3K", "the text fallback stays in the body")
	})

	t.Run("a footer that pushes the text past the limit makes a second message; only it has buttons", func(t *testing.T) {
		before := len(r.api.callsOf("push"))
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerID, Content: strings.Repeat("word ", 996), Actions: approvalActions(),
		})
		require.NoError(t, err)
		msgs := r.api.callsOf("push")[before].messages()
		require.GreaterOrEqual(t, len(msgs), 2)
		for i, m := range msgs {
			if i < len(msgs)-1 {
				assert.NotContains(t, m, "quickReply", "message %d", i)
			}
		}
		assert.Contains(t, msgs[len(msgs)-1], "quickReply")
		assert.True(t, strings.HasSuffix(msgs[len(msgs)-1]["text"].(string), "/reject 7F3K"))
	})

	t.Run("a message without actions has none", func(t *testing.T) {
		before := len(r.api.callsOf("push"))
		err := r.ch.Send(context.Background(), bus.OutboundMessage{ChatID: ownerID, Content: "plain"})
		require.NoError(t, err)
		assert.NotContains(t, r.api.callsOf("push")[before].messages()[0], "quickReply")
	})
}

// ---------------------------------------------------------------------------
// Taps.
// ---------------------------------------------------------------------------

func TestLINE_PostbackBuildsTheControlInput(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "ok", AnswerCallback: "ok"}, true
	}}
	r := newRig(t, nil, router)

	r.press(tap{user: ownerID, data: "kq1|snz4h|01HZ"})
	r.press(tap{user: ownerID, srcType: "group", groupID: groupID, data: "kq1|use|01HZ"})
	r.press(tap{user: ownerID, srcType: "room", groupID: "Rroom", data: "kq1|nu|01HZ"})

	in := router.inputs()
	require.Len(t, in, 3)
	assert.Equal(t, "line", in[0].Channel)
	assert.Equal(t, ownerID, in[0].ChatID, "a 1:1 chat is addressed by the user id, as the message path does")
	assert.Equal(t, "line:"+ownerID, in[0].SenderID)
	assert.Equal(t, "line:"+ownerID, in[0].Sender.CanonicalID)
	assert.Equal(t, "direct", in[0].PeerKind)
	assert.Empty(t, in[0].Text)
	require.NotNil(t, in[0].Callback)
	assert.Equal(t, channels.ControlCallback{Action: "snooze_4h", Payload: "01HZ", CallbackID: "evt-1"}, *in[0].Callback)

	assert.Equal(t, groupID, in[1].ChatID)
	assert.Equal(t, "group", in[1].PeerKind)
	assert.Equal(t, "Rroom", in[2].ChatID)
	assert.Equal(t, "group", in[2].PeerKind, "a multi-person room is a group")
}

func TestLINE_PostbackRoutesToControl(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)

	r.press(tap{user: ownerID, data: approveData})

	got := appr.decisions()
	require.Len(t, got, 1)
	assert.Equal(t, decision{Code: "7F3K", Nonce: "abcd1234", Sender: "line:" + ownerID, Approve: true}, got[0])
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))

	reply := r.api.callsOf("reply")
	require.Len(t, reply, 1, "answered once, with the event's reply token")
	assert.Equal(t, "rt-1", reply[0].Body["replyToken"])
	assert.Contains(t, reply[0].text(), "Approved proposal 7F3K")

	// Reject takes the same road.
	appr2 := newFakeApprover("9ZZZ", "n0nce000")
	r2 := newRigWithApprover(t, appr2, nil)
	r2.press(tap{user: ownerID, data: "kq1|rej|9ZZZ.n0nce000"})
	require.Len(t, appr2.decisions(), 1)
	assert.False(t, appr2.decisions()[0].Approve)
	assert.Contains(t, r2.lastReplyText(), "Rejected proposal 9ZZZ")
}

const approveData = approveDt

func TestLINE_PostbackNonOwnerDenied(t *testing.T) {
	t.Run("allow-listed, but not the owner", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, []string{ownerID, otherID})
		r.press(tap{user: otherID, data: approveData})

		assert.Equal(t, "Not authorized.", r.lastReplyText())
		assert.Empty(t, appr.decisions(), "the approval service was never asked")
		assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	})

	t.Run("not on the allow-list at all: nothing is said, nothing is routed", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "should not happen"}, true
		}}
		r := newRig(t, []string{ownerID}, router)
		// Valid data, malformed data and garbage all get the same silence.
		for _, data := range []string{approveData, "kq1|bogus|x", "junk", ""} {
			r.press(tap{user: otherID, data: data})
		}
		assert.Empty(t, router.inputs(), "the control path never saw it")
		assert.Zero(t, r.api.total(), "LINE has no toast; a stranger gets no reply")
	})

	t.Run("a postback with no user id has nobody to hold to the owner rule", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "should not happen"}, true
		}}
		r := newRig(t, nil, router)
		r.press(tap{user: "", srcType: "group", groupID: groupID, data: approveData})
		assert.Empty(t, router.inputs())
		assert.Zero(t, r.api.total())
	})
}

func TestLINE_ForgedPayloadIsRefusedByDecide(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, data: "kq1|apv|7F3K.deadbeef"})
	require.Len(t, appr.decisions(), 1, "it reached Decide, which is where it is refused")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, r.lastReplyText(), "No pending proposal 7F3K")
}

func TestLINE_PostbackInGroupFollowsTheGroupRule(t *testing.T) {
	for _, src := range []string{"group", "room"} {
		t.Run(src, func(t *testing.T) {
			appr := newFakeApprover("7F3K", "abcd1234")
			r := newRigWithApprover(t, appr, nil)
			r.press(tap{user: ownerID, srcType: src, groupID: groupID, data: approveData})
			assert.Empty(t, appr.decisions(), "an owner command does not work outside the owner chat")
			assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
			assert.Contains(t, r.lastReplyText(), "Owner commands only work in your owner chat")
			// The reply went to the same group.
			assert.Equal(t, "rt-1", r.api.callsOf("reply")[0].Body["replyToken"])
		})
	}
}

func TestLINE_DoubleTapApprovesOnce(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, data: approveData})
	r.press(tap{user: ownerID, data: approveData})
	r.press(tap{user: ownerID, data: rejectDt})

	require.Len(t, appr.decisions(), 3)
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"), "the first decision stands")
	replies := r.api.callsOf("reply")
	require.Len(t, replies, 3)
	assert.Contains(t, replies[0].text(), "Approved proposal 7F3K")
	assert.Contains(t, replies[1].text(), "already approved")
	assert.Contains(t, replies[2].text(), "already approved")
}

func TestLINE_EditOriginalBecomesAShortReply(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{
			Text: "Approved proposal 7F3K: sell 0.01 BTC on okx\nPlacing the order now.", EditOriginal: true,
			AnswerCallback: "Approved",
		}, true
	}}
	r := newRig(t, nil, router)
	r.press(tap{user: ownerID, data: approveData})
	assert.Equal(t, "Approved proposal 7F3K: sell 0.01 BTC on okx", r.lastReplyText(), "LINE cannot edit: the outcome line")
	assert.Len(t, r.api.callsOf("push"), 0)
}

// Whatever happens to a tap from somebody allowed to use the bot, it is answered,
// once, in the chat it came from. (Somebody who is not allowed gets no answer at
// all: see TestLINE_PostbackNonOwnerDenied.)
func TestLINE_EveryPostbackIsAnswered(t *testing.T) {
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
		data   string
		want   string
		router channels.ControlRouter
	}{
		{"handled", "kq1|snz4h|x", "fine", handling},
		{"router panics", "kq1|snz1h|x", "That command could not be handled", handling},
		{"router does not know it", "kq1|use|x", "That button is not valid.", handling},
		{"unknown version", "kq9|apv|7F3K.abcd1234", "That button is not valid.", handling},
		{"unknown action", "kq1|zzz|x", "That button is not valid.", handling},
		{"malformed payload", "kq1|apv|7F3K", "That button is not valid.", handling},
		{"no data", "", "That button is not valid.", handling},
		{"data too long", "kq1|snz1h|" + strings.Repeat("a", 300), "That button is not valid.", handling},
		{"no router", "kq1|snz4h|x", "Buttons are not available right now.", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil, tc.router)
			r.press(tap{user: ownerID, data: tc.data})
			replies := r.api.callsOf("reply")
			require.Len(t, replies, 1, "exactly one answer")
			assert.Equal(t, "rt-1", replies[0].Body["replyToken"])
			assert.True(t, strings.HasPrefix(replies[0].text(), tc.want), "reply %q, want prefix %q", replies[0].text(), tc.want)
			select {
			case m := <-r.mb.InboundChan():
				t.Fatalf("a tap became an inbound message: %+v", m)
			default:
			}
		})
	}

	t.Run("a reply token LINE no longer accepts: the answer is pushed", func(t *testing.T) {
		r := newRig(t, nil, handling)
		r.api.failReply = true
		r.press(tap{user: ownerID, srcType: "group", groupID: groupID, data: "kq1|snz4h|x"})
		push := r.api.callsOf("push")
		require.Len(t, push, 1)
		assert.Equal(t, groupID, push[0].Body["to"])
		assert.Equal(t, "fine", push[0].text())
	})

	t.Run("no reply token: the answer is pushed", func(t *testing.T) {
		r := newRig(t, nil, handling)
		r.press(tap{user: ownerID, data: "kq1|snz4h|x", noReplyToken: true})
		assert.Len(t, r.api.callsOf("reply"), 0)
		assert.Len(t, r.api.callsOf("push"), 1)
	})
}

// LINE does not say which message carried a button; quick replies show on the
// newest message of a chat only, so the latest one sent with buttons is the card.
func TestLINE_SnoozeTapCarriesTheCardMessageID(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "ok"}, true
	}}
	r := newRig(t, nil, router)
	ctx := context.Background()

	_, err := r.ch.SendWithIDs(ctx, bus.OutboundMessage{
		ChatID: ownerID, Content: "BTC moved", Actions: []bus.Action{
			{ID: "snooze_1h", Label: "Snooze 1h", Payload: "01HZX3K8V0F9Q2M4N6P8R0S2T4", TextCommand: "/snooze 1h"},
		},
	})
	require.NoError(t, err)
	r.press(tap{user: ownerID, data: "kq1|snz1h|01HZX3K8V0F9Q2M4N6P8R0S2T4"})
	in := router.inputs()
	require.Len(t, in, 1)
	assert.Equal(t, "M1", in[0].ReplyToMessageID, "the ID LINE gave the card")
	assert.Equal(t, "M1", in[0].MessageID)

	// Another message in the chat takes the buttons away; a stale tap has no card.
	err = r.ch.Send(ctx, bus.OutboundMessage{ChatID: ownerID, Content: "later"})
	require.NoError(t, err)
	r.press(tap{user: ownerID, data: "kq1|snz1h|01HZX3K8V0F9Q2M4N6P8R0S2T4"})
	in = router.inputs()
	require.Len(t, in, 2)
	assert.Empty(t, in[1].ReplyToMessageID)
}

// ---------------------------------------------------------------------------
// The webhook.
// ---------------------------------------------------------------------------

func signed(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestLINE_WebhookSignatureCoversPostbackEvents(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "fine"}, true
	}}
	r := newRig(t, nil, router)
	body, err := json.Marshal(map[string]any{"events": []any{map[string]any{
		"type": "postback", "replyToken": "rt-w", "webhookEventId": "evt-w",
		"source":   map[string]any{"type": "user", "userId": ownerID},
		"postback": map[string]any{"data": "kq1|snz4h|x"},
	}}})
	require.NoError(t, err)

	post := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/line", bytes.NewReader(body))
		req.Header.Set("X-Line-Signature", sig)
		rec := httptest.NewRecorder()
		r.ch.ServeHTTP(rec, req)
		return rec.Code
	}

	assert.Equal(t, http.StatusForbidden, post(signed("not-the-secret", body)))
	assert.Equal(t, http.StatusForbidden, post(""))
	time.Sleep(100 * time.Millisecond) // events are handled in a goroutine; give a wrong one time to show
	assert.Empty(t, router.inputs(), "an unsigned postback reached the control path")
	assert.Zero(t, r.api.total())

	assert.Equal(t, http.StatusOK, post(signed("secret", body)))
	eventually(t, "the postback to be answered", func() bool { return len(r.api.callsOf("reply")) == 1 })
	require.Len(t, router.inputs(), 1)
	assert.Equal(t, "fine", r.lastReplyText())
}

// ---------------------------------------------------------------------------
// No model ever sees a tap.
// ---------------------------------------------------------------------------

// The channel and a real AgentLoop share one bus. Taps of every kind (handled,
// refused, malformed, from a stranger) publish nothing for the agent, and the
// model is never called; the same channel does feed the agent an ordinary
// message, so the check can fail.
func TestLINE_NoModelEverSeesAPostback(t *testing.T) {
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
		{user: ownerID, data: approveData},                  // handled
		{user: ownerID, data: approveData},                  // already decided
		{user: otherID, data: approveData},                  // allow-listed, not the owner
		{user: "Ustranger", data: approveData},              // a stranger
		{user: ownerID, data: "kq1|zzz|7F3K.abcd1234"},      // unknown action
		{user: ownerID, data: "kq1|apv|7F3K"},               // malformed
		{user: ownerID, data: "kq1|snz1h|7F3K abcd1234 "},   // junk payload
		{user: ownerID, srcType: "group", groupID: groupID}, // no data, in a group
	} {
		r.press(tp)
	}
	time.Sleep(200 * time.Millisecond) // a (wrongly) queued message would be picked up by now

	calls, seen := prov.snapshot()
	assert.Zero(t, calls, "the model was called for a tap")
	assert.NotContains(t, seen, "7F3K")
	assert.Empty(t, mb.InboundChan(), "a tap was published as an inbound message")

	// Positive control: the same wiring does hand an ordinary message to the agent.
	r.ch.processEvent(lineEvent{
		Type: "message", ReplyToken: "rt-m", Source: lineSource{Type: "user", UserID: ownerID},
		Message: json.RawMessage(`{"id":"m1","type":"text","text":"how is the market today?"}`),
	})
	eventually(t, "the model to be called for an ordinary message", func() bool { n, _ := prov.snapshot(); return n > 0 })
	n, seen := prov.snapshot()
	assert.Equal(t, 1, n)
	assert.Contains(t, seen, "how is the market today?")
}
