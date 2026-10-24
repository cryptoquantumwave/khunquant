package control_test

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/control"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func replyMeta(id string) map[string]string { return map[string]string{"reply_to_message_id": id} }

func mustContain(t *testing.T, what, got string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(got, s) {
			t.Errorf("%s = %q, want it to contain %q", what, got, s)
		}
	}
}

// Every command handled for the owner in the owner chat: what it does, what it
// says, and that nothing reached the agent.
func TestControl_OwnerCommands(t *testing.T) {
	type tc struct {
		name  string
		text  string
		meta  map[string]string
		setup func(r *rig)
		reply []string
		check func(t *testing.T, r *rig)
	}
	cases := []tc{
		{name: "approve", text: "/approve 7F3K", reply: []string{"Approved proposal 7F3K", "sell 0.01 BTC on okx"},
			check: func(t *testing.T, r *rig) {
				want := []decideCall{{Code: "7F3K", Sender: "telegram:" + ownerUser, Approve: true}}
				if got := r.appr.calls(); !reflect.DeepEqual(got, want) {
					t.Errorf("Decide calls = %+v, want %+v", got, want)
				}
			}},
		{name: "approve is case-insensitive and tolerates the bot suffix", text: "  /APPROVE@khunbot 7f3k  ",
			reply: []string{"Approved proposal 7F3K"}},
		{name: "reject", text: "/reject 7F3K", reply: []string{"Rejected proposal 7F3K", "Nothing was placed"},
			check: func(t *testing.T, r *rig) {
				if got := r.appr.calls(); len(got) != 1 || got[0].Approve || got[0].Code != "7F3K" {
					t.Errorf("Decide calls = %+v", got)
				}
			}},
		{name: "reject without a valid code", text: "/reject", reply: []string{"Usage: /reject CODE"},
			check: func(t *testing.T, r *rig) {
				if len(r.appr.calls()) != 0 {
					t.Error("Decide was called")
				}
			}},
		{name: "approve with a trailing word", text: "/approve 7F3K now", reply: []string{"Usage: /approve CODE"}},
		{name: "snooze default", text: "/snooze", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc"),
			reply: []string{"Snoozed for 1h"}, check: muted("exact:price:btc", time.Hour)},
		{name: "snooze 4h", text: "/snooze 4h", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc", "dn:12"),
			reply: []string{"Snoozed for 4h"}, check: func(t *testing.T, r *rig) {
				muted("exact:price:btc", 4*time.Hour)(t, r)
				muted("exact:dn:12", 4*time.Hour)(t, r)
			}},
		{name: "snooze 1d", text: "/snooze 1d", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc"),
			reply: []string{"Snoozed for 1d"}, check: muted("exact:price:btc", 24*time.Hour)},
		{name: "snooze bad duration", text: "/snooze 3h", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc"),
			reply: []string{"Usage: /snooze [1h|4h|1d]"}},
		{name: "snooze without a reply", text: "/snooze", reply: []string{"Reply to one of my alerts"}},
		{name: "snooze an unknown message", text: "/snooze", meta: replyMeta("777"),
			reply: []string{"Nothing to snooze"}},
		{name: "snooze a message with no decisions", text: "/snooze", meta: replyMeta("900"),
			setup: func(r *rig) { r.sent["900"] = "I9" }, reply: []string{"Nothing to snooze"}},
		{name: "mute", text: "/mute price_move", reply: []string{"Muted price_move for 30 days"},
			check: muted("price_move", 30*24*time.Hour)},
		{name: "mute without a trigger", text: "/mute", reply: []string{"Usage: /mute TRIGGER"}},
		{name: "mute with a bad trigger", text: "/mute a%b", reply: []string{"Usage: /mute TRIGGER"}},
		{name: "unmute", text: "/unmute price_move", reply: []string{"Unmuted price_move"},
			check: func(t *testing.T, r *rig) {
				if !reflect.DeepEqual(r.att.unmute, []string{"price_move"}) {
					t.Errorf("unmuted %v", r.att.unmute)
				}
			}},
		{name: "useful", text: "/useful", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc"),
			reply: []string{"Noted as useful", "only written to the log"}},
		{name: "notuseful", text: "/notuseful", meta: replyMeta("900"), setup: alertAt("900", "I1", "price:btc"),
			reply: []string{"Noted as not useful", "only written to the log"}},
		{name: "useful without a reply", text: "/useful", reply: []string{"Reply to one of my alerts"}},
		{name: "useful on something that is not an alert", text: "/useful", meta: replyMeta("777"),
			reply: []string{"not one of my alerts"}},
		{name: "whoami", text: "/whoami", reply: []string{"sender_id: telegram:" + ownerUser, "You are the configured owner (this is the owner chat)."}},
		{name: "stop", text: "/stop", reply: []string{"Stopped acting: approvals and automatic actions are refused until /resume. Monitoring and notifications stay on."},
			check: func(t *testing.T, r *rig) {
				if !r.kill.Killed() || r.kill.by != "telegram:"+ownerUser {
					t.Errorf("kill = %v by %q", r.kill.Killed(), r.kill.by)
				}
			}},
		{name: "resume", text: "/resume", setup: func(r *rig) { r.kill.on = true }, reply: []string{"Resumed"},
			check: func(t *testing.T, r *rig) {
				if r.kill.Killed() {
					t.Error("still killed")
				}
			}},
		{name: "resume when not stopped", text: "/resume", reply: []string{"not stopped"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			if c.setup != nil {
				c.setup(r)
			}
			r.ch.say(ownerUser, ownerChat, c.text, c.meta)
			reply := r.ch.onlyReply(t)
			mustContain(t, "reply", reply.Content, c.reply...)
			if reply.ChatID != ownerChat || reply.Channel != "telegram" || reply.ReplyToMessageID != "m1" {
				t.Errorf("reply addressed %+v", reply)
			}
			if got := r.inbound(); len(got) != 0 {
				t.Errorf("%d messages reached the agent: %+v", len(got), got)
			}
			if c.check != nil {
				c.check(t, r)
			}
		})
	}
}

