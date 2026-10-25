package slack

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Block Kit buttons.
//
// A message with bus.Actions carries its text as section blocks followed by one
// actions block of buttons, on its last message. A tap arrives over socket mode as
// an interactive event of type block_actions, which is a control input exactly
// like a typed /approve: it goes to the channel's control router (pkg/control) and
// to nothing else. It never becomes an inbound message, so no model ever sees it.
//
// The authority of a tap is the payload's own user.id, as Slack reports it.
// Nothing in a button names a sender, a chat or an outcome: it carries an action
// and an opaque payload (the shared kq1|<action>|<payload> codec), and the
// approval service checks the payload against the stored proposal.
//
// Which field carries what: the button's value is the whole codec string
// ("kq1|apv|7F3K.abcd1234"), because the value is Slack's field for the opaque data
// of a button (2000 bytes) and the strict parser then sees exactly what the other
// channels send; the action_id is "kq1_<code>", a stable identifier that is unique
// within the block, lets a handler tell this bot's buttons from any other
// component by prefix, and must agree with the code inside the value.

const (
	// actionIDPrefix starts the action_id of every button this channel sends.
	actionIDPrefix = "kq1_"
	// actionsBlockID is the block_id of the actions block.
	actionsBlockID = "kq1_actions"

	// maxButtonValueBytes and maxActionIDBytes are Slack's limits on a button's
	// value and action_id.
	maxButtonValueBytes = 2000
	maxActionIDBytes    = 255
	// maxButtonTextRunes is Slack's limit on a button's text.
	maxButtonTextRunes = 75
	// maxActionsPerBlock is Slack's limit on the elements of an actions block.
	maxActionsPerBlock = 25
	// maxSectionTextRunes is Slack's limit on the text of a section block.
	maxSectionTextRunes = 3000

	// tapTimeout bounds the Web API calls that answer a tap.
	tapTimeout = 15 * time.Second
)

// SupportsActions implements channels.ActionRenderer: bus.Action buttons are
// shown as Block Kit buttons. The text fallback of the actions stays in the
// message body (see SendWithIDs).
func (c *SlackChannel) SupportsActions() bool { return true }

// buildActionsBlock builds the actions block of a message's actions. It is all or
// nothing: one action that cannot be encoded (or too many) fails the block,
// because Approve without Reject is worse than neither. The caller sends the
// message without buttons then; the text fallback is in the message body.
func buildActionsBlock(actions []bus.Action) (*slack.ActionBlock, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	if len(actions) > maxActionsPerBlock {
		return nil, fmt.Errorf("%d actions, the limit is %d", len(actions), maxActionsPerBlock)
	}
	seen := make(map[string]bool, len(actions))
	elements := make([]slack.BlockElement, 0, len(actions))
	for _, a := range actions {
		value, err := channels.EncodeCallbackData(a, maxButtonValueBytes)
		if err != nil {
			return nil, err
		}
		code, _ := channels.CallbackCode(a.ID) // EncodeCallbackData has checked it
		actionID := actionIDPrefix + code
		if len(actionID) > maxActionIDBytes {
			return nil, fmt.Errorf("action %q: action_id is %d bytes, the limit is %d", a.ID, len(actionID), maxActionIDBytes)
		}
		if seen[actionID] {
			// Slack refuses an actions block whose action_ids repeat.
			return nil, fmt.Errorf("action %q appears twice", a.ID)
		}
		seen[actionID] = true
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = a.ID
		}
		btn := slack.NewButtonBlockElement(
			actionID, value,
			slack.NewTextBlockObject(slack.PlainTextType, channels.TruncateRunes(label, maxButtonTextRunes), false, false),
		)
		switch a.Style {
		case "primary", "success":
			btn = btn.WithStyle(slack.StylePrimary)
		case "danger":
			btn = btn.WithStyle(slack.StyleDanger)
		}
		elements = append(elements, btn)
	}
	return slack.NewActionBlock(actionsBlockID, elements...), nil
}

// sectionBlocks lays text out as section blocks of at most 3000 characters.
func sectionBlocks(text string) []slack.Block {
	var blocks []slack.Block
	for _, part := range channels.SplitMessage(text, maxSectionTextRunes) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, part, false, false), nil, nil))
	}
	return blocks
}

