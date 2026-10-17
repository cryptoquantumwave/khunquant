package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Inline buttons.
//
// A message with bus.Actions carries an inline keyboard on its last chunk. A tap
// arrives as a callback query, which is a control input exactly like a typed
// /approve: it goes to the channel's control router (pkg/control) and to nothing
// else. It never becomes an inbound message, so no model ever sees it.
//
// The authority of a tap is the callback's own from.id, as Telegram reports it.
// Nothing in the callback data names a sender, a chat or a decision's outcome:
// the data carries an action and an opaque payload, and the approval service
// checks the payload (a code and a nonce) against the stored proposal.

const (
	// maxCallbackDataBytes is Telegram's limit on callback_data.
	maxCallbackDataBytes = 64
	// maxButtonsPerRow keeps the buttons large enough to tap on a phone.
	maxButtonsPerRow = 3
	// maxToastRunes is Telegram's limit on the text of an answerCallbackQuery.
	maxToastRunes = 200

	// callbackAnswerTimeout bounds the call that answers a tap and the ones that
	// follow it; they run on a context that outlives a cancelled handler.
	callbackAnswerTimeout = 10 * time.Second
)

// Toasts the channel itself shows (the control path's own words are used for
// everything it handles).
const (
	toastNotAuthorized = "Not authorized."
	toastInvalidButton = "That button is not valid."
	toastUnavailable   = "Buttons are not available right now."
	toastFailed        = "That command could not be handled. Try again."
)

// callbackCodes maps an action ID to its short code in the callback data. The
// codec itself (kq1|<action>|<payload>) is shared by every channel that renders
// buttons and lives in pkg/channels; only Telegram's 64-byte limit is its own.
var callbackCodes = channels.CallbackCodes()

// encodeCallbackData builds "kq1|<action>|<payload>" for a button. A string over
// 64 bytes, an action the control path does not know and a payload that does not
// fit are programming errors: they are returned, never truncated.
func encodeCallbackData(a bus.Action) (string, error) {
	return channels.EncodeCallbackData(a, maxCallbackDataBytes)
}

// parseCallbackData is the strict inverse of encodeCallbackData.
func parseCallbackData(data string) (actionID, payload string, err error) {
	return channels.ParseCallbackData(data, maxCallbackDataBytes)
}

// buildKeyboard builds the inline keyboard of a message's actions, at most
// maxButtonsPerRow to a row. It is all or nothing: one action that cannot be
// encoded fails the keyboard, because Approve without Reject is worse than
// neither. The caller sends the message without a keyboard then; the text
// fallback is in the message body.
func buildKeyboard(actions []bus.Action) (*telego.InlineKeyboardMarkup, error) {
	if len(actions) == 0 {
		return nil, nil
	}
	buttons := make([]telego.InlineKeyboardButton, 0, len(actions))
	for _, a := range actions {
		data, err := encodeCallbackData(a)
		if err != nil {
			return nil, err
		}
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = a.ID
		}
		btn := tu.InlineKeyboardButton(label).WithCallbackData(data)
		switch a.Style {
		case "primary", "danger", "success":
			btn = btn.WithStyle(a.Style)
		}
		buttons = append(buttons, btn)
	}
	var rows [][]telego.InlineKeyboardButton
	for len(buttons) > 0 {
		n := min(len(buttons), maxButtonsPerRow)
		rows = append(rows, tu.InlineKeyboardRow(buttons[:n]...))
		buttons = buttons[n:]
	}
	return tu.InlineKeyboard(rows...), nil
}

// telegramSender is the sender of a message or a tap in the one canonical form
// ("telegram:<id>") the allow-list and the control path compare.
func telegramSender(user *telego.User) bus.SenderInfo {
	platformID := fmt.Sprintf("%d", user.ID)
	return bus.SenderInfo{
		Platform:    "telegram",
		PlatformID:  platformID,
		CanonicalID: identity.BuildCanonicalID("telegram", platformID),
		Username:    user.Username,
		DisplayName: user.FirstName,
	}
}