func alertAt(msgID, intervention string, keys ...string) func(*rig) {
	return func(r *rig) {
		r.sent[msgID] = intervention
		r.att.keys[intervention] = keys
	}
}

func muted(key string, dur time.Duration) func(*testing.T, *rig) {
	return func(t *testing.T, r *rig) {
		t.Helper()
		got, ok := r.att.muted()[key]
		if !ok {
			t.Fatalf("%q was not muted: %v", key, r.att.muted())
		}
		if d := time.Until(got); d < dur-time.Minute || d > dur+time.Minute {
			t.Errorf("%q muted for %v, want about %v", key, d, dur)
		}
	}
}

var ownerOnlyTexts = []string{
	"/approve 7F3K", "/reject 7F3K", "/snooze", "/mute price_move", "/unmute price_move",
	"/useful", "/notuseful", "/stop", "/resume",
}

// A sender who passed the allow-list but is not the owner gets "Not authorized."
// and nothing else, and nothing is done.
func TestControl_NonOwnerDenied(t *testing.T) {
	for _, text := range ownerOnlyTexts {
		t.Run(text, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			alertAt("900", "I1", "price:btc")(r)
			r.ch.say(otherUser, otherUser, text, replyMeta("900"))
			if got := r.ch.onlyReply(t).Content; got != "Not authorized." {
				t.Errorf("reply = %q", got)
			}
			if len(r.appr.calls()) != 0 || r.kill.Killed() || len(r.att.muted()) != 0 || len(r.att.unmute) != 0 {
				t.Errorf("a non-owner changed something: decides=%v killed=%v mutes=%v", r.appr.calls(), r.kill.Killed(), r.att.muted())
			}
			if got := r.inbound(); len(got) != 0 {
				t.Errorf("the command reached the agent: %+v", got)
			}
		})
	}

	t.Run("the owner's id in another platform is not the owner", func(t *testing.T) {
		r := newRig(t, "discord", func(c *config.Config) {
			c.Proactive.Owner = config.OwnerConfig{Channel: "telegram", ChatID: ownerChat, SenderID: "telegram:" + ownerUser}
		})
		r.ch.say(ownerUser, ownerChat, "/stop", nil)
		if got := r.ch.onlyReply(t).Content; got != "Not authorized." {
			t.Errorf("reply = %q", got)
		}
		if r.kill.Killed() {
			t.Error("killed by a sender on another platform")
		}
	})

	t.Run("a non-owner may still /whoami", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.say(otherUser, otherUser, "/whoami", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "sender_id: telegram:"+otherUser, "You are not the configured owner.")
	})
}