// handleInteractive acknowledges an interactive event at once (Slack drops the
// tap if the envelope is not acknowledged within 3 seconds) and then handles it
// off the event loop, which must not wait for the control path.
func (c *SlackChannel) handleInteractive(event socketmode.Event) {
	if event.Request != nil && c.ackFn != nil {
		if err := c.ackFn(*event.Request); err != nil {
			logger.WarnCF("slack", "Could not acknowledge an interactive event", map[string]any{"error": err.Error()})
		}
	}
	cb, ok := event.Data.(slack.InteractionCallback)
	if !ok || cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	go c.handleBlockActions(cb)
}

// handleBlockActions handles one tap.
func (c *SlackChannel) handleBlockActions(cb slack.InteractionCallback) {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	channelID := cb.Channel.ID
	if channelID == "" {
		channelID = cb.Container.ChannelID
	}
	userID := cb.User.ID
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("slack", "A block action handler panicked", map[string]any{"panic": fmt.Sprint(rec)})
			c.tellOnlyYou(ctx, channelID, userID, "", channels.TapFailed)
		}
	}()

	// Only this bot's own buttons are taps; any other block action is not ours.
	var action *slack.BlockAction
	for _, a := range cb.ActionCallback.BlockActions {
		if a != nil && strings.HasPrefix(a.ActionID, actionIDPrefix) {
			action = a
			break
		}
	}
	if action == nil {
		return
	}
	if userID == "" || channelID == "" {
		logger.DebugC("slack", "Block action without a user or a channel ignored")
		return
	}
	sender := slackSender(userID)
	messageTS := cb.Container.MessageTs
	if messageTS == "" {
		messageTS = cb.Message.Timestamp
	}
	// A message in a thread is addressed channel/thread, as the message path forms
	// it. The parent of a thread reports its own ts as thread_ts; that is not a
	// thread to reply into.
	threadTS := cb.Message.ThreadTimestamp
	if threadTS == "" {
		threadTS = cb.Container.ThreadTs
	}
	if threadTS == messageTS {
		threadTS = ""
	}

	// The allow-list applies to a tap exactly as to a message, and comes first:
	// somebody who may not talk to the bot gets "Not authorized." and nothing else,
	// not even a verdict on whether their data was well formed.
	if !c.IsAllowedSender(sender) {
		logger.DebugCF("slack", "Block action rejected by allowlist", map[string]any{"user_id": userID})
		c.tellOnlyYou(ctx, channelID, userID, threadTS, channels.TapNotAuthorized)
		return
	}

	actionID, payload, err := channels.ParseCallbackData(action.Value, maxButtonValueBytes)
	if err == nil {
		if code, _ := channels.CallbackCode(actionID); action.ActionID != actionIDPrefix+code {
			err = fmt.Errorf("action_id does not match the value")
		}
	}
	if err != nil {
		logger.DebugCF("slack", "Dropped a malformed block action", map[string]any{"error": err.Error()})
		c.tellOnlyYou(ctx, channelID, userID, threadTS, channels.TapInvalidButton)
		return
	}

	peerKind := "channel"
	if isDMChannel(channelID) {
		peerKind = "direct"
	}
	chatID := channelID
	if threadTS != "" {
		chatID = channelID + "/" + threadTS
	}
	in := channels.NewTapInput("slack", chatID, messageTS, peerKind, sender, channels.ControlCallback{
		Action: actionID, Payload: payload, CallbackID: cb.TriggerID,
	})

	out := channels.RouteTap(ctx, c.ControlRouter(), in)
	text := out.Words()
	if text == "" {
		return
	}
	if out.Edit && strings.TrimSpace(out.Text) != "" && messageTS != "" {
		// The card becomes the outcome: new text, and an empty block list removes
		// the buttons.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tapTimeout)
		defer cancel()
		_, _, _, err := c.api.UpdateMessageContext(actx, channelID, messageTS,
			slack.MsgOptionText(text, false), slack.MsgOptionBlocks())
		if err == nil {
			return
		}
		// Fall through to a message only the tapper sees: the outcome must not be lost.
		logger.WarnCF("slack", "Could not update the card a button was on, answering instead",
			map[string]any{"error": err.Error()})
	}
	c.tellOnlyYou(ctx, channelID, userID, threadTS, text)
}

// tellOnlyYou says text in the channel in a message only userID sees.
func (c *SlackChannel) tellOnlyYou(ctx context.Context, channelID, userID, threadTS, text string) {
	if channelID == "" || userID == "" || strings.TrimSpace(text) == "" {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tapTimeout)
	defer cancel()
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	if _, err := c.api.PostEphemeralContext(actx, channelID, userID, opts...); err != nil {
		logger.WarnCF("slack", "Could not answer a block action", map[string]any{"error": err.Error()})
	}
}
