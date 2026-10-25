package channels

import (
	"context"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
)

// Taps.
//
// A tap on a button (a LINE postback, a Discord component interaction, a Slack
// block action) is a control input exactly like a typed /approve: it goes to the
// channel's ControlRouter and to nothing else. It is never published as an
// inbound message, so no model sees it. The helpers here are what those channels
// share; Telegram's callback handler predates them and keeps its own.
//
// The authority of a tap is the platform's own sender field on the interaction,
// made canonical the way the channel's message path makes a sender. Nothing in
// the callback data names a sender, a chat or an outcome.

// Words the channel itself says when a tap does not reach the control path (the
// control path's own words are used for everything it handles).
const (
	TapNotAuthorized = "Not authorized."
	TapInvalidButton = "That button is not valid."
	TapUnavailable   = "Buttons are not available right now."
	TapFailed        = controlFailedText
)

// TapOutcome is what a tap decided: the short words for the acknowledgement, the
// control path's full reply, and what to do with the card.
type TapOutcome struct {
	// Toast is the short acknowledgement ("" is possible only for a router that
	// said nothing at all).
	Toast string
	// Text is the control path's full reply, "" when there is none.
	Text string
	// Edit asks for the card to be replaced by the outcome instead of answering
	// beside it.
	Edit bool
}

// Words is what a channel with no separate toast says: the full reply, else the
// toast.
func (o TapOutcome) Words() string {
	if t := strings.TrimSpace(o.Text); t != "" {
		return t
	}
	return o.Toast
}

// OutcomeLine is the first line of Words: the short outcome a channel that cannot
// edit a card says instead.
func (o TapOutcome) OutcomeLine() string { return FirstLine(o.Words()) }

// NewTapInput builds the control input of a tap. For a button, the message it was
// on is both the message a reply goes to and the one a snooze or a rating is
// about, so messageID fills MessageID and ReplyToMessageID alike. It may be empty
// when the platform does not say which message carried the button.
func NewTapInput(
	channel, chatID, messageID, peerKind string, sender bus.SenderInfo, cb ControlCallback,
) ControlInput {
	return ControlInput{
		Channel:          channel,
		ChatID:           chatID,
		MessageID:        messageID,
		ReplyToMessageID: messageID,
		Sender:           sender,
		SenderID:         sender.CanonicalID,
		PeerKind:         peerKind,
		Callback:         &cb,
	}
}

// RouteTap offers a tap to the router and decides what it means. A router that is
// missing, panics or does not know the action never lets the tap go anywhere
// else; every outcome carries the words to say. It does no I/O of its own.
func RouteTap(ctx context.Context, r ControlRouter, in ControlInput) TapOutcome {
	if r == nil {
		return TapOutcome{Toast: TapUnavailable}
	}
	reply, handled, panicked := callRouter(ctx, r, in)
	switch {
	case panicked:
		return TapOutcome{Toast: TapFailed}
	case !handled:
		// The router does not know this action (or the payload is not one it
		// accepts): it is dropped, never given to the agent.
		return TapOutcome{Toast: TapInvalidButton}
	}
	// The control path fills AnswerCallback in for a command it ran; a refusal
	// before that (not the owner, rate limited, wrong chat) carries only Text. A
	// tap is never left without its words.
	toast := reply.AnswerCallback
	if toast == "" {
		toast = FirstLine(reply.Text)
	}
	return TapOutcome{Toast: toast, Text: reply.Text, Edit: reply.EditOriginal}
}

// FirstLine returns the first line of s, trimmed.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// TruncateRunes cuts s to at most max runes.
func TruncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
