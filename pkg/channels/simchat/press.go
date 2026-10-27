package simchat

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// Argument schemas of press and react (see ops.go for the shared ones).
const (
	argMessage = `"message":{"type":"string","description":"id of the message, e.g. sc-3"}`
	argOn      = `"on":{"type":"object","description":"newest message matching {to, severity, contains[], intervention_id}, instead of message"}`
)

// onSpec picks the newest delivered message that matches every field it sets.
type onSpec struct {
	To, Severity   string
	Contains       []string
	InterventionID string `json:"intervention_id"`
}

// find returns a copy of the message id names, or the newest one on matches (that has
// a button for action, when action is set).
func (w *World) find(id string, on *onSpec, action string) (OutMsg, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id != "" {
		if m := w.byID[id]; m != nil {
			return *m, nil
		}
		return OutMsg{}, badArgs("no message %q in the outbox", id)
	}
	if on == nil {
		return OutMsg{}, badArgs("message or on is required")
	}
	for i := len(w.out) - 1; i >= 0; i-- {
		m := w.out[i]
		if !m.Delivered || (on.To != "" && m.To != on.To) || (on.Severity != "" && m.Severity != on.Severity) ||
			(on.InterventionID != "" && m.InterventionID != on.InterventionID) || (action != "" && m.button(action) == nil) {
			continue
		}
		if !containsAll(m.Text, on.Contains) {
			continue
		}
		return *m, nil
	}
	return OutMsg{}, badArgs("no delivered message matches on")
}

func containsAll(text string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(text, p) {
			return false
		}
	}
	return true
}

func (m *OutMsg) button(action string) *Button {
	for i := range m.Buttons {
		if m.Buttons[i].Action == action {
			return &m.Buttons[i]
		}
	}
	return nil
}

// actor resolves who taps or reacts on m: as, or the owner of the DM the message is in.
// It takes the persona's sender and the peer kind of the chat.
func (w *World) actor(as string, m OutMsg) (bus.SenderInfo, string, error) {
	if as == "" {
		as = strings.TrimPrefix(m.To, "dm:")
	}
	p, _, kind, err := w.route(as, m.To)
	if err != nil {
		return bus.SenderInfo{}, "", badArgs("%v", err)
	}
	return sender(p), kind, nil
}

// pressOp taps a button as a client would: the callback data the message was sent with
// goes through the allow-list and then only to the control router, never to the agent.
func (w *World) pressOp(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		Message, Action, As string
		On                  *onSpec
		Force               bool
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	ch := w.Attached()
	if ch == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "the simchat channel is not running in this instance")
	}
	m, err := w.find(a.Message, a.On, a.Action)
	if err != nil {
		return nil, err
	}
	btn := m.button(a.Action)
	if btn == nil {
		return nil, badArgs("message %s has no %q button", m.ID, a.Action)
	}
	if !m.ButtonsLive && !a.Force {
		return nil, badArgs("the buttons of %s were removed; force:true taps a stale keyboard", m.ID)
	}
	snd, kind, err := w.actor(a.As, m)
	if err != nil {
		return nil, err
	}

	var out channels.TapOutcome
	actionID, payload, perr := channels.ParseCallbackData(btn.Data, maxButtonData)
	switch {
	case !ch.IsAllowedSender(snd): // RouteTap does no allow-list check; the channel does, first
		out = channels.TapOutcome{Toast: channels.TapNotAuthorized}
	case perr != nil:
		out = channels.TapOutcome{Toast: channels.TapInvalidButton}
	default:
		w.mu.Lock()
		w.cbID++
		cb := "cb-" + strconv.FormatInt(w.cbID, 10)
		w.mu.Unlock()
		out = channels.RouteTap(ctx, ch.ControlRouter(), channels.NewTapInput(channelName, m.To, m.ID, kind, snd,
			channels.ControlCallback{Action: actionID, Payload: payload, CallbackID: cb}))
	}

	// What follows a tap, as Telegram's deliverCallbackReply does it: edit the card when
	// the reply asks for that, else say the full reply beside it unless the toast was all of it.
	text, edited := strings.TrimSpace(out.Text), false
	if out.Edit && text != "" {
		edited = ch.EditMessage(ctx, m.To, m.ID, out.Text) == nil
	}
	if !edited && text != "" && text != strings.TrimSpace(out.Toast) {
		_, _ = ch.SendWithIDs(ctx, bus.OutboundMessage{Channel: channelName, ChatID: m.To, Content: out.Text, ReplyToMessageID: m.ID})
	}
	res := map[string]any{"message": m.ID, "action": a.Action, "as": snd.CanonicalID,
		"toast": out.Toast, "text": out.Text, "edit": edited}
	w.log("chat.press", m.To, res)
	return res, nil
}

// reactOp reacts to a message as a persona: the allow-list applies as for a message,
// then the control router's feedback side is told, as the Telegram adapter does.
func (w *World) reactOp(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		Message, Emoji, As string
		On                 *onSpec
	}
	if err := decode(args, &a); err != nil {
		return nil, err
	}
	ch := w.Attached()
	if ch == nil {
		return nil, lab.Errorf(lab.CodeUnavailable, "the simchat channel is not running in this instance")
	}
	m, err := w.find(a.Message, a.On, "")
	if err != nil {
		return nil, err
	}
	snd, kind, err := w.actor(a.As, m)
	if err != nil {
		return nil, err
	}
	emoji := a.Emoji
	switch emoji {
	case "up":
		emoji = "\U0001F44D"
	case "down":
		emoji = "\U0001F44E"
	case "":
		return nil, badArgs("emoji is required")
	}
	allowed := ch.IsAllowedSender(snd)
	if allowed {
		ch.ObserveReaction(ctx, channels.ControlInput{Channel: channelName, ChatID: m.To, MessageID: m.ID,
			ReplyToMessageID: m.ID, Sender: snd, SenderID: snd.CanonicalID, PeerKind: kind}, emoji)
	}
	res := map[string]any{"message": m.ID, "emoji": emoji, "as": snd.CanonicalID, "allowed": allowed}
	w.log("chat.react", m.To, res)
	return res, nil
}
