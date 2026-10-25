package channels

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	// actionFooterPrefix introduces the text fallback appended to the last
	// chunk of a message whose channel cannot render buttons.
	actionFooterPrefix = "\n\n↳ "
	actionFooterSep    = " · "
)

// DeliveryReceipt is the outcome of delivering one logical message, however
// many chunks it was split into.
type DeliveryReceipt struct {
	// Msg is the logical message as the manager sent it: Actions are already
	// stripped or validated, and the content is the original, un-chunked text.
	Msg bus.OutboundMessage
	// PlatformMessageIDs lists the platform IDs of every message created, in
	// send order. Empty when the channel does not report IDs. When a placeholder
	// was edited instead of sending, the edited message's ID is reported.
	PlatformMessageIDs []string
	// Err is the first chunk failure, or nil when every chunk was delivered.
	// IDs of chunks that did go out are still listed.
	Err error
	// At is when delivery finished.
	At time.Time
}

// SetDeliveryObserver registers fn to be told the outcome of every logical
// message, once, after its final retry. It runs on the delivering goroutine
// without any manager lock held. A panic in fn is recovered and logged. Pass nil
// to remove the observer.
func (m *Manager) SetDeliveryObserver(fn func(DeliveryReceipt)) {
	if fn == nil {
		m.observer.Store(nil)
		return
	}
	m.observer.Store(&fn)
}

// SetControlEnabled records whether the control-command path exists. Until it
// does, text commands in a footer would reach the LLM as ordinary chat, so the
// manager strips message actions while this is false.
func (m *Manager) SetControlEnabled(enabled bool) { m.controlEnabled.Store(enabled) }

// ControlEnabled reports whether message actions may be sent. It is false until
// the control-command dispatcher is registered.
func (m *Manager) ControlEnabled() bool { return m.controlEnabled.Load() }

// notifyObserver hands a receipt to the observer, shielding the caller from a
// panicking observer.
func (m *Manager) notifyObserver(receipt DeliveryReceipt) {
	fn := m.observer.Load()
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorCF("channels", "Delivery observer panic recovered", map[string]any{
				"channel": receipt.Msg.Channel,
				"panic":   fmt.Sprintf("%v", r),
				"stack":   string(debug.Stack()),
			})
		}
	}()
	(*fn)(receipt)
}

// SendAndReport delivers msg synchronously, in the caller's goroutine, and
// returns what happened. It runs the same pipeline as the queued path — same
// splitting, rate limiter, retry and adapter call — so it shares the channel's
// rate limiter with the worker. It does not wait for messages already queued
// for the channel. An unknown channel or one without a running worker yields a
// receipt carrying an error. The delivery observer is notified exactly once, as
// for any other message.
func (m *Manager) SendAndReport(ctx context.Context, msg bus.OutboundMessage) DeliveryReceipt {
	m.mu.RLock()
	_, exists := m.channels[msg.Channel]
	w, wExists := m.workers[msg.Channel]
	m.mu.RUnlock()

	var err error
	switch {
	case !exists:
		err = fmt.Errorf("channel %s not found", msg.Channel)
	case !wExists || w == nil:
		err = fmt.Errorf("channel %s has no active worker", msg.Channel)
	}
	if err != nil {
		receipt := DeliveryReceipt{Msg: msg, Err: err, At: time.Now()}
		m.notifyObserver(receipt)
		return receipt
	}

	return m.deliver(ctx, msg.Channel, w, msg, true)
}

type skipPreSendKey struct{}

// skipsPreSend reports whether ctx marks a delivery that must leave the typing
// indicator, reaction and placeholder of the chat alone.
func skipsPreSend(ctx context.Context) bool {
	v, _ := ctx.Value(skipPreSendKey{}).(bool)
	return v
}