// The SECURITY line: once per sender per minute, naming the command and the
// channel and never the owner.
func TestControl_NonOwnerSecurityLogIsRateLimitedAndHidesTheOwner(t *testing.T) {
	var buf lockedBuffer
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := newRig(t, "telegram", nil)
	r.d = control.New(r.d.Deps(), control.WithClock(clock), control.WithRateLimit(100, time.Minute))
	r.ch.SetControlRouter(r.d)

	for i := 0; i < 5; i++ {
		r.ch.say(otherUser, otherUser, "/stop", nil)
	}
	out := ansi.ReplaceAllString(buf.String(), "")
	if n := strings.Count(out, "SECURITY: control command from a sender who is not the owner"); n != 1 {
		t.Fatalf("%d SECURITY lines in a minute, want 1:\n%s", n, out)
	}
	mustContain(t, "log", out, "command=stop", "channel=telegram")
	if strings.Contains(out, ownerUser) {
		t.Errorf("the log names the owner:\n%s", out)
	}

	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	r.ch.say(otherUser, otherUser, "/stop", nil)
	if n := strings.Count(ansi.ReplaceAllString(buf.String(), ""), "SECURITY: control command"); n != 2 {
		t.Errorf("%d SECURITY lines after the minute, want 2", n)
	}
}

// With no owner configured every owner-only command is refused with a hint.
func TestControl_NoOwnerConfigured(t *testing.T) {
	for _, text := range ownerOnlyTexts {
		t.Run(text, func(t *testing.T) {
			r := newRig(t, "telegram", func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} })
			r.ch.say(ownerUser, ownerChat, text, nil)
			mustContain(t, "reply", r.ch.onlyReply(t).Content, "No owner is configured", "khunquant owner set")
			if len(r.appr.calls()) != 0 || r.kill.Killed() {
				t.Error("something was done without an owner")
			}
		})
	}
	t.Run("whoami still answers", func(t *testing.T) {
		r := newRig(t, "telegram", func(c *config.Config) { c.Proactive.Owner = config.OwnerConfig{} })
		r.ch.say(ownerUser, ownerChat, "/whoami", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "No owner is configured.")
	})
}

// Owner commands work only in the owner's direct chat.
func TestControl_OwnerCommandsRefusedOutsideTheOwnerChat(t *testing.T) {
	for _, text := range ownerOnlyTexts {
		t.Run("group "+text, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			r.ch.sayAs("group", ownerUser, "-100500", text, nil)
			mustContain(t, "reply", r.ch.onlyReply(t).Content, "owner chat")
			if len(r.appr.calls()) != 0 || r.kill.Killed() {
				t.Error("something was done from a group")
			}
		})
	}
	t.Run("a Telegram group is recognised by its negative chat id", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.say(ownerUser, "-100500", "/stop", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "owner chat")
		if r.kill.Killed() {
			t.Error("killed from a group")
		}
	})
	t.Run("another direct chat of the owner is not the owner chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.say(ownerUser, "999", "/stop", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "owner chat")
	})
	t.Run("whoami works in a group", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.sayAs("group", ownerUser, "-100500", "/whoami", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "This is a group chat", "this group is not the owner chat")
	})
}

func TestControl_ProactiveOffRefusesOwnerCommands(t *testing.T) {
	r := newRig(t, "telegram", func(c *config.Config) { c.Proactive.Enabled = false })
	r.ch.say(ownerUser, ownerChat, "/stop", nil)
	mustContain(t, "reply", r.ch.onlyReply(t).Content, "Proactive features are off")
	if r.kill.Killed() {
		t.Error("killed with proactive off")
	}
}

