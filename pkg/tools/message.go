package tools

import (
	"context"
	"fmt"
	"sync"
)

type SendCallback func(channel, chatID, content string) error

// sentTarget records the channel+chatID that the message tool sent to.
type sentTarget struct {
	Channel string
	ChatID  string
}

// maxTrackedTurns bounds how many turns' sent targets a MessageTool remembers.
//
// A turn's entry is normally removed by ClearTurn when the turn ends. Entries
// are abandoned when nobody is left to clear them: an async subagent that sends
// after its parent turn has already been cleared, or a cron turn that fails
// before its caller reaches PublishResponseIfNeeded. The cap, with oldest-first
// eviction, keeps those from accumulating for the life of the process. It is
// far above the number of turns that can be live at once, so a running turn's
// entry is not the one evicted.
const maxTrackedTurns = 1024

type MessageTool struct {
	sendCallback SendCallback
	mu           sync.Mutex
	// sentByTurn records, per turn ID, the targets this tool sent to. It is
	// keyed by turn because one MessageTool serves every turn of an agent, and
	// turns overlap: a heartbeat's message must not make a concurrent user turn
	// look as if it had already replied.
	sentByTurn map[string]map[sentTarget]struct{}
	// turnOrder lists the keys of sentByTurn oldest-first, for eviction.
	turnOrder []string
}

func NewMessageTool() *MessageTool {
	return &MessageTool{}
}

func (t *MessageTool) Name() string {
	return NameMessage
}

func (t *MessageTool) Description() string {
	return "Send a message to user on a chat channel. Use this when you want to communicate something."
}

func (t *MessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The message content to send",
			},
			"channel": map[string]any{
				"type":        "string",
				"description": "Optional: target channel (telegram, whatsapp, etc.)",
			},
			"chat_id": map[string]any{
				"type":        "string",
				"description": "Optional: target chat/user ID",
			},
		},
		"required": []string{"content"},
	}
}

// HasSentInTurn reports whether the message tool sent anything during the turn.
func (t *MessageTool) HasSentInTurn(turnID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.sentByTurn[turnID]) > 0
}

// HasSentToInTurn reports whether the message tool sent to the specific
// channel+chatID during the turn. It decides whether a turn's final response
// would duplicate a message the user already received, so it matches the exact
// conversation: a message sent elsewhere does not suppress the reply.
func (t *MessageTool) HasSentToInTurn(turnID, channel, chatID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.sentByTurn[turnID][sentTarget{Channel: channel, ChatID: chatID}]
	return ok
}

// ClearTurn forgets what was sent during the turn. Whoever owns the turn ID
// calls it once the turn's outcome has been decided.
func (t *MessageTool) ClearTurn(turnID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.sentByTurn[turnID]; !ok {
		return
	}
	delete(t.sentByTurn, turnID)
	for i, id := range t.turnOrder {
		if id == turnID {
			t.turnOrder = append(t.turnOrder[:i], t.turnOrder[i+1:]...)
			break
		}
	}
}

// recordSent notes that the turn sent to target. A send made outside any turn
// (turnID "") is not recorded: nobody could ask about it, and nothing would
// ever clear it.
func (t *MessageTool) recordSent(turnID string, target sentTarget) {
	if turnID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	targets, ok := t.sentByTurn[turnID]
	if !ok {
		if t.sentByTurn == nil {
			t.sentByTurn = make(map[string]map[sentTarget]struct{})
		}
		for len(t.turnOrder) >= maxTrackedTurns {
			delete(t.sentByTurn, t.turnOrder[0])
			t.turnOrder = t.turnOrder[1:]
		}
		targets = make(map[sentTarget]struct{})
		t.sentByTurn[turnID] = targets
		t.turnOrder = append(t.turnOrder, turnID)
	}
	targets[target] = struct{}{}
}

func (t *MessageTool) SetSendCallback(callback SendCallback) {
	t.sendCallback = callback
}

func (t *MessageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	content, ok := args["content"].(string)
	if !ok {
		return &ToolResult{ForLLM: "content is required", IsError: true}
	}

	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)

	if channel == "" {
		channel = ToolChannel(ctx)
	}
	if chatID == "" {
		chatID = ToolChatID(ctx)
	}

	if channel == "" || chatID == "" {
		return &ToolResult{ForLLM: "No target channel/chat specified", IsError: true}
	}

	if t.sendCallback == nil {
		return &ToolResult{ForLLM: "Message sending not configured", IsError: true}
	}

	if err := t.sendCallback(channel, chatID, content); err != nil {
		return &ToolResult{
			ForLLM:  fmt.Sprintf("sending message: %v", err),
			IsError: true,
			Err:     err,
		}
	}

	t.recordSent(TurnID(ctx), sentTarget{Channel: channel, ChatID: chatID})

	// Silent: user already received the message directly
	return &ToolResult{
		ForLLM: fmt.Sprintf("Message sent to %s:%s", channel, chatID),
		Silent: true,
	}
}
