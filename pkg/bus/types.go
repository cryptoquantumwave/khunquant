package bus

// Peer identifies the routing peer for a message (direct, group, channel, etc.)
type Peer struct {
	Kind string `json:"kind"` // "direct" | "group" | "channel" | ""
	ID   string `json:"id"`
}

// SenderInfo provides structured sender identity information.
type SenderInfo struct {
	Platform    string `json:"platform,omitempty"`     // "telegram", "discord", "slack", ...
	PlatformID  string `json:"platform_id,omitempty"`  // raw platform ID, e.g. "123456"
	CanonicalID string `json:"canonical_id,omitempty"` // "platform:id" format
	Username    string `json:"username,omitempty"`     // username (e.g. @alice)
	DisplayName string `json:"display_name,omitempty"` // display name
}

type InboundMessage struct {
	Channel    string            `json:"channel"`
	SenderID   string            `json:"sender_id"`
	Sender     SenderInfo        `json:"sender"`
	ChatID     string            `json:"chat_id"`
	Content    string            `json:"content"`
	Media      []string          `json:"media,omitempty"`
	Peer       Peer              `json:"peer"`                  // routing peer
	MessageID  string            `json:"message_id,omitempty"`  // platform message ID
	MediaScope string            `json:"media_scope,omitempty"` // media lifecycle scope
	SessionKey string            `json:"session_key"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	NoHistory  bool              `json:"no_history,omitempty"`
}

// ContextUsage describes how much of the model's context window the current
// session consumes, and how far it is from triggering compression.
type ContextUsage struct {
	UsedTokens       int `json:"used_tokens"`
	TotalTokens      int `json:"total_tokens"`       // model context window
	CompressAtTokens int `json:"compress_at_tokens"` // threshold that triggers compression
	UsedPercent      int `json:"used_percent"`       // 0-100
}

// VisibleToolCallFunction holds the function name and truncated JSON arguments
// shown in the web UI tool-call box.
type VisibleToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// VisibleToolCallExtraContent carries optional tool-feedback metadata shown
// alongside the call in the UI.
type VisibleToolCallExtraContent struct {
	ToolFeedbackExplanation string `json:"tool_feedback_explanation,omitempty"`
}

// VisibleToolCall is a frontend-safe representation of a tool call that is
// serialized into Pico Protocol tool_calls payloads.
type VisibleToolCall struct {
	ID           string                       `json:"id,omitempty"`
	Type         string                       `json:"type,omitempty"`
	Function     *VisibleToolCallFunction     `json:"function,omitempty"`
	ExtraContent *VisibleToolCallExtraContent `json:"extra_content,omitempty"`
}

type OutboundMessage struct {
	Channel          string        `json:"channel"`
	ChatID           string        `json:"chat_id"`
	Content          string        `json:"content"`
	ReplyToMessageID string        `json:"reply_to_message_id,omitempty"`
	ContextUsage     *ContextUsage `json:"context_usage,omitempty"`
	// Pico reasoning/tool-call fields — set only for pico channel (best-effort).
	Kind      string            `json:"kind,omitempty"`       // "" | "thought" | "tool_calls"
	ModelName string            `json:"model_name,omitempty"` // model that produced this message
	ToolCalls []VisibleToolCall `json:"tool_calls,omitempty"` // populated when Kind == "tool_calls"
}

// MediaPart describes a single media attachment to send.
type MediaPart struct {
	Type        string `json:"type"`                   // "image" | "audio" | "video" | "file"
	Ref         string `json:"ref"`                    // media store ref, e.g. "media://abc123"
	Caption     string `json:"caption,omitempty"`      // optional caption text
	Filename    string `json:"filename,omitempty"`     // original filename hint
	ContentType string `json:"content_type,omitempty"` // MIME type hint
}

// OutboundMediaMessage carries media attachments from Agent to channels via the bus.
type OutboundMediaMessage struct {
	Channel string      `json:"channel"`
	ChatID  string      `json:"chat_id"`
	Parts   []MediaPart `json:"parts"`
}