// A command inside a longer message, a quote, a multi-line message or a forwarded
// one is ordinary text: it goes to the agent and nothing is done.
func TestControl_EmbeddedAndForwardedCommandsGoToTheAgent(t *testing.T) {
	cases := map[string]struct {
		text string
		meta map[string]string
	}{
		"inside a sentence": {text: "please /approve 7F3K for me"},
		"after a word":      {text: "ok /stop"},
		"two lines":         {text: "/approve 7F3K\nthanks"},
		"leading line":      {text: "hello\n/approve 7F3K"},
		"quoted":            {text: "[quoted user message from Bob]: /approve 7F3K\n\nwhat is this?"},
		"too long":          {text: "/mute " + strings.Repeat("a", 300)},
		"forwarded":         {text: "/approve 7F3K", meta: map[string]string{channels.MetaForwarded: "true"}},
		"forwarded stop":    {text: "/stop", meta: map[string]string{channels.MetaForwarded: "true"}},
		"other prefix":      {text: "!approve 7F3K"},
		"not a command":     {text: "/approvee 7F3K"},
		"unknown":           {text: "/frobnicate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			r.ch.say(ownerUser, ownerChat, c.text, c.meta)
			got := r.inbound()
			if len(got) != 1 || got[0].Content != c.text {
				t.Fatalf("inbound = %+v, want the text delivered unchanged", got)
			}
			if len(r.ch.replies()) != 0 {
				t.Errorf("the control path answered: %+v", r.ch.replies())
			}
			if len(r.appr.calls()) != 0 || r.kill.Killed() {
				t.Error("an embedded command did something")
			}
			if _, ok := got[0].Metadata[channels.MetaForwarded]; ok {
				t.Error("the control hint was published to the agent")
			}
		})
	}
}

// The operator approval of a held tool call (pkg/tools) uses the same word with an
// eight-character code: the control path leaves it alone.
func TestControl_OperatorApprovalCodesAreLeftToTheirOwnPath(t *testing.T) {
	for _, text := range []string{"/approve ABCD2345", "/approve", "/approve 7F3", "/approve 7F3K5", "/approve 0O1I", "/deny ABCD2345"} {
		t.Run(text, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			r.ch.say(ownerUser, ownerChat, text, nil)
			if got := r.inbound(); len(got) != 1 || got[0].Content != text {
				t.Fatalf("inbound = %+v", got)
			}
			if len(r.ch.replies()) != 0 || len(r.appr.calls()) != 0 {
				t.Error("the control path acted on an operator approval")
			}
		})
	}
}

