package telegram

import (
	"context"
	"fmt"

	"github.com/mymmrac/telego"

	"github.com/cryptoquantumwave/khunquant/pkg/channels"
)

// Reactions.
//
// With proactive.learning on, the bot asks Telegram for message_reaction updates, and a
// thumbs up or down on one of its messages is the owner's verdict on it. Like a tap it
// is not a message: it goes to the control path's feedback side (pkg/control, which
// applies the owner rule and maps the message to its intervention) and to nothing else,
// so no model sees it.
//
// Whether Telegram delivers these updates to a bot in a private chat is UNVERIFIED (the
// Bot API documents them for chats the bot administers); the handler is tested against
// the API stub only.

const (
	emojiThumbsUp   = "\U0001F44D"
	emojiThumbsDown = "\U0001F44E"
)

// learningOn reports whether the bot should receive and handle reactions: proactive
// learning is effectively on (it needs proactive on and a valid owner).
func (c *TelegramChannel) learningOn() bool {
	return c.config != nil && c.config.Proactive.Effective().Learning
}

// addedVerdict returns the thumbs the user ADDED with this update: the one that is in
// the new reaction list and was not in the old one. A removed reaction, any other
// emoji, a custom emoji and both thumbs at once give "".
func addedVerdict(r *telego.MessageReactionUpdated) string {
	had := map[string]bool{}
	for _, o := range r.OldReaction {
		if e, ok := o.(*telego.ReactionTypeEmoji); ok {
			had[e.Emoji] = true
		}
	}
	added := map[string]bool{}
	for _, n := range r.NewReaction {
		if e, ok := n.(*telego.ReactionTypeEmoji); ok && !had[e.Emoji] {
			added[e.Emoji] = true
		}
	}
	switch {
	case added[emojiThumbsUp] && !added[emojiThumbsDown]:
		return emojiThumbsUp
	case added[emojiThumbsDown] && !added[emojiThumbsUp]:
		return emojiThumbsDown
	}
	return ""
}

// handleReaction offers a thumbs reaction to the control path. An anonymous reaction
// (no user) and a user the allow-list refuses are dropped, as for a message.
func (c *TelegramChannel) handleReaction(ctx context.Context, r *telego.MessageReactionUpdated) error {
	if r == nil || r.User == nil {
		return nil
	}
	emoji := addedVerdict(r)
	if emoji == "" {
		return nil
	}
	sender := telegramSender(r.User)
	if !c.senderAllowed(r.Chat.Type, sender) {
		return nil
	}
	peerKind := "direct"
	if r.Chat.Type != "private" {
		peerKind = "group"
	}
	id := fmt.Sprintf("%d", r.MessageID)
	c.ObserveReaction(ctx, channels.ControlInput{
		Channel: c.Name(), ChatID: chatIDWithThread(r.Chat, 0), MessageID: id, ReplyToMessageID: id,
		Sender: sender, SenderID: sender.CanonicalID, PeerKind: peerKind,
	}, emoji)
	return nil
}