// chatIDWithThread is the chat id the rest of the system uses: "chatID", or
// "chatID/threadID" in a forum topic, so that a reply goes back to the topic.
func chatIDWithThread(chat telego.Chat, threadID int) string {
	if chat.IsForum && threadID != 0 {
		return fmt.Sprintf("%d/%d", chat.ID, threadID)
	}
	return fmt.Sprintf("%d", chat.ID)
}

// callbackOutcome is what a tap decided: the toast, and what to do with the card.
type callbackOutcome struct {
	toast string
	// text is the control path's full reply, "" when there is none.
	text string
	// edit asks for the card to be edited to the text instead of a new message.
	edit bool
}

// handleCallbackQuery handles one tap. Every tap is answered, whatever happens to
// it, because a client keeps its spinner until the query is answered.
func (c *TelegramChannel) handleCallbackQuery(ctx context.Context, q *telego.CallbackQuery) error {
	if q == nil {
		return errors.New("callback query is nil")
	}
	out := c.routeCallback(ctx, q)

	// The answer and what follows it must not die with a cancelled handler.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callbackAnswerTimeout)
	defer cancel()
	if err := c.bot.AnswerCallbackQuery(actx, &telego.AnswerCallbackQueryParams{
		CallbackQueryID: q.ID,
		Text:            truncateRunes(out.toast, maxToastRunes),
	}); err != nil {
		logger.WarnCF("telegram", "Could not answer a callback query", map[string]any{"error": err.Error()})
	}
	if out.text != "" {
		c.deliverCallbackReply(actx, q, out)
	}
	return nil
}

// routeCallback decides what a tap means. It does no network I/O of its own.
func (c *TelegramChannel) routeCallback(ctx context.Context, q *telego.CallbackQuery) (out callbackOutcome) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.ErrorCF("telegram", "A callback query handler panicked", map[string]any{"panic": fmt.Sprint(rec)})
			out = callbackOutcome{toast: toastFailed}
		}
	}()

	sender := telegramSender(&q.From)
	msg := q.Message
	chatType := ""
	if msg != nil {
		chatType = msg.GetChat().Type
	}

	// The allow-list applies to a tap exactly as to a message, and comes first:
	// somebody who may not talk to the bot gets "Not authorized." and nothing else,
	// not even a verdict on whether their data was well formed.
	if !c.senderAllowed(chatType, sender) {
		logger.DebugCF("telegram", "Callback query rejected by allowlist", map[string]any{"user_id": sender.PlatformID})
		return callbackOutcome{toast: toastNotAuthorized}
	}

	if msg == nil {
		// A button on an inline-mode message: not something this bot sends.
		return callbackOutcome{toast: toastInvalidButton}
	}
	actionID, payload, err := parseCallbackData(q.Data)
	if err != nil {
		logger.DebugCF("telegram", "Dropped a malformed callback", map[string]any{"error": err.Error()})
		return callbackOutcome{toast: toastInvalidButton}
	}
	router := c.ControlRouter()
	if router == nil {
		return callbackOutcome{toast: toastUnavailable}
	}

	// The message a button was on may be inaccessible (too old); its id and chat
	// are still there, its thread is not.
	threadID := 0
	if m := msg.Message(); m != nil {
		threadID = m.MessageThreadID
	}
	chat := msg.GetChat()
	messageID := fmt.Sprintf("%d", msg.GetMessageID())
	peerKind := "direct"
	if chat.Type != "private" {
		peerKind = "group"
	}
	in := channels.ControlInput{
		Channel:  c.Name(),
		ChatID:   chatIDWithThread(chat, threadID),
		Sender:   sender,
		SenderID: sender.CanonicalID,
		PeerKind: peerKind,
		// For a button, the message it was on is both: MessageID is where a reply
		// goes, ReplyToMessageID is what a snooze or a rating is about.
		MessageID:        messageID,
		ReplyToMessageID: messageID,
		Callback:         &channels.ControlCallback{Action: actionID, Payload: payload, CallbackID: q.ID},
	}

	reply, handled := router.TryHandle(ctx, in)
	if !handled {
		// The router does not know this action (or the payload is not one it
		// accepts): it is dropped, never given to the agent.
		return callbackOutcome{toast: toastInvalidButton}
	}
	// The control path fills AnswerCallback in for a command it ran; a refusal
	// before that (not the owner, rate limited, wrong chat) carries only Text. A
	// tap is never left without its words.
	toast := reply.AnswerCallback
	if toast == "" {
		toast = truncateRunes(firstLine(reply.Text), maxToastRunes)
	}
	return callbackOutcome{toast: toast, text: reply.Text, edit: reply.EditOriginal}
}

