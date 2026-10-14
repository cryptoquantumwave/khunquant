// Package whoami builds the reply to /whoami: the IDs proactive.owner needs, and
// whether the caller already is the configured owner.
//
// It is a package of its own because two paths answer /whoami and must say the
// same thing: the control path (pkg/control), which answers it for every channel
// before any model sees the message, and the forwarding alias in pkg/commands
// that keeps the command in the help text and the Telegram menu. pkg/commands
// cannot import pkg/control (control reaches pkg/channels, which imports
// commands), so the text is built here, in a leaf both import.
package whoami

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// Query is who is asking, and from where.
type Query struct {
	Channel  string
	ChatID   string
	SenderID string
	// PeerKind is the routing peer kind the channel attached to the message:
	// "direct", "group", "channel", or "" when the channel does not say.
	PeerKind string
}

// Text is the reply. It never reveals the owner's IDs to anyone else: it is built
// from the caller's own fields only, and the configured owner enters it as a
// yes/no verdict, never as a value. cfg may be nil.
func Text(q Query, cfg *config.Config) string {
	sender := CallerCanonicalID(q.Channel, q.SenderID)
	var sb strings.Builder
	fmt.Fprintf(&sb, "channel: %s\nchat_id: %s\nsender_id: %s\n", q.Channel, q.ChatID, sender)

	group := IsGroupChat(q.Channel, q.ChatID, q.PeerKind)
	if group {
		sb.WriteString("\nThis is a group chat. The owner chat must be a direct chat: " +
			"send /whoami to me in a private message and use the IDs shown there.\n")
		if id, ok := telegramUserID(q.Channel, sender); ok {
			fmt.Fprintf(&sb, "On Telegram a direct chat's chat_id is your own user id, so use chat_id %s "+
				"with sender_id telegram:%s.\n", id, id)
		}
	}

	sb.WriteString("\n")
	sb.WriteString(ownerVerdict(q, sender, cfg, group))
	return sb.String()
}

// ownerVerdict answers "am I the owner?" without ever quoting the owner's IDs.
func ownerVerdict(q Query, sender string, cfg *config.Config, group bool) string {
	if cfg == nil {
		return "Owner status is unavailable here."
	}
	route, ok := cfg.Proactive.OwnerRoute()
	if !ok {
		return "No owner is configured."
	}
	if !identity.SameSender(sender, route.SenderID) {
		return "You are not the configured owner."
	}
	if strings.EqualFold(q.Channel, route.Channel) && strings.TrimSpace(q.ChatID) == route.ChatID {
		return "You are the configured owner (this is the owner chat)."
	}
	if group {
		return "You are the configured owner, but this group is not the owner chat."
	}
	return "You are the configured owner, but this is not the owner chat."
}

// CallerCanonicalID returns the caller's "platform:id". Channels set SenderID
// to the canonical form already; a raw id is qualified with the channel name,
// the same way the allow-list falls back.
func CallerCanonicalID(channel, senderID string) string {
	if platform, id, ok := identity.ParseCanonicalID(senderID); ok {
		return identity.BuildCanonicalID(platform, id)
	}
	return identity.BuildCanonicalID(channel, senderID)
}

// IsGroupChat reports whether the message came from a group or channel. Only
// Telegram's chat ids carry the answer themselves (negative); elsewhere the
// peer kind the channel attached is the signal.
func IsGroupChat(channel, chatID, peerKind string) bool {
	switch peerKind {
	case "group", "channel":
		return true
	}
	if strings.EqualFold(channel, "telegram") {
		first, _, _ := strings.Cut(chatID, "/") // "chat/thread" in forum topics
		if n, err := strconv.ParseInt(first, 10, 64); err == nil && n < 0 {
			return true
		}
	}
	return false
}

// telegramUserID returns the numeric Telegram user id from a canonical sender.
func telegramUserID(channel, sender string) (string, bool) {
	if !strings.EqualFold(channel, "telegram") {
		return "", false
	}
	platform, id, ok := identity.ParseCanonicalID(sender)
	if !ok || !strings.EqualFold(platform, "telegram") {
		return "", false
	}
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || n <= 0 {
		return "", false
	}
	return id, true
}