// deliver is the one delivery pipeline, shared by the queued worker,
// SendMessage and SendAndReport: sanitise the action buttons, split the
// message, put Actions and InterventionID on the last chunk only, add the text
// fallback footer where buttons cannot render, send every chunk with rate
// limiting and retry, and finally tell the observer once.
//
// markerSplit selects whether the model's split marker is honoured in addition
// to the channel's length limit (the queued path does; SendMessage never did).
func (m *Manager) deliver(
	ctx context.Context, name string, w *channelWorker, msg bus.OutboundMessage, markerSplit bool,
) DeliveryReceipt {
	msg = m.sanitizeActions(name, msg)

	maxLen := 0
	if mlp, ok := w.ch.(MessageLengthProvider); ok {
		maxLen = mlp.MaxMessageLength()
	}

	// Two independent splits, applied in order:
	//  1. the marker, which is the model saying "these are separate
	//     messages" — a semantic boundary;
	//  2. the channel's length limit, which is a transport constraint.
	// Length splitting runs on each marker part, so a long part is still
	// chunked; doing it the other way round would let a length split
	// cut across a marker and merge two intended messages.
	var chunks []string
	if markerSplit {
		chunks = m.splitOutbound(msg.Content, maxLen)
	} else {
		chunks = splitByLength(msg.Content, maxLen)
	}

	if len(msg.Actions) > 0 && !rendersActions(w.ch) {
		footer := actionFooter(msg.Actions)
		if maxLen > 0 && runeLen(footer) > maxLen/2 {
			// All or nothing: offering Approve without Reject is worse than
			// offering neither. The message goes out without actions, and the
			// receipt says so.
			logger.WarnCF("channels", "Action footer does not fit the channel's length limit, sent without actions",
				map[string]any{
					"channel":         name,
					"intervention_id": msg.InterventionID,
					"actions":         len(msg.Actions),
				})
			msg.Actions = nil
		} else {
			chunks = appendActionFooter(chunks, footer, maxLen)
		}
	}

	// A proactive message is not the answer a typing indicator or placeholder
	// is waiting for. Decide on the whole message: its early chunks have
	// already lost InterventionID.
	if msg.IsProactive() || len(msg.Actions) > 0 {
		ctx = context.WithValue(ctx, skipPreSendKey{}, true)
	}

	last := len(chunks) - 1
	var ids []string
	var firstErr error
	for i, part := range chunks {
		partMsg := msg
		partMsg.Content = part
		if i < last {
			// Buttons and the intervention link belong to the end of the
			// message, not to every piece of it.
			partMsg.Actions = nil
			partMsg.InterventionID = ""
		}
		sent, err := m.sendWithRetry(ctx, name, w, partMsg)
		ids = append(ids, sent...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}

	receipt := DeliveryReceipt{Msg: msg, PlatformMessageIDs: ids, Err: firstErr, At: time.Now()}
	m.notifyObserver(receipt)
	return receipt
}

// sanitizeActions removes actions the manager must not send: all of them while
// the control path does not exist, and all of them when they break the limits.
// A malformed button must not fail the alert it belongs to, so the message
// itself always goes out. Only IDs and counts are logged above DEBUG.
func (m *Manager) sanitizeActions(name string, msg bus.OutboundMessage) bus.OutboundMessage {
	if len(msg.Actions) == 0 {
		return msg
	}
	fields := map[string]any{
		"channel":         name,
		"intervention_id": msg.InterventionID,
		"actions":         len(msg.Actions),
	}
	if !m.ControlEnabled() {
		logger.DebugCF("channels", "Control path disabled, stripped message actions", fields)
		msg.Actions = nil
		return msg
	}
	if reason := actionLimitViolation(msg.Actions); reason != "" {
		fields["reason"] = reason
		logger.WarnCF("channels", "Invalid message actions dropped, sending message without them", fields)
		msg.Actions = nil
	}
	return msg
}

// actionLimitViolation names the first limit the actions break, or "".
func actionLimitViolation(actions []bus.Action) string {
	if len(actions) > bus.MaxActions {
		return "too many actions"
	}
	for _, a := range actions {
		if len(a.Payload) > bus.MaxActionPayloadBytes {
			return "action payload too long"
		}
	}
	return ""
}

// rendersActions reports whether ch shows bus.Action as real buttons.
func rendersActions(ch Channel) bool {
	r, ok := ch.(ActionRenderer)
	return ok && r.SupportsActions()
}

// actionFooter builds the text fallback for actions: the prefix followed by the
// non-empty TextCommands joined with a separator, or "" when there are none.
// Whitespace inside a command is collapsed, so a command cannot add line breaks.
func actionFooter(actions []bus.Action) string {
	cmds := make([]string, 0, len(actions))
	for _, a := range actions {
		if cmd := strings.Join(strings.Fields(a.TextCommand), " "); cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == 0 {
		return ""
	}
	return actionFooterPrefix + strings.Join(cmds, actionFooterSep)
}

// appendActionFooter adds footer to the last chunk. When that would push the
// chunk past maxLen the last chunk is re-split to leave room, so the footer is
// never split and always ends the final chunk.
func appendActionFooter(chunks []string, footer string, maxLen int) []string {
	if footer == "" || len(chunks) == 0 {
		return chunks
	}
	last := chunks[len(chunks)-1]
	if last == "" {
		footer = strings.TrimLeft(footer, "\n")
	}
	if maxLen > 0 && runeLen(last)+runeLen(footer) > maxLen {
		parts := SplitMessage(last, maxLen-runeLen(footer))
		if len(parts) == 0 {
			parts = []string{""}
		}
		chunks = append(chunks[:len(chunks)-1:len(chunks)-1], parts...)
		last = chunks[len(chunks)-1]
	}
	chunks[len(chunks)-1] = last + footer
	return chunks
}

func runeLen(s string) int { return len([]rune(s)) }
