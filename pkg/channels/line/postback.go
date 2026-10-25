package line

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Quick-reply buttons.
//
// A message with bus.Actions carries a quickReply on its last message: one
// "postback" item per action. A tap arrives as a postback event, which is a
// control input exactly like a typed /approve: it goes to the channel's control
// router (pkg/control) and to nothing else. It never becomes an inbound message,
// so no model ever sees it.
//
// The authority of a tap is the event's own source.userId, as LINE reports it.
// Nothing in the postback data names a sender, a chat or an outcome: the data
// carries an action and an opaque payload (the shared kq1|<action>|<payload>
// codec), and the approval service checks the payload against the stored proposal.
//
// The webhook signature (X-Line-Signature) is checked per request over the whole
// body before any event in it is parsed, so it covers postback events like every
// other type; there is no second check to forget.

const (
	// maxPostbackDataBytes is LINE's limit on the data of a postback action.
	maxPostbackDataBytes = 300
	// maxQuickReplyItems is LINE's limit on the items of a quickReply.
	maxQuickReplyItems = 13
	// maxQuickReplyLabelRunes is LINE's limit on the label of a quick-reply item.
	maxQuickReplyLabelRunes = 20
	// maxDisplayTextRunes is LINE's limit on a postback action's displayText.
	maxDisplayTextRunes = 300

	// postbackReplyTimeout bounds the call that answers a tap.
	postbackReplyTimeout = 10 * time.Second
)

// buildQuickReply builds the quickReply object of a message's actions. It is all
// or nothing: one action that cannot be encoded (or too many) fails it, because
// Approve without Reject is worse than neither. The caller sends the message
// without buttons then; the text fallback is in the message body.
func buildQuickReply(actions []bus.Action) (map[string]any, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	if len(actions) > maxQuickReplyItems {
		return nil, fmt.Errorf("%d actions, the limit is %d", len(actions), maxQuickReplyItems)
	}
	items := make([]map[string]any, 0, len(actions))
	for _, a := range actions {
		data, err := channels.EncodeCallbackData(a, maxPostbackDataBytes)
		if err != nil {
			return nil, err
		}
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = a.ID
		}
		items = append(items, map[string]any{
			"type": "action",
			"action": map[string]any{
				"type":  "postback",
				"label": channels.TruncateRunes(label, maxQuickReplyLabelRunes),
				"data":  data,
				// What the tap shows in the chat as the user's own message.
				"displayText": channels.TruncateRunes(label, maxDisplayTextRunes),
			},
		})
	}
	return map[string]any{"items": items}, nil
}

// processPostback handles one tap. The webhook already answered 200 for the
// request, so the work here may take as long as the control path needs.
func (c *LINEChannel) processPostback(event lineEvent) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("line", "A postback handler panicked", map[string]any{"panic": fmt.Sprint(rec)})
		}
	}()

	userID := event.Source.UserID
	if userID == "" {
		// LINE leaves the user out of a group event when it cannot say who it is.
		// Without a sender there is nobody to hold to the owner rule.
		logger.DebugC("line", "Postback without a user id ignored")
		return
	}
	sender := lineSender(userID)
	chatID := c.resolveChatID(event.Source)

	// The allow-list applies to a tap exactly as to a message, and comes first.
	// LINE has no toast and a refusal here must not tell a stranger anything, so
	// a sender who may not talk to the bot gets no reply at all.
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("line", "Postback rejected by allowlist", map[string]any{"user_id": userID})
		return
	}

	data := ""
	if event.Postback != nil {
		data = event.Postback.Data
	}
	actionID, payload, err := channels.ParseCallbackData(data, maxPostbackDataBytes)
	if err != nil {
		logger.DebugCF("line", "Dropped a malformed postback", map[string]any{"error": err.Error()})
		c.answerPostback(ctx, event, chatID, channels.TapInvalidButton)
		return
	}

	peerKind := "direct"
	if isGroupSource(event.Source) {
		peerKind = "group"
	}
	// LINE does not say which message carried the button; quick replies show on
	// the newest message only, so it is the latest one sent with buttons.
	cardID := ""
	if v, ok := c.lastCard.Load(chatID); ok {
		cardID, _ = v.(string)
	}
	in := channels.NewTapInput("line", chatID, cardID, peerKind, sender, channels.ControlCallback{
		Action: actionID, Payload: payload, CallbackID: event.WebhookEventID,
	})

	out := channels.RouteTap(ctx, c.ControlRouter(), in)
	text := out.Words()
	if out.Edit {
		// LINE cannot edit a message that was sent: the outcome goes out as a short
		// reply instead.
		text = out.OutcomeLine()
	}
	c.answerPostback(ctx, event, chatID, text)
}

// answerPostback says text in the chat the tap came from: with the event's reply
// token (free, and what LINE expects), or pushed if the token no longer works.
func (c *LINEChannel) answerPostback(ctx context.Context, event lineEvent, chatID, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	text = channels.TruncateRunes(text, maxMessageLength)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postbackReplyTimeout)
	defer cancel()
	messages := []map[string]any{buildTextMessage(text, "")}
	if event.ReplyToken != "" {
		if _, err := c.sendReply(actx, event.ReplyToken, messages); err == nil {
			return
		}
		logger.DebugC("line", "Reply API failed for a postback, falling back to Push API")
	}
	if _, err := c.sendPush(actx, chatID, messages); err != nil {
		logger.WarnCF("line", "Could not answer a postback", map[string]any{"error": err.Error()})
	}
}

// SupportsActions implements channels.ActionRenderer: bus.Action buttons are
// shown as quick-reply buttons. The text fallback of the actions stays in the
// message body (see SendWithIDs).
func (c *LINEChannel) SupportsActions() bool { return true }
