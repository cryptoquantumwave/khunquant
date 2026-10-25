package channels

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// The control path.
//
// A handful of inputs act on the system itself: approving or rejecting an order
// proposal, snoozing an alert, stopping the assistant. They must work at once,
// even while the agent is in the middle of a long turn, and they must NEVER go
// through a model. BaseChannel.HandleMessage therefore offers every message that
// passed the allow-list to a ControlRouter before it does anything else: before
// the typing indicator, the reaction, the placeholder and PublishInbound.
//
// The types live here, not in pkg/control, because pkg/control imports the
// approval service, which imports pkg/notify, which imports this package; the
// router (pkg/control.Dispatcher) aliases them.

// Metadata keys a channel may set to tell the control path something the text
// alone cannot. HandleMessage removes them before it publishes the message, so a
// message that is not a control command reaches the agent exactly as it did
// before they existed.
const (
	// MetaOwnText holds the sender's own text when Content also carries text the
	// channel added around it (a quoted message prepended to a reply). Without
	// it, "yes" typed as a reply to a card could not be told from a long message.
	MetaOwnText = "control_own_text"
	// MetaForwarded is "true" when the message was forwarded from somewhere else.
	// A forwarded message is never a control command.
	MetaForwarded = "control_forwarded"
)

// metaReplyTo lists the metadata keys under which channels put the platform ID of
// the message the sender replied to, in the order they are tried.
var metaReplyTo = []string{"reply_to_message_id", "reply_to_msg_id"}

// ControlCallback is a button press.
type ControlCallback struct {
	// Action is the button's action ID ("approve", "reject", "snooze_1h", ...).
	Action string
	// Payload is the opaque payload the button carried.
	Payload    string
	CallbackID string
}

// ControlInput is one inbound message (or button press) offered to the control
// path.
type ControlInput struct {
	Channel, ChatID, MessageID string
	Sender                     bus.SenderInfo
	// SenderID is the canonical sender ("platform:id") when the channel gave
	// one, else the raw id.
	SenderID string
	// PeerKind is the peer kind the channel attached: "direct", "group",
	// "channel" or "".
	PeerKind string
	// Text is what the sender typed: for a reply, only their own words.
	Text             string
	ReplyToMessageID string
	// Forwarded marks a message the channel says was forwarded.
	Forwarded bool
	// Callback is set for a button press, and then Text is empty. For a button,
	// ReplyToMessageID is the message the button was on.
	Callback *ControlCallback
}

// ControlReply is what the control path says back. An empty Text sends nothing.
type ControlReply struct {
	Text string
	// EditOriginal asks the channel to edit the message a button was on instead
	// of sending a new one (channels that cannot, send a new one).
	EditOriginal bool
	// AnswerCallback is the short text for the button press acknowledgement.
	AnswerCallback string
}

// ControlRouter handles control inputs. pkg/control.Dispatcher is the one
// implementation.
type ControlRouter interface {
	// TryHandle returns (reply, true) when the input is a control command it has
	// dealt with, and false for everything else. It must return false quickly and
	// without I/O for an input it does not recognise.
	TryHandle(ctx context.Context, in ControlInput) (ControlReply, bool)
	// Recognises reports, with no I/O, whether in would be treated as a control
	// command. It is how a panic inside TryHandle is told apart: a command fails
	// closed, anything else carries on to the agent.
	Recognises(in ControlInput) bool
}

// FeedbackObserver is an optional interface of a ControlRouter: it is told about two
// inputs that are not control commands but are the owner's answer to a proactive
// message, so that they can be recorded. The channel checks for it with a type
// assertion, so a router without it is untouched. Both calls must return at once
// without waiting on I/O (they hand the work to a goroutine), and neither changes
// what the channel does next: a reply still goes to the agent exactly as it was.
type FeedbackObserver interface {
	// ObserveReply is called with the input of an inbound message that was not
	// handled as a control command. in.Text is the sender's own words, without the
	// quoted message the channel put in front of them, and in.ReplyToMessageID is the
	// message replied to.
	ObserveReply(ctx context.Context, in ControlInput)
	// ObserveReaction is called when a user reacts to a message (emoji is the reaction
	// as the platform names it). in.ReplyToMessageID and in.MessageID are the message
	// reacted to; in.Text is empty.
	ObserveReaction(ctx context.Context, in ControlInput, emoji string)
}

// observeReply tells the router's feedback side, if it has one, about a reply. It
// never panics into the caller and never changes the message.
func observeReply(ctx context.Context, r ControlRouter, in ControlInput) {
	fo, ok := r.(FeedbackObserver)
	if !ok || in.ReplyToMessageID == "" {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("channels", "The feedback observer panicked", map[string]any{"channel": in.Channel})
		}
	}()
	fo.ObserveReply(ctx, in)
}

