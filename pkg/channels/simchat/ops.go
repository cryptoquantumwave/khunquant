package simchat

import (
	"cmp"
	"context"
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// Argument schemas. A name shared by several ops has one type everywhere: the MCP tool
// merges the ops' properties into one schema.
const (
	argAs       = `"as":{"type":"string","description":"persona, or persona@group:<name> to speak in a group"}`
	argText     = `"text":{"type":"string"}`
	argTo       = `"to":{"type":"string","description":"chat id: dm:<persona> or group:<name>"}`
	argSince    = `"since":{"type":"string","description":"RFC3339 sim time, inclusive"}`
	argAfterSeq = `"after_seq":{"type":"integer","description":"cursor: only messages with a greater seq"}`
	argLimit    = `"limit":{"type":"integer","description":"newest messages to return, default 100"}`
)

func schema(props ...string) json.RawMessage {
	return json.RawMessage("{" + strings.Join(props, ",") + "}")
}

func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return lab.Errorf(lab.CodeBadArgs, "%v", err)
	}
	return nil
}

func badArgs(format string, a ...any) error { return lab.Errorf(lab.CodeBadArgs, format, a...) }

// Group is the lab group "chat": talk to the instance as a persona, read what it sent,
// and switch the channel's failure state.
func (w *World) Group() lab.Group {
	return lab.Group{Name: "chat", Description: "Talk to the instance through the simulated chat channel as a persona, " +
		"read every message it sent (with its buttons), and make the channel fail.",
		Ops: []lab.Op{
			{Name: "send", Description: "a persona sends a message; it goes through the real channel path",
				Args: schema(argAs, argText, `"chat":{"type":"string","description":"overrides the @ part of as"}`,
					`"reply_to":{"type":"string","description":"id of the message replied to"}`,
					`"quote":{"type":"string","description":"text of the replied-to message, put before the reply as a client does"}`,
					`"forwarded":{"type":"boolean"}`, `"mention":{"type":"boolean","description":"mention the bot (group chats)"}`,
					`"spoof_as":{"type":"string","description":"claim another persona's sender id; only an unauthenticated persona can be claimed"}`),
				Required: []string{"as", "text"}, Run: w.sendOp},
			{Name: "outbox", Description: "messages the instance sent, newest last",
				Args: schema(argTo, argSince, argAfterSeq, argLimit, `"include_failed":{"type":"boolean","description":"also the sends the channel refused"}`),
				Run:  w.outboxOp},
			{Name: "set_state", Description: "make the channel up, down, slow, ratelimited or flaky",
				Args: schema(`"state":{"type":"string","enum":["up","down","slow","ratelimited","flaky"]}`,
					`"delay":{"type":"string","description":"slow: Go duration of wall time per send"}`,
					`"per_minute":{"type":"integer","description":"ratelimited: sends allowed per sim minute"}`,
					`"p":{"type":"number","description":"flaky: chance of a temporary failure"}`,
					`"seed":{"type":"integer","description":"flaky: random seed"}`,
					`"error":{"type":"string","enum":["temporary","send_failed"],"description":"down: temporary (the manager retries) or send_failed (it does not)"}`),
				Required: []string{"state"}, Run: w.setStateOp},
		}}
}