// senderAllowed is the allow-list check a message and a tap share. It is
// handleMessage's check (strict in a private chat when pairing is on), followed
// by the check BaseChannel.HandleMessage applies to what handleMessage lets
// through.
func (c *TelegramChannel) senderAllowed(chatType string, sender bus.SenderInfo) bool {
	allowed, _ := c.checkAllowList(chatType, sender)
	return allowed && c.IsAllowedSender(sender)
}

// checkAllowList is the first allow-list check on an inbound update. isPairingDM
// reports whether it was the strict check of a private chat with pairing on,
// where an empty allow_from means nobody is approved yet.
func (c *TelegramChannel) checkAllowList(chatType string, sender bus.SenderInfo) (allowed, isPairingDM bool) {
	isPairingDM = chatType == "private" && c.config != nil && c.config.Channels.Telegram.PairingEnabled
	if !isPairingDM {
		return c.IsAllowedSender(sender), false
	}
	// Strict allowlist: empty = deny all. Read from live config so we use
	// the latest allow_from without requiring a code restart for the check.
	for _, id := range c.config.Channels.Telegram.AllowFrom {
		if identity.MatchAllowed(sender, id) {
			return true, true
		}
	}
	return false, true
}

// deliverCallbackReply shows the control path's reply on the chat: it edits the
// card the button was on (which also removes the keyboard) when the reply asks
// for that, and otherwise sends it as a reply to the card, unless the toast
// already said all of it.
func (c *TelegramChannel) deliverCallbackReply(ctx context.Context, q *telego.CallbackQuery, out callbackOutcome) {
	msg := q.Message
	if msg == nil {
		return
	}
	chat := msg.GetChat()
	if out.edit {
		err := c.editCard(ctx, chat.ID, msg.GetMessageID(), out.text)
		if err == nil {
			return
		}
		// Fall through to a new message: the outcome must not be lost.
		logger.WarnCF("telegram", "Could not edit the card a button was on, sending the reply instead",
			map[string]any{"error": err.Error()})
	} else if strings.TrimSpace(out.text) == strings.TrimSpace(out.toast) {
		return
	}
	threadID := 0
	if m := msg.Message(); m != nil {
		threadID = m.MessageThreadID
	}
	if _, err := c.SendWithIDs(ctx, bus.OutboundMessage{
		Channel:          c.Name(),
		ChatID:           chatIDWithThread(chat, threadID),
		Content:          out.text,
		ReplyToMessageID: fmt.Sprintf("%d", msg.GetMessageID()),
	}); err != nil {
		logger.WarnCF("telegram", "Could not send a callback reply", map[string]any{"error": err.Error()})
	}
}

// editCard replaces the text of a message and, by leaving the reply markup out,
// removes its keyboard.
func (c *TelegramChannel) editCard(ctx context.Context, chatID int64, messageID int, text string) error {
	edit := tu.EditMessageText(tu.ID(chatID), messageID, markdownToTelegramHTML(text))
	edit.ParseMode = telego.ModeHTML
	if _, err := c.bot.EditMessageText(ctx, edit); err != nil {
		// HTML the API would not parse: say it in plain text.
		edit.Text = text
		edit.ParseMode = ""
		if _, err2 := c.bot.EditMessageText(ctx, edit); err2 != nil {
			return err2
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