// ObserveReaction offers a reaction to the control router's feedback side. It is for
// the channels that receive reactions; a router without one ignores it.
func (c *BaseChannel) ObserveReaction(ctx context.Context, in ControlInput, emoji string) {
	fo, ok := c.ControlRouter().(FeedbackObserver)
	if !ok {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("channels", "The feedback observer panicked", map[string]any{"channel": in.Channel})
		}
	}()
	fo.ObserveReaction(ctx, in, emoji)
}

type controlRouterBox struct{ r ControlRouter }

// SetControlRouter injects the router. nil removes it. Safe to call while the
// channel is running.
func (c *BaseChannel) SetControlRouter(r ControlRouter) {
	if r == nil {
		c.controlRouter.Store(nil)
		return
	}
	c.controlRouter.Store(&controlRouterBox{r: r})
}

// ControlRouter returns the injected router (may be nil). A channel that receives
// button presses calls TryHandle on it directly.
func (c *BaseChannel) ControlRouter() ControlRouter {
	if b := c.controlRouter.Load(); b != nil {
		return b.r
	}
	return nil
}

// controlReplyTimeout bounds the channel's Send of a control reply.
const controlReplyTimeout = 10 * time.Second

// controlFailedText is said when a command was recognised but could not be handled.
const controlFailedText = "That command could not be handled. Try again."

// takeControlHints removes the control hints from metadata (in place: the map is
// the one the channel just built) and returns them.
func takeControlHints(metadata map[string]string) (ownText string, hasOwn, forwarded bool) {
	if len(metadata) == 0 {
		return "", false, false
	}
	if v, ok := metadata[MetaOwnText]; ok {
		ownText, hasOwn = v, true
		delete(metadata, MetaOwnText)
	}
	if v, ok := metadata[MetaForwarded]; ok {
		forwarded = v == "true"
		delete(metadata, MetaForwarded)
	}
	return ownText, hasOwn, forwarded
}

// routeControl offers a message to the router and, when the router handled it,
// sends the reply through the channel's own Send. It reports whether the message
// was consumed. A router that panics never lets a control command through to the
// agent.
func (c *BaseChannel) routeControl(
	ctx context.Context, r ControlRouter, peer bus.Peer, messageID, senderID, chatID, content string,
	sender bus.SenderInfo, metadata map[string]string, ownText string, hasOwn, forwarded bool,
) bool {
	text := content
	if hasOwn {
		text = ownText
	}
	in := ControlInput{
		Channel: c.name, ChatID: chatID, MessageID: messageID,
		Sender: sender, SenderID: senderID, PeerKind: peer.Kind,
		Text: text, Forwarded: forwarded,
	}
	for _, k := range metaReplyTo {
		if v := metadata[k]; v != "" {
			in.ReplyToMessageID = v
			break
		}
	}

	reply, handled, panicked := callRouter(ctx, r, in)
	if panicked {
		if !routerRecognises(r, in) {
			return false // not a command: the agent gets it, as it always did
		}
		c.sendControlReply(ctx, in, ControlReply{Text: controlFailedText})
		return true
	}
	if !handled {
		observeReply(ctx, r, in)
		return false
	}
	c.sendControlReply(ctx, in, reply)
	return true
}

func callRouter(ctx context.Context, r ControlRouter, in ControlInput) (reply ControlReply, handled, panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("channels", "The control router panicked", map[string]any{
				"channel": in.Channel,
				"panic":   fmt.Sprint(rec),
			})
			reply, handled, panicked = ControlReply{}, false, true
		}
	}()
	reply, handled = r.TryHandle(ctx, in)
	return reply, handled, false
}

// routerRecognises asks the router whether in is a command. If it cannot answer,
// anything shaped like one counts as one.
func routerRecognises(r ControlRouter, in ControlInput) (is bool) {
	defer func() {
		if rec := recover(); rec != nil {
			is = in.Callback != nil || strings.HasPrefix(strings.TrimSpace(in.Text), "/")
		}
	}()
	return r.Recognises(in)
}

// sendControlReply says the reply in the chat the input came from, as a reply to
// it where the platform supports that.
func (c *BaseChannel) sendControlReply(ctx context.Context, in ControlInput, reply ControlReply) {
	if reply.Text == "" {
		return
	}
	msg := bus.OutboundMessage{
		Channel:          c.name,
		ChatID:           in.ChatID,
		Content:          reply.Text,
		ReplyToMessageID: in.MessageID,
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlReplyTimeout)
	defer cancel()
	var err error
	switch {
	case c.owner != nil:
		err = c.owner.Send(sendCtx, msg)
	case c.bus != nil:
		err = c.bus.PublishOutbound(sendCtx, msg)
	default:
		err = fmt.Errorf("no way to send")
	}
	if err != nil {
		logger.WarnCF("channels", "Could not send a control reply", map[string]any{
			"channel": c.name,
			"error":   err.Error(),
		})
	}
}
