package control

import (
	"context"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Feedback is the part of *feedback.Service the control path records through. It is
// nil while learning is off, and then nothing here records and every reply reads as
// it did before the store existed.
type Feedback interface {
	Record(ctx context.Context, r feedback.Response) (bool, error)
	Async(f func(ctx context.Context))
}

// The reactions that mean something. Any other emoji is ignored.
const (
	reactionUseful    = "\U0001F44D" // thumbs up
	reactionNotUseful = "\U0001F44E" // thumbs down
)

var (
	_ channels.FeedbackObserver = (*Dispatcher)(nil)
)

// feedbackOwner reports whether in comes from the owner by the control path's own
// rule: the configured owner writing in the owner chat, or the authenticated web
// launcher chat (standing), with proactive enabled. A response from anybody else is
// not recorded.
func (d *Dispatcher) feedbackOwner(deps *Deps, in Input) bool {
	return deps != nil && deps.Feedback != nil && deps.Cfg != nil && deps.Cfg.Proactive.Enabled &&
		d.standing(deps, in) == standOwner
}

// feedbackSource is "<channel>:<path>", or "web:<path>" for the web launcher chat.
func feedbackSource(deps *Deps, in Input, path string) string {
	ch := strings.ToLower(strings.TrimSpace(in.Channel))
	if isWebOwner(deps, in) {
		ch = "web"
	}
	return ch + ":" + path
}

// record stores one response of the owner to an intervention. A failure is logged and
// swallowed: the command the owner gave still answers.
func (d *Dispatcher) record(ctx context.Context, deps *Deps, in Input, interventionID string,
	kind feedback.Kind, value, path, event string,
) {
	if deps == nil || deps.Feedback == nil || interventionID == "" {
		return
	}
	_, err := deps.Feedback.Record(ctx, feedback.Response{
		InterventionID: interventionID, Kind: kind, Value: value,
		Source: feedbackSource(deps, in, path), Event: event, At: d.now(),
	})
	if err != nil {
		logger.WarnCF("control", "Could not record the owner's response", map[string]any{"kind": string(kind), "error": err.Error()})
	}
}

// tapOrCommand is the path and the event of a response given through a command: a
// button press is keyed by the message the button was on (so a double tap is one
// response), a typed command by the owner's own message.
func tapOrCommand(in Input, detail string) (path, event string) {
	if in.Callback != nil {
		return "button", in.ReplyToMessageID + ":" + detail
	}
	return "command", in.MessageID + ":" + detail
}

// interventionOf maps the message in replies to (or a button was on) to the
// intervention that sent it. ok is false when it is not one of the alerts.
func (d *Dispatcher) interventionOf(ctx context.Context, deps *Deps, channel, messageID string) (string, bool) {
	if deps.Sent == nil || messageID == "" {
		return "", false
	}
	id, ok, err := deps.Sent.LookupByPlatformMessageID(ctx, channel, messageID)
	if err != nil {
		logger.WarnCF("control", "Could not look up the message a response points at", map[string]any{"error": err.Error()})
		return "", false
	}
	return id, ok
}

// ObserveReply records a reply of the owner to a proactive message. in.Text is
// classified here and dropped: only the class goes to the database, and the message
// still goes to the agent unchanged (the channel calls this beside publishing it).
// It returns at once; the lookup and the write run on a goroutine of the feedback
// service.
func (d *Dispatcher) ObserveReply(_ context.Context, in Input) {
	deps := d.deps.Load()
	if in.ReplyToMessageID == "" || in.Forwarded || in.Callback != nil || !d.feedbackOwner(deps, in) || deps.Sent == nil {
		return
	}
	class := feedback.ClassifyReply(in.Text)
	in.Text = ""
	deps.Feedback.Async(func(ctx context.Context) {
		if id, ok := d.interventionOf(ctx, deps, in.Channel, in.ReplyToMessageID); ok {
			d.record(ctx, deps, in, id, feedback.KindReply, class, "reply", in.MessageID)
		}
	})
}

// ObserveReaction records a thumbs up or down by the owner on a proactive message as
// useful or not_useful, with the reaction as its source. Any other emoji is ignored.
func (d *Dispatcher) ObserveReaction(ctx context.Context, in Input, emoji string) {
	deps := d.deps.Load()
	kind := feedback.KindUseful
	switch strings.TrimSpace(emoji) {
	case reactionUseful:
	case reactionNotUseful:
		kind = feedback.KindNotUseful
	default:
		return
	}
	if !d.feedbackOwner(deps, in) {
		return
	}
	msg := in.ReplyToMessageID
	if id, ok := d.interventionOf(ctx, deps, in.Channel, msg); ok {
		d.record(ctx, deps, in, id, kind, "", "reaction", msg+":"+strings.TrimSpace(emoji))
	}
}
