package channels

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/commands"
)

// TypingCapable — channels that can show a typing/thinking indicator.
// StartTyping begins the indicator and returns a stop function.
// The stop function MUST be idempotent and safe to call multiple times.
type TypingCapable interface {
	StartTyping(ctx context.Context, chatID string) (stop func(), err error)
}

// MessageEditor — channels that can edit an existing message.
// messageID is always string; channels convert platform-specific types internally.
type MessageEditor interface {
	EditMessage(ctx context.Context, chatID string, messageID string, content string) error
}

// ReactionCapable — channels that can add a reaction (e.g. 👀) to an inbound message.
// ReactToMessage adds a reaction and returns an undo function to remove it.
// The undo function MUST be idempotent and safe to call multiple times.
type ReactionCapable interface {
	ReactToMessage(ctx context.Context, chatID, messageID string) (undo func(), err error)
}

// PlaceholderCapable — channels that can send a placeholder message
// (e.g. "Thinking... 💭") that will later be edited to the actual response.
// The channel MUST also implement MessageEditor for the placeholder to be useful.
// SendPlaceholder returns the platform message ID of the placeholder so that
// Manager.preSend can later edit it via MessageEditor.EditMessage.
type PlaceholderCapable interface {
	SendPlaceholder(ctx context.Context, chatID string) (messageID string, err error)
}

// PlaceholderRecorder is injected into channels by Manager.
// Channels call these methods on inbound to register typing/placeholder state.
// Manager uses the registered state on outbound to stop typing and edit placeholders.
type PlaceholderRecorder interface {
	RecordPlaceholder(channel, chatID, placeholderID string)
	RecordTypingStop(channel, chatID string, stop func())
	RecordReactionUndo(channel, chatID string, undo func())
}

// CommandRegistrarCapable is implemented by channels that can register
// command menus with their upstream platform (e.g. Telegram BotCommand).
// Channels that do not support platform-level command menus can ignore it.
type CommandRegistrarCapable interface {
	RegisterCommands(ctx context.Context, defs []commands.Definition) error
}

// IDReportingSender — channels that can report the platform IDs of the messages
// they sent. SendWithIDs does everything Send does and additionally returns one
// ID per platform message created, in send order (one Send can create several
// when the adapter re-splits internally). When an existing message was edited
// instead of sending a new one, the edited message's ID is reported.
//
// A channel implementing this interface should make Send delegate to
// SendWithIDs so there is a single code path. The Manager calls exactly one of
// the two per attempt, never both. IDs may accompany a non-nil error when part
// of a message was sent before the failure.
type IDReportingSender interface {
	SendWithIDs(ctx context.Context, msg bus.OutboundMessage) ([]string, error)
}

// ActionRenderer — channels that can render bus.Action buttons natively.
// While SupportsActions is false (or the interface is absent) the Manager adds
// a text footer carrying each action's TextCommand instead.
type ActionRenderer interface {
	SupportsActions() bool
}