// Sending an approval typed as a reply to a pending card.
func TestControl_ReplyApproveOnlyForPendingCard(t *testing.T) {
	card := approval.Proposal{Code: "7F3K", Summary: "buy 1 ETH on okx", Status: approval.StatusPending}
	setup := func(r *rig) { r.appr.cards["telegram|"+ownerChat+"|500"] = card }

	for _, word := range []string{"yes", "Yes", "OK", "approve", " Approve ", "อนุมัติ", "ตกลง"} {
		t.Run("approves "+word, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			setup(r)
			r.ch.say(ownerUser, ownerChat, word, replyMeta("500"))
			mustContain(t, "reply", r.ch.onlyReply(t).Content, "Approved proposal 7F3K")
			want := []decideCall{{Code: "7F3K", Sender: "telegram:" + ownerUser, Approve: true}}
			if got := r.appr.calls(); !reflect.DeepEqual(got, want) {
				t.Errorf("Decide calls = %+v, want %+v", got, want)
			}
			if got := r.inbound(); len(got) != 0 {
				t.Errorf("the approval reached the agent: %+v", got)
			}
		})
	}

	t.Run("a reply to some other message is ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		setup(r)
		r.ch.say(ownerUser, ownerChat, "yes", replyMeta("501"))
		assertAgentGot(t, r, "yes")
	})
	t.Run("a card that is no longer pending is ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.say(ownerUser, ownerChat, "yes", replyMeta("500"))
		assertAgentGot(t, r, "yes")
	})
	t.Run("other words are ordinary chat", func(t *testing.T) {
		for _, w := range []string{"yes please", "yes!", "no", "yess", "ok then"} {
			r := newRig(t, "telegram", nil)
			setup(r)
			r.ch.say(ownerUser, ownerChat, w, replyMeta("500"))
			assertAgentGot(t, r, w)
		}
	})
	t.Run("a non-owner's yes to the card is ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		setup(r)
		r.ch.say(otherUser, ownerChat, "yes", replyMeta("500"))
		assertAgentGot(t, r, "yes")
	})
	t.Run("the owner's yes from a group is ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		setup(r)
		r.ch.sayAs("group", ownerUser, "-100500", "yes", replyMeta("500"))
		assertAgentGot(t, r, "yes")
	})
	t.Run("a forwarded yes is ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		setup(r)
		meta := replyMeta("500")
		meta[channels.MetaForwarded] = "true"
		r.ch.say(ownerUser, ownerChat, "yes", meta)
		assertAgentGot(t, r, "yes")
	})
	t.Run("the sender's own text is used, not the quoted prefix", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		setup(r)
		meta := replyMeta("500")
		meta[channels.MetaOwnText] = "yes"
		r.ch.say(ownerUser, ownerChat, "[quoted assistant message from KhunQuant]: Proposal 7F3K ...\n\nyes", meta)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "Approved proposal 7F3K")
		if got := r.inbound(); len(got) != 0 {
			t.Errorf("reached the agent: %+v", got)
		}
	})
	t.Run("a lookup that fails is said, not guessed", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.appr.cardErr = errBoom
		r.ch.say(ownerUser, ownerChat, "yes", replyMeta("500"))
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "could not be handled")
		if got := r.inbound(); len(got) != 0 {
			t.Errorf("reached the agent: %+v", got)
		}
	})
	t.Run("no approval service: ordinary chat", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		deps := r.d.Deps()
		deps.Approvals = nil
		r.d.SetDeps(deps)
		r.ch.say(ownerUser, ownerChat, "yes", replyMeta("500"))
		assertAgentGot(t, r, "yes")
	})
}

func TestControl_YesReplyToNormalMessageGoesToAgent(t *testing.T) {
	r := newRig(t, "telegram", nil)
	r.appr.cards["telegram|"+ownerChat+"|500"] = approval.Proposal{Code: "7F3K"}
	r.ch.say(ownerUser, ownerChat, "yes", replyMeta("42")) // 42 is not a card
	assertAgentGot(t, r, "yes")
	r.ch.say(ownerUser, ownerChat, "yes", nil) // not a reply at all
	assertAgentGot(t, r, "yes")
	if len(r.appr.calls()) != 0 {
		t.Error("Decide was called")
	}
}

func assertAgentGot(t *testing.T, r *rig, text string) {
	t.Helper()
	got := r.inbound()
	if len(got) != 1 || got[0].Content != text {
		t.Fatalf("inbound = %+v, want the single message %q", got, text)
	}
	if len(r.ch.replies()) != 0 || len(r.appr.calls()) != 0 {
		t.Errorf("the control path acted: replies=%+v decides=%+v", r.ch.replies(), r.appr.calls())
	}
}

// The control path comes before the typing indicator, the reaction and the
// placeholder; a message that is not handled still gets all three.
func TestControl_NoTypingOrPlaceholderSideEffects(t *testing.T) {
	r := newRig(t, "telegram", nil)
	r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	r.ch.say(otherUser, otherUser, "/stop", nil) // denied: still a control command
	r.ch.say(ownerUser, ownerChat, "/whoami", nil)
	if n := len(r.ch.replies()); n != 3 {
		t.Fatalf("%d replies, want 3", n)
	}
	if r.ch.typing.Load() != 0 || r.ch.reactions.Load() != 0 || r.ch.placeholders.Load() != 0 {
		t.Errorf("side effects on handled commands: typing=%d reactions=%d placeholders=%d",
			r.ch.typing.Load(), r.ch.reactions.Load(), r.ch.placeholders.Load())
	}

	// An ordinary message still gets all three.
	r.ch.say(ownerUser, ownerChat, "hello", nil)
	if r.ch.typing.Load() != 1 || r.ch.reactions.Load() != 1 || r.ch.placeholders.Load() != 1 {
		t.Errorf("ordinary message: typing=%d reactions=%d placeholders=%d, want 1 each",
			r.ch.typing.Load(), r.ch.reactions.Load(), r.ch.placeholders.Load())
	}
}

