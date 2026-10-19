package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
)

var _ channels.ActionRenderer = (*DiscordChannel)(nil)

func TestDiscord_SupportsActions(t *testing.T) {
	assert.True(t, (&DiscordChannel{}).SupportsActions())
}

// ---------------------------------------------------------------------------
// Sending: the buttons.
// ---------------------------------------------------------------------------

func TestDiscord_ComponentsPayloadFormat(t *testing.T) {
	rows, err := buildComponents(approvalActions())
	require.NoError(t, err)
	b, err := json.Marshal(rows)
	require.NoError(t, err)
	var got []struct {
		Type       int `json:"type"`
		Components []struct {
			Type     int    `json:"type"`
			Label    string `json:"label"`
			Style    int    `json:"style"`
			CustomID string `json:"custom_id"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(b, &got))
	require.Len(t, got, 1)
	assert.Equal(t, 1, got[0].Type, "an action row")
	require.Len(t, got[0].Components, 2)
	assert.Equal(t, 2, got[0].Components[0].Type, "a button")
	assert.Equal(t, "Approve", got[0].Components[0].Label)
	assert.Equal(t, 1, got[0].Components[0].Style, "primary")
	assert.Equal(t, approveCID, got[0].Components[0].CustomID)
	assert.Equal(t, "Reject", got[0].Components[1].Label)
	assert.Equal(t, 4, got[0].Components[1].Style, "danger")
	assert.Equal(t, rejectCID, got[0].Components[1].CustomID)

	t.Run("at most five buttons to a row", func(t *testing.T) {
		var actions []bus.Action
		for id := range channels.CallbackCodes() {
			payload := "01HZX3K8V0F9Q2M4N6P8R0S2T4"
			if id == "approve" || id == "reject" {
				payload = "7F3K.abcd1234"
			}
			actions = append(actions, bus.Action{ID: id, Label: id, Payload: payload})
		}
		require.Len(t, actions, 8)
		rows, err := buildComponents(actions)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		assert.Len(t, rows[0].(discordgo.ActionsRow).Components, 5)
		assert.Len(t, rows[1].(discordgo.ActionsRow).Components, 3)
	})

	t.Run("the same button twice is refused: Discord rejects repeated custom ids", func(t *testing.T) {
		_, err := buildComponents(append(approvalActions(), approvalActions()[0]))
		assert.Error(t, err)
	})
}

func TestDiscord_CustomIDLimit(t *testing.T) {
	// The longest string the codec makes fits Discord's 100 bytes.
	longest := 0
	for id := range channels.CallbackCodes() {
		payload := strings.Repeat("a", bus.MaxActionPayloadBytes)
		if id == "approve" || id == "reject" {
			payload = "7F3K." + strings.Repeat("a", bus.MaxActionPayloadBytes-5)
		}
		data, err := channels.EncodeCallbackData(bus.Action{ID: id, Payload: payload}, maxCustomIDBytes)
		require.NoError(t, err, id)
		longest = max(longest, len(data))
	}
	assert.LessOrEqual(t, longest, maxCustomIDBytes)

	// Over the limit is an error, never a truncation.
	_, err := channels.EncodeCallbackData(bus.Action{ID: "approve", Payload: "7F3K.abcd1234"}, 20)
	assert.Error(t, err)

	// An action that cannot be encoded: the message goes out with no buttons, and
	// its text fallback is still in the body.
	r := newRig(t, nil, nil)
	for name, bad := range map[string]bus.Action{
		"payload over 48 bytes": {ID: "approve", Label: "Approve", Payload: "7F3K." + strings.Repeat("a", 60), TextCommand: "/approve 7F3K"},
		"unknown action":        {ID: "explode", Label: "Boom", Payload: "x", TextCommand: "/explode"},
		"bad payload byte":      {ID: "snooze_1h", Label: "Snooze", Payload: "a b", TextCommand: "/snooze 1h"},
	} {
		before := len(r.api.matching(http.MethodPost, "/messages"))
		_, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: "alert", Actions: append(approvalActions(), bad),
		})
		require.NoError(t, err, name)
		sent := r.api.matching(http.MethodPost, "/messages")
		require.Len(t, sent, before+1, name)
		body := sent[len(sent)-1].body
		assert.NotContains(t, body, "custom_id", name)
		assert.Contains(t, body, "/approve 7F3K", name)
	}
}

func TestDiscord_ButtonsOnLastMessageOnly(t *testing.T) {
	r := newRig(t, nil, nil)
	type sentMessage struct {
		Content    string           `json:"content"`
		Components []map[string]any `json:"components"`
		Reference  *struct {
			MessageID string `json:"message_id"`
		} `json:"message_reference"`
	}
	decode := func(req recordedRequest) sentMessage {
		var m sentMessage
		require.NoError(t, json.Unmarshal([]byte(req.body), &m))
		return m
	}

	t.Run("one message", func(t *testing.T) {
		ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: "Sell 0.01 BTC?", Actions: approvalActions(),
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"9001"}, ids)
		m := decode(r.api.matching(http.MethodPost, "/messages")[0])
		require.Len(t, m.Components, 1)
		assert.Contains(t, m.Content, "/approve 7F3K", "the text fallback stays in the body")
	})

	t.Run("a footer that pushes the text past the limit makes a second message; only it has buttons", func(t *testing.T) {
		before := len(r.api.matching(http.MethodPost, "/messages"))
		ids, err := r.ch.SendWithIDs(context.Background(), bus.OutboundMessage{
			ChatID: ownerDM, Content: strings.Repeat("word ", 396), Actions: approvalActions(), ReplyToMessageID: "77",
		})
		require.NoError(t, err)
		sent := r.api.matching(http.MethodPost, "/messages")[before:]
		require.Len(t, sent, 2)
		require.Len(t, ids, 2)
		first, last := decode(sent[0]), decode(sent[1])
		assert.Empty(t, first.Components)
		assert.Len(t, last.Components, 1)
		assert.True(t, strings.HasSuffix(last.Content, "/reject 7F3K"))
		require.NotNil(t, first.Reference)
		assert.Equal(t, "77", first.Reference.MessageID)
		assert.Nil(t, last.Reference, "only the first message is a reply")
	})

	t.Run("a message without actions has none", func(t *testing.T) {
		before := len(r.api.matching(http.MethodPost, "/messages"))
		require.NoError(t, r.ch.Send(context.Background(), bus.OutboundMessage{ChatID: ownerDM, Content: "plain"}))
		m := decode(r.api.matching(http.MethodPost, "/messages")[before])
		assert.Empty(t, m.Components)
	})
}

// ---------------------------------------------------------------------------
// Taps.
// ---------------------------------------------------------------------------

func TestDiscord_InteractionBuildsTheControlInput(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{Text: "ok", AnswerCallback: "ok"}, true
	}}
	r := newRig(t, nil, router)

	// A DM: the sender is i.User.
	r.press(tap{id: "i-dm", user: ownerID, customID: "kq1|snz4h|01HZ"})
	// A guild: the sender is i.Member.User, whatever i.User says.
	ic := r.interaction(tap{id: "i-guild", user: ownerID, customID: "kq1|use|01HZ", guild: true})
	ic.User = &discordgo.User{ID: otherID}
	r.ch.handleInteraction(r.ch.session, ic)

	in := router.inputs()
	require.Len(t, in, 2)
	assert.Equal(t, "discord", in[0].Channel)
	assert.Equal(t, ownerDM, in[0].ChatID)
	assert.Equal(t, "discord:"+ownerID, in[0].SenderID)
	assert.Equal(t, "discord:"+ownerID, in[0].Sender.CanonicalID)
	assert.Equal(t, "direct", in[0].PeerKind)
	assert.Equal(t, cardID, in[0].MessageID)
	assert.Equal(t, cardID, in[0].ReplyToMessageID, "for a button, the message it was on")
	assert.Empty(t, in[0].Text)
	require.NotNil(t, in[0].Callback)
	assert.Equal(t, channels.ControlCallback{Action: "snooze_4h", Payload: "01HZ", CallbackID: "i-dm"}, *in[0].Callback)

	assert.Equal(t, "discord:"+ownerID, in[1].SenderID, "the member's user, not the bare user field")
	assert.Equal(t, guildChan, in[1].ChatID)
	assert.Equal(t, "channel", in[1].PeerKind, "a guild channel is a group, as for a message")
}

func TestDiscord_InteractionRoutesToControl(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, customID: approveCID})

	got := appr.decisions()
	require.Len(t, got, 1)
	assert.Equal(t, decision{Code: "7F3K", Nonce: "abcd1234", Sender: "discord:" + ownerID, Approve: true}, got[0])
	assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"))

	cb := r.api.callbacks()
	require.Len(t, cb, 1, "responded once")
	assert.Equal(t, int(discordgo.InteractionResponseChannelMessageWithSource), cb[0].Type)
	assert.Equal(t, int(discordgo.MessageFlagsEphemeral), cb[0].Data.Flags, "only the tapper sees it")
	assert.Contains(t, cb[0].Data.Content, "Approved proposal 7F3K")
	assert.Contains(t, r.api.matching(http.MethodPost, "/callback")[0].path, "/interactions/int-1/tok-int-1/")

	// Reject takes the same road.
	appr2 := newFakeApprover("9ZZZ", "n0nce000")
	r2 := newRigWithApprover(t, appr2, nil)
	r2.press(tap{user: ownerID, customID: "kq1|rej|9ZZZ.n0nce000"})
	require.Len(t, appr2.decisions(), 1)
	assert.False(t, appr2.decisions()[0].Approve)
	assert.Contains(t, r2.lastCallback().Data.Content, "Rejected proposal 9ZZZ")
}

func TestDiscord_InteractionNonOwnerDenied(t *testing.T) {
	notAuthorized := func(c callback) bool {
		return c.Type == int(discordgo.InteractionResponseChannelMessageWithSource) &&
			c.Data.Content == "Not authorized." && c.Data.Flags == int(discordgo.MessageFlagsEphemeral)
	}

	t.Run("allow-listed, but not the owner", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, []string{ownerID, otherID})
		r.press(tap{user: otherID, customID: approveCID})
		assert.True(t, notAuthorized(r.lastCallback()), "%+v", r.lastCallback())
		assert.Empty(t, appr.decisions(), "the approval service was never asked")
		assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	})

	t.Run("not on the allow-list at all", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "should not happen"}, true
		}}
		r := newRig(t, []string{ownerID}, router)
		// Valid data, malformed data and garbage all get the same answer.
		for _, data := range []string{approveCID, "kq1|bogus|x", "junk", ""} {
			r.press(tap{user: otherID, customID: data})
			assert.True(t, notAuthorized(r.lastCallback()), "data %q", data)
		}
		assert.Empty(t, router.inputs(), "the control path never saw it")
		assert.Len(t, r.api.requests(), 4, "nothing but the four responses")
	})

	t.Run("an interaction that names nobody", func(t *testing.T) {
		router := &scriptedRouter{}
		r := newRig(t, nil, router)
		ic := r.interaction(tap{user: ownerID, customID: approveCID})
		ic.User = nil
		r.ch.handleInteraction(r.ch.session, ic)
		assert.True(t, notAuthorized(r.lastCallback()))
		assert.Empty(t, router.inputs())
	})
}

func TestDiscord_ForgedPayloadIsRefusedByDecide(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	r.press(tap{user: ownerID, customID: "kq1|apv|7F3K.deadbeef"})
	require.Len(t, appr.decisions(), 1, "it reached Decide, which is where it is refused")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, r.lastCallback().Data.Content, "No pending proposal 7F3K")
}

func TestDiscord_GuildTapFollowsTheGroupRule(t *testing.T) {
	appr := newFakeApprover("7F3K", "abcd1234")
	r := newRigWithApprover(t, appr, nil)
	// The owner, tapping in a guild channel.
	r.press(tap{user: ownerID, customID: approveCID, guild: true})
	assert.Empty(t, appr.decisions(), "an owner command does not work outside the owner chat")
	assert.Equal(t, approval.StatusPending, appr.statusOf("7F3K"))
	assert.Contains(t, r.lastCallback().Data.Content, "Owner commands only work in your owner chat")
}

func TestDiscord_EditOriginalUpdatesTheCard(t *testing.T) {
	router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
		return channels.ControlReply{
			Text: "Approved proposal 7F3K: sell 0.01 BTC on okx\nPlacing the order now.", EditOriginal: true,
			AnswerCallback: "Approved",
		}, true
	}}
	r := newRig(t, nil, router)
	r.press(tap{user: ownerID, customID: approveCID})

	cb := r.lastCallback()
	assert.Equal(t, int(discordgo.InteractionResponseUpdateMessage), cb.Type)
	assert.Contains(t, cb.Data.Content, "Approved proposal 7F3K")
	assert.Zero(t, cb.Data.Flags, "the card itself is edited, not an ephemeral message")
	require.NotNil(t, cb.Data.Components, "an empty list, not an absent one, removes the buttons")
	assert.Empty(t, *cb.Data.Components)
	assert.Len(t, r.api.requests(), 1, "one response and nothing else")
}

// Whatever happens to a tap, it is answered, once, within the deadline.
func TestDiscord_EveryInteractionIsAnswered(t *testing.T) {
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
		router func(t *testing.T) channels.ControlRouter
		allow  []string
	}{
		{"handled", tap{user: ownerID, customID: "kq1|snz4h|x"}, "fine", nil, nil},
		{"router panics", tap{user: ownerID, customID: "kq1|snz1h|x"}, "That command could not be handled", nil, nil},
		{"router does not know it", tap{user: ownerID, customID: "kq1|use|x"}, "That button is not valid.", nil, nil},
		{"unknown version", tap{user: ownerID, customID: "kq9|apv|7F3K.abcd1234"}, "That button is not valid.", nil, nil},
		{"unknown action", tap{user: ownerID, customID: "kq1|zzz|x"}, "That button is not valid.", nil, nil},
		{"malformed payload", tap{user: ownerID, customID: "kq1|apv|7F3K"}, "That button is not valid.", nil, nil},
		{"no custom id", tap{user: ownerID, customID: ""}, "That button is not valid.", nil, nil},
		{"id too long", tap{user: ownerID, customID: "kq1|snz1h|" + strings.Repeat("a", 100)}, "That button is not valid.", nil, nil},
		{"not allow-listed", tap{user: otherID, customID: "kq1|snz4h|x"}, "Not authorized.", nil, []string{ownerID}},
		{"no router", tap{user: ownerID, customID: "kq1|snz4h|x"}, "Buttons are not available right now.", func(*testing.T) channels.ControlRouter { return nil }, nil},
		{"real dispatcher, bad nonce", tap{user: ownerID, customID: "kq1|apv|7F3K.00000000"}, "No pending proposal", func(*testing.T) channels.ControlRouter {
			return control.New(control.Deps{Cfg: ownerConfig(), Approvals: appr})
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var router channels.ControlRouter = handling
			if tc.router != nil {
				router = tc.router(t)
			}
			r := newRig(t, tc.allow, router)
			r.press(tc.tap)
			cb := r.api.callbacks()
			require.Len(t, cb, 1, "exactly one response")
			assert.True(t, strings.HasPrefix(cb[0].Data.Content, tc.want), "content %q, want prefix %q", cb[0].Data.Content, tc.want)
			assert.Equal(t, int(discordgo.MessageFlagsEphemeral), cb[0].Data.Flags)
			select {
			case m := <-r.mb.InboundChan():
				t.Fatalf("a tap became an inbound message: %+v", m)
			default:
			}
		})
	}

	t.Run("an interaction of another kind is not ours", func(t *testing.T) {
		r := newRig(t, nil, handling)
		ic := r.interaction(tap{user: ownerID, customID: "kq1|snz4h|x"})
		ic.Type = discordgo.InteractionApplicationCommand
		r.ch.handleInteraction(r.ch.session, ic)
		r.ch.handleInteraction(r.ch.session, nil)
		assert.Empty(t, r.api.requests())
	})
}

// The control path is slow: the tap is acknowledged at the deadline, before the
// control path has answered, and the words follow.
func TestDiscord_SlowControlPathIsAcknowledgedFirst(t *testing.T) {
	run := func(t *testing.T, edit bool) (*rig, func()) {
		release := make(chan struct{})
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			<-release
			return channels.ControlReply{Text: "Approved proposal 7F3K: late", EditOriginal: edit, AnswerCallback: "Approved"}, true
		}}
		r := newRig(t, nil, router)
		r.ch.ackDeadline = 30 * time.Millisecond
		var once sync.Once
		return r, func() { once.Do(func() { close(release) }) }
	}

	t.Run("the card is edited afterwards", func(t *testing.T) {
		r, release := run(t, true)
		defer release()
		done := make(chan struct{})
		go func() { defer close(done); r.press(tap{user: ownerID, customID: approveCID}) }()
		eventually(t, "the early acknowledgement", func() bool { return len(r.api.callbacks()) == 1 })
		assert.Equal(t, int(discordgo.InteractionResponseDeferredMessageUpdate), r.api.callbacks()[0].Type)
		assert.Empty(t, r.api.matching(http.MethodPatch, "~/messages/@original"), "nothing yet: the control path has not answered")
		release()
		<-done
		edits := r.api.matching(http.MethodPatch, "~/messages/@original")
		require.Len(t, edits, 1)
		assert.Contains(t, edits[0].body, "Approved proposal 7F3K")
		assert.Contains(t, edits[0].body, `"components":[]`, "the buttons are removed")
		assert.Len(t, r.api.callbacks(), 1)
	})

	t.Run("an ephemeral followup otherwise", func(t *testing.T) {
		r, release := run(t, false)
		defer release()
		done := make(chan struct{})
		go func() { defer close(done); r.press(tap{user: ownerID, customID: approveCID}) }()
		eventually(t, "the early acknowledgement", func() bool { return len(r.api.callbacks()) == 1 })
		release()
		<-done
		follow := r.api.matching(http.MethodPost, "~/webhooks/app1/")
		require.Len(t, follow, 1)
		assert.Contains(t, follow[0].body, "Approved proposal 7F3K")
		assert.Contains(t, follow[0].body, `"flags":64`)
	})
}

func TestDiscord_DoubleTapApprovesOnce(t *testing.T) {
	t.Run("two taps", func(t *testing.T) {
		appr := newFakeApprover("7F3K", "abcd1234")
		r := newRigWithApprover(t, appr, nil)
		r.press(tap{user: ownerID, customID: approveCID})
		r.press(tap{user: ownerID, customID: approveCID})
		r.press(tap{user: ownerID, customID: rejectCID})

		require.Len(t, appr.decisions(), 3)
		assert.Equal(t, approval.StatusApproved, appr.statusOf("7F3K"), "the first decision stands")
		cb := r.api.callbacks()
		require.Len(t, cb, 3)
		assert.Contains(t, cb[0].Data.Content, "Approved proposal 7F3K")
		assert.Contains(t, cb[1].Data.Content, "already approved")
		assert.Contains(t, cb[2].Data.Content, "already approved")
	})

	t.Run("one interaction delivered twice reaches the control path once", func(t *testing.T) {
		router := &scriptedRouter{fn: func(channels.ControlInput) (channels.ControlReply, bool) {
			return channels.ControlReply{Text: "ok"}, true
		}}
		r := newRig(t, nil, router)
		r.press(tap{id: "same", user: ownerID, customID: approveCID})
		r.press(tap{id: "same", user: ownerID, customID: approveCID})
		assert.Len(t, router.inputs(), 1)
		assert.Len(t, r.api.callbacks(), 1)
	})
}

// ---------------------------------------------------------------------------
// No model ever sees a tap.
// ---------------------------------------------------------------------------

// The channel and a real AgentLoop share one bus. Taps of every kind (handled,
// refused, malformed, from a stranger) publish nothing for the agent, and the
// model is never called; the same channel does feed the agent an ordinary
// message, so the check can fail.
func TestDiscord_NoModelEverSeesAnInteraction(t *testing.T) {
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
		{user: ownerID, customID: approveCID},                // handled
		{user: ownerID, customID: approveCID},                // already decided
		{user: otherID, customID: approveCID},                // allow-listed, not the owner
		{user: "100000000000000099", customID: approveCID},   // a stranger
		{user: ownerID, customID: "kq1|zzz|7F3K.abcd1234"},   // unknown action
		{user: ownerID, customID: "kq1|apv|7F3K"},            // malformed
		{user: ownerID, customID: "kq1|snz1h|7F3K abcd1234"}, // junk payload
		{user: ownerID, customID: approveCID, guild: true},   // in a guild
	} {
		r.press(tp)
	}
	time.Sleep(200 * time.Millisecond) // a (wrongly) queued message would be picked up by now

	calls, seen := prov.snapshot()
	assert.Zero(t, calls, "the model was called for a tap")
	assert.NotContains(t, seen, "7F3K")
	assert.Empty(t, mb.InboundChan(), "a tap was published as an inbound message")

	// Positive control: the same wiring does hand an ordinary message to the agent.
	r.ch.handleMessage(r.ch.session, &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "m1", ChannelID: ownerDM, Content: "how is the market today?",
		Author: &discordgo.User{ID: ownerID, Username: "owner"},
	}})
	eventually(t, "the model to be called for an ordinary message", func() bool { n, _ := prov.snapshot(); return n > 0 })
	n, seen := prov.snapshot()
	assert.Equal(t, 1, n)
	assert.Contains(t, seen, "how is the market today?")
}