// sendOp is one inbound message. The sender and chat come from the persona; nothing the
// caller passes can name another sender except spoof_as, for an unauthenticated persona.
func (w *World) sendOp(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		As, Chat, Text, Quote string
		SpoofAs               string `json:"spoof_as"`
		ReplyTo               string `json:"reply_to"`
		Forwarded, Mention    bool
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Text) == "" {
		return nil, badArgs("text is required")
	}
	ch := w.Attached()
	if ch == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "the simchat channel is not running in this instance")
	}
	p, chatID, kind, err := w.route(a.As, a.Chat)
	if err != nil {
		return nil, badArgs("%v", err)
	}
	spoofedBy := ""
	if a.SpoofAs != "" {
		c, ok := w.personas[a.SpoofAs]
		if !ok || !c.Unauthenticated {
			return nil, badArgs("persona %q cannot be spoofed: only a persona with unauthenticated:true can", a.SpoofAs)
		}
		spoofedBy, p = p.Name, c
		chatID, kind = "dm:"+c.Name, "direct"
	}

	text := a.Text
	if kind == "group" {
		var ok bool
		if ok, text = ch.ShouldRespondInGroup(a.Mention, text); !ok {
			w.log("chat.in", chatID, map[string]any{"as": a.As, "text": a.Text, "ignored": "group trigger"})
			return map[string]any{"chat": chatID, "delivered": false, "reason": "the group trigger ignores a message that does not mention the bot"}, nil
		}
	}
	snd := sender(p)
	content, meta := text, map[string]string{"user_id": snd.PlatformID, "username": p.Name}
	if a.ReplyTo != "" {
		meta["reply_to_message_id"] = a.ReplyTo
	}
	if a.ReplyTo != "" || a.Quote != "" {
		meta[channels.MetaOwnText] = text // the control path judges the sender's own words
	}
	if a.Quote != "" {
		content = "[quoted message]: " + a.Quote + "\n\n" + text
	}
	if a.Forwarded {
		meta[channels.MetaForwarded] = "true"
	}

	w.mu.Lock()
	w.inID++
	id := "in-" + strconv.FormatInt(w.inID, 10)
	w.mu.Unlock()
	allowed := ch.IsAllowedSender(snd) // HandleMessage drops a refused sender without a word
	w.log("chat.in", chatID, map[string]any{"id": id, "as": a.As, "sender": snd.CanonicalID, "text": a.Text,
		"reply_to": a.ReplyTo, "quote": a.Quote, "forwarded": a.Forwarded, "mention": a.Mention, "spoofed_by": spoofedBy,
		"allowed": allowed})
	if !allowed {
		return map[string]any{"id": id, "chat": chatID, "sender": snd.CanonicalID, "delivered": false,
			"reason": "the channel's allow_from refuses this sender"}, nil
	}
	ch.HandleMessage(ctx, bus.Peer{Kind: kind, ID: chatID}, id, snd.CanonicalID, chatID, content, nil, meta, snd)
	return map[string]any{"id": id, "chat": chatID, "sender": snd.CanonicalID, "delivered": true}, nil
}

func (w *World) outboxOp(_ context.Context, args json.RawMessage) (any, error) {
	var a struct {
		To            string
		Since         time.Time
		AfterSeq      int64 `json:"after_seq"`
		Limit         int
		IncludeFailed bool `json:"include_failed"`
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	if a.Limit < 0 {
		return nil, badArgs("limit must not be negative")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	msgs := []OutMsg{}
	for _, m := range w.out {
		if m.Seq > a.AfterSeq && (m.Delivered || a.IncludeFailed) && (a.To == "" || m.To == a.To) && !m.SimAt.Before(a.Since) {
			msgs = append(msgs, *m)
		}
	}
	msgs = msgs[max(0, len(msgs)-cmp.Or(a.Limit, 100)):]
	next := a.AfterSeq
	if n := len(msgs); n > 0 {
		next = msgs[n-1].Seq
	}
	return map[string]any{"messages": msgs, "next_seq": next}, nil
}

func (w *World) setStateOp(_ context.Context, args json.RawMessage) (any, error) {
	var a struct {
		State, Error string
		Delay        string
		PerMinute    int `json:"per_minute"`
		P            float64
		Seed         int64
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	s := chanState{kind: a.State, perMinute: a.PerMinute, p: a.P}
	switch a.State {
	case stateUp:
	case stateDown:
		switch s.err = channels.ErrTemporary; a.Error {
		case "", "temporary":
		case "send_failed":
			s.err = channels.ErrSendFailed
		default:
			return nil, badArgs("error must be temporary or send_failed")
		}
	case stateSlow:
		d, err := time.ParseDuration(a.Delay)
		if err != nil || d <= 0 {
			return nil, badArgs("slow needs a positive delay such as 2s")
		}
		s.delay = d
	case stateRateLimited:
		if a.PerMinute < 1 {
			return nil, badArgs("ratelimited needs per_minute >= 1")
		}
	case stateFlaky:
		if a.P < 0 || a.P > 1 {
			return nil, badArgs("flaky needs p between 0 and 1")
		}
		s.rng = rand.New(rand.NewSource(a.Seed)) //nolint:gosec // a seeded simulation, not security
	default:
		return nil, badArgs("state must be up, down, slow, ratelimited or flaky")
	}
	w.setState(s)
	w.log("chat.state", "", map[string]any{"state": a.State, "delay": a.Delay, "per_minute": a.PerMinute,
		"p": a.P, "seed": a.Seed, "error": a.Error})
	return map[string]string{"state": a.State}, nil
}