// A message that is not a control command is published exactly as it was before
// the control path existed.
func TestControl_NonCommandIsPublishedUnchanged(t *testing.T) {
	r := newRig(t, "telegram", nil)
	meta := map[string]string{"user_id": "1001", "username": "alice", "reply_to_message_id": "42"}
	// The hints a channel may add are removed; everything else is kept.
	withHints := map[string]string{"user_id": "1001", "username": "alice", "reply_to_message_id": "42",
		channels.MetaOwnText: "hi", channels.MetaForwarded: "true"}
	sender := bus.SenderInfo{Platform: "telegram", PlatformID: ownerUser, CanonicalID: "telegram:" + ownerUser, Username: "alice"}
	r.ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: ownerUser}, "m7", ownerUser, ownerChat,
		"[quoted assistant message from KhunQuant]: x\n\nhi", []string{"/tmp/a.png"}, withHints, sender)

	got := r.inbound()
	if len(got) != 1 {
		t.Fatalf("%d inbound messages", len(got))
	}
	want := bus.InboundMessage{
		Channel: "telegram", SenderID: "telegram:" + ownerUser, Sender: sender, ChatID: ownerChat,
		Content: "[quoted assistant message from KhunQuant]: x\n\nhi", Media: []string{"/tmp/a.png"},
		Peer: bus.Peer{Kind: "direct", ID: ownerUser}, MessageID: "m7",
		MediaScope: "telegram:" + ownerChat + ":m7", Metadata: meta,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("published message differs:\n got  %+v\n want %+v", got[0], want)
	}
	if len(r.ch.replies()) != 0 {
		t.Errorf("replies: %+v", r.ch.replies())
	}
}

// Ten control actions a minute per sender; /stop is exempt for the owner.
func TestControl_RateLimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	r := newRig(t, "telegram", nil)
	r.d = control.New(r.d.Deps(), control.WithClock(clock))
	r.ch.SetControlRouter(r.d)

	for i := 0; i < 10; i++ {
		r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	}
	if n := len(r.appr.calls()); n != 10 {
		t.Fatalf("%d decisions in the first ten, want 10", n)
	}
	r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	r.ch.say(ownerUser, ownerChat, "/reject 7F3K", nil)
	replies := r.ch.replies()
	if len(replies) != 12 || replies[10].Content != "Too many commands; wait a minute." || replies[11].Content != "Too many commands; wait a minute." {
		t.Fatalf("replies after the limit: %+v", replies[10:])
	}
	if n := len(r.appr.calls()); n != 10 {
		t.Errorf("a rate-limited command was executed: %d decisions", n)
	}

	// /stop always works for the owner.
	r.ch.say(ownerUser, ownerChat, "/stop", nil)
	if !r.kill.Killed() {
		t.Error("/stop was rate limited")
	}

	// The limit is per sender.
	r.ch.say(otherUser, otherUser, "/whoami", nil)
	if got := r.ch.replies(); strings.Contains(got[len(got)-1].Content, "Too many") {
		t.Error("another sender was limited")
	}

	// A non-owner cannot use the exemption to flood.
	for i := 0; i < 12; i++ {
		r.ch.say(otherUser, otherUser, "/stop", nil)
	}
	last := r.ch.replies()
	if last[len(last)-1].Content != "Too many commands; wait a minute." {
		t.Errorf("a non-owner's /stop was not limited: %q", last[len(last)-1].Content)
	}

	// The window slides.
	advance(61 * time.Second)
	r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	if n := len(r.appr.calls()); n != 11 {
		t.Errorf("after the window: %d decisions, want 11", n)
	}
}

type panicRouter struct {
	channels.ControlRouter
	panicOn func(channels.ControlInput) bool
	// recognisesPanics makes Recognises panic too.
	recognisesPanics bool
}

