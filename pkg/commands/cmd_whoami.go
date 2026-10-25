package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/identity"
)

// whoamiCommand tells the caller the IDs that proactive.owner needs, and
// whether the caller already is the configured owner.
//
// It must never reveal the owner's IDs to anyone else: the reply is built from
// the caller's own request fields only, and the configured owner enters it as a
// yes/no verdict, never as a value.
func whoamiCommand() Definition {
	return Definition{
		Name:        "whoami",
		Description: "Show your channel, chat and sender IDs (for proactive.owner)",
		Usage:       "/whoami",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			return req.Reply(formatWhoami(req, rt))
		},
	}
}

func formatWhoami(req Request, rt *Runtime) string {
	sender := callerCanonicalID(req)
	var sb strings.Builder
	fmt.Fprintf(&sb, "channel: %s\nchat_id: %s\nsender_id: %s\n", req.Channel, req.ChatID, sender)

	group := isGroupChat(req)
	if group {
		sb.WriteString("\nThis is a group chat. The owner chat must be a direct chat: " +
			"send /whoami to me in a private message and use the IDs shown there.\n")
		if id, ok := telegramUserID(req.Channel, sender); ok {
			fmt.Fprintf(&sb, "On Telegram a direct chat's chat_id is your own user id, so use chat_id %s "+
				"with sender_id telegram:%s.\n", id, id)
		}
	}

	sb.WriteString("\n")
	sb.WriteString(ownerVerdict(req, sender, rt, group))
	return sb.String()
}

// ownerVerdict answers "am I the owner?" without ever quoting the owner's IDs.
func ownerVerdict(req Request, sender string, rt *Runtime, group bool) string {
	if rt == nil || rt.Config == nil {
		return "Owner status is unavailable here."
	}
	route, ok := rt.Config.Proactive.OwnerRoute()
	if !ok {
		return "No owner is configured."
	}
	if !strings.EqualFold(sender, route.SenderID) {
		return "You are not the configured owner."
	}
	if strings.EqualFold(req.Channel, route.Channel) && strings.TrimSpace(req.ChatID) == route.ChatID {
		return "You are the configured owner (this is the owner chat)."
	}
	if group {
		return "You are the configured owner, but this group is not the owner chat."
	}
	return "You are the configured owner, but this is not the owner chat."
}

// callerCanonicalID returns the caller's "platform:id". Channels set SenderID
// to the canonical form already; a raw id is qualified with the channel name,
// the same way the allow-list falls back.
func callerCanonicalID(req Request) string {
	if platform, id, ok := identity.ParseCanonicalID(req.SenderID); ok {
		return identity.BuildCanonicalID(platform, id)
	}
	return identity.BuildCanonicalID(req.Channel, req.SenderID)
}

// isGroupChat reports whether the message came from a group or channel. Only
// Telegram's chat ids carry the answer themselves (negative); elsewhere the
// peer kind the channel attached is the signal.
func isGroupChat(req Request) bool {
	switch req.PeerKind {
	case "group", "channel":
		return true
	}
	if strings.EqualFold(req.Channel, "telegram") {
		first, _, _ := strings.Cut(req.ChatID, "/") // "chat/thread" in forum topics
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