func (p panicRouter) TryHandle(ctx context.Context, in channels.ControlInput) (channels.ControlReply, bool) {
	if p.panicOn(in) {
		panic("router bug")
	}
	return p.ControlRouter.TryHandle(ctx, in)
}

func (p panicRouter) Recognises(in channels.ControlInput) bool {
	if p.recognisesPanics {
		panic("router bug")
	}
	return p.ControlRouter.Recognises(in)
}

// A router that panics on a command never lets the command through to the agent;
// anything else carries on as if there were no router.
func TestControl_RouterPanicFailsClosed(t *testing.T) {
	always := func(channels.ControlInput) bool { return true }
	for name, p := range map[string]panicRouter{
		"TryHandle panics":               {panicOn: always},
		"TryHandle and Recognises panic": {panicOn: always, recognisesPanics: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			p.ControlRouter = r.d
			r.ch.SetControlRouter(p)

			r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
			mustContain(t, "reply", r.ch.onlyReply(t).Content, "could not be handled")
			if got := r.inbound(); len(got) != 0 {
				t.Fatalf("a command reached the agent after a router panic: %+v", got)
			}
			if r.ch.typing.Load() != 0 {
				t.Error("typing started for a command")
			}
		})
	}

	t.Run("a message that is not a command carries on", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.ch.SetControlRouter(panicRouter{ControlRouter: r.d, panicOn: always})
		r.ch.say(ownerUser, ownerChat, "good morning", nil)
		assertAgentGot(t, r, "good morning")
	})

	t.Run("a handler that panics is answered, not fatal", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		if !r.d.Override("stop", func(context.Context, control.Input) (control.Reply, error) { panic("handler bug") }) {
			t.Fatal("no such command")
		}
		r.ch.say(ownerUser, ownerChat, "/stop", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "could not be handled")
		if got := r.inbound(); len(got) != 0 {
			t.Errorf("reached the agent: %+v", got)
		}
	})
	t.Run("a handler that errors is answered", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		r.att.err = errBoom
		alertAt("900", "I1", "price:btc")(r)
		r.ch.say(ownerUser, ownerChat, "/snooze", replyMeta("900"))
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "could not be handled")
	})
}

// A decision the approval service refuses is explained, not swallowed.
func TestControl_ApproveRefusals(t *testing.T) {
	cases := map[string]struct {
		err  error
		want []string
	}{
		"killed":    {approval.ErrKilled, []string{"kill switch is engaged", "/resume", "/reject 7F3K"}},
		"not found": {approval.ErrNotFound, []string{"No pending proposal 7F3K"}},
		"decided":   {&approval.StatusError{Code: "7F3K", Status: approval.StatusExecuted}, []string{"already executed"}},
		"not owner": {approval.ErrNotOwner, []string{"Not authorized."}},
		"other":     {errBoom, []string{"could not be handled"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "telegram", nil)
			r.appr.decideFn = func(string, bool) (approval.Proposal, error) { return approval.Proposal{}, c.err }
			r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
			mustContain(t, "reply", r.ch.onlyReply(t).Content, c.want...)
			if strings.Contains(r.ch.replies()[0].Content, "boom") {
				t.Error("an internal error text reached the chat")
			}
		})
	}
	t.Run("approvals not wired", func(t *testing.T) {
		r := newRig(t, "telegram", nil)
		deps := r.d.Deps()
		deps.Approvals = nil
		r.d.SetDeps(deps)
		r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
		mustContain(t, "reply", r.ch.onlyReply(t).Content, "Order approvals are not available")
	})
}

// Button presses reach the same handlers.
func TestControl_Callbacks(t *testing.T) {
	r := newRig(t, "telegram", nil)
	in := func(action, payload, reply string) channels.ControlInput {
		return channels.ControlInput{
			Channel: "telegram", ChatID: ownerChat, MessageID: "m1", SenderID: "telegram:" + ownerUser, PeerKind: "direct",
			ReplyToMessageID: reply, Callback: &channels.ControlCallback{Action: action, Payload: payload, CallbackID: "cb1"},
		}
	}

	rep, ok := r.d.TryHandle(context.Background(), in("approve", "7F3K.abcdef0123456789", ""))
	if !ok || !strings.Contains(rep.Text, "Approved proposal 7F3K") || rep.AnswerCallback == "" {
		t.Fatalf("approve button: %+v, %v", rep, ok)
	}
	if got := r.appr.calls(); len(got) != 1 || got[0].Nonce != "abcdef0123456789" || got[0].Code != "7F3K" || !got[0].Approve {
		t.Errorf("Decide calls = %+v", got)
	}
	rep, ok = r.d.TryHandle(context.Background(), in("reject", "7F3K.abcdef0123456789", ""))
	if !ok || !strings.Contains(rep.Text, "Rejected") {
		t.Fatalf("reject button: %+v, %v", rep, ok)
	}

	alertAt("900", "I1", "price:btc")(r)
	rep, ok = r.d.TryHandle(context.Background(), in("snooze_4h", "", "900"))
	if !ok || !strings.Contains(rep.Text, "Snoozed for 4h") {
		t.Fatalf("snooze button: %+v, %v", rep, ok)
	}
	muted("exact:price:btc", 4*time.Hour)(t, r)
	rep, ok = r.d.TryHandle(context.Background(), in("not_useful", "", "900"))
	if !ok || !strings.Contains(rep.Text, "not useful") {
		t.Fatalf("not_useful button: %+v, %v", rep, ok)
	}

	// A button from somebody who is not the owner.
	bad := in("approve", "7F3K.abcdef0123456789", "")
	bad.SenderID = "telegram:" + otherUser
	before := len(r.appr.calls())
	rep, ok = r.d.TryHandle(context.Background(), bad)
	if !ok || rep.Text != "Not authorized." {
		t.Errorf("a non-owner's button: %+v, %v", rep, ok)
	}
	if len(r.appr.calls()) != before {
		t.Error("a non-owner's button decided a proposal")
	}

	// A button this path does not know is left to the channel.
	for _, c := range []channels.ControlInput{in("frob", "x", ""), in("approve", "not-a-payload", ""), in("approve", "", "")} {
		if _, ok := r.d.TryHandle(context.Background(), c); ok {
			t.Errorf("handled %+v", c.Callback)
		}
	}
}

// Every handled input emits control.command, with no code, nonce, text or sender.
func TestControl_EmitsEvents(t *testing.T) {
	eb := events.NewBus()
	t.Cleanup(func() { _ = eb.Close() })
	sub, ch, err := eb.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t", Buffer: 32})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	r := newRig(t, "telegram", nil)
	deps := r.d.Deps()
	deps.Events = eb
	r.d.SetDeps(deps)

	r.ch.say(ownerUser, ownerChat, "/approve 7F3K", nil)
	r.ch.say(otherUser, otherUser, "/stop", nil)
	r.ch.say(ownerUser, ownerChat, "just chatting", nil)

	var got []events.Event
	for len(got) < 2 {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d events: %+v", len(got), got)
		}
	}
	select {
	case e := <-ch:
		t.Fatalf("an event for a message that is not a command: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	type payload = map[string]any
	if got[0].Kind != events.KindControlCommand || got[0].Payload.(payload)["command"] != "approve" ||
		got[0].Payload.(payload)["owner"] != true || got[0].Payload.(payload)["outcome"] != "ok" {
		t.Errorf("first event: %+v", got[0])
	}
	if p := got[1].Payload.(payload); p["command"] != "stop" || p["owner"] != false || p["outcome"] != "denied" {
		t.Errorf("second event: %+v", got[1])
	}
	for _, e := range got {
		// The owner's direct chat id is the owner's user id (ownerChat == ownerUser here,
		// as on Telegram): the scope must carry neither.
		if e.Scope.SenderID != "" || e.Scope.ChatID != "" || e.Scope.MessageID != "" || e.Scope.Channel != "telegram" {
			t.Errorf("event carries more than it should: %+v", e)
		}
		for k, v := range e.Payload.(payload) {
			if s, ok := v.(string); ok && (strings.Contains(strings.ToUpper(s), "7F3K") || strings.Contains(s, ownerUser)) {
				t.Errorf("payload %s=%q leaks", k, s)
			}
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for the logger's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
