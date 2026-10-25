package tools

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/constants"
)

// Tool is the interface that all tools must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]any
	Execute(ctx context.Context, args map[string]any) *ToolResult
}

// --- Request-scoped tool context (channel / chatID) ---
//
// Carried via context.Value so that concurrent tool calls each receive
// their own immutable copy — no mutable state on singleton tool instances.
//
// Keys are unexported pointer-typed vars — guaranteed collision-free,
// and only accessible through the helper functions below.

type toolCtxKey struct{ name string }

var (
	ctxKeyChannel = &toolCtxKey{"channel"}
	ctxKeyChatID  = &toolCtxKey{"chatID"}
	ctxKeyOrigin  = &toolCtxKey{"origin"}
	ctxKeySender  = &toolCtxKey{"sender"}
)

// Tool call origins. A turn is tagged where it starts, and the tag travels with
// the context into ExecuteWithContext, so a policy can distinguish a person
// typing a command from a scheduled job running unattended.
const (
	// OriginInternal is a local console or CLI turn.
	OriginInternal = "internal"
	// OriginRemote is a turn driven by a message from a chat channel.
	OriginRemote = "remote"
	// OriginCron is a scheduled job: no human is present to answer a prompt.
	OriginCron = "cron"
	// OriginHeartbeat is a periodic wake-up the assistant starts by itself: no
	// human is present either, and the prompt is a file the assistant can edit.
	OriginHeartbeat = "heartbeat"
)

// IsAutonomousOrigin reports whether a turn with this origin was started by the
// assistant's own machinery rather than by a person. Autonomous turns run under
// the class ceiling in classification.go.
func IsAutonomousOrigin(origin string) bool {
	return origin == OriginCron || origin == OriginHeartbeat
}

// WithToolOrigin returns a child context tagged with the origin of the turn.
//
// It is deliberately set-once: the first tag wins and later calls are no-ops.
// Cron tags at its own call site, which is outside the agent loop, and the loop
// then tags every turn by channel. Were this unconditional, the loop's tag
// would overwrite "cron" with "internal" and the scheduled-job gate would never
// fire. Enforcing that here rather than asking each call site to check first
// means a new call site cannot reintroduce the bug. It also gives sub-turns
// spawned from a cron turn the correct answer: they stay cron.
func WithToolOrigin(ctx context.Context, origin string) context.Context {
	if origin == "" || ToolOrigin(ctx) != "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyOrigin, origin)
}

// ToolOrigin extracts the turn's origin from ctx, or "" if untagged.
func ToolOrigin(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyOrigin).(string)
	return v
}

// OriginForChannel maps the channel a turn arrived on to its origin.
//
// constants.IsInternalChannel covers cli, system and subagent. A subagent turn
// is the agent's own work rather than a stranger's message, so it counts as
// internal - and a subagent spawned from a cron turn keeps "cron" regardless,
// because WithToolOrigin is set-once. An empty channel means no channel was
// attached at all (a direct local invocation), which is likewise internal.
func OriginForChannel(channel string) string {
	if channel == "" || constants.IsInternalChannel(channel) {
		return OriginInternal
	}
	return OriginRemote
}

// WithToolSender tags the turn with the sender who started it, so an operator
// approval can be bound to the person who made the request. Set-once for the
// same reason as WithToolOrigin: a sub-turn must not relabel its requester.
func WithToolSender(ctx context.Context, senderID string) context.Context {
	if senderID == "" || ToolSender(ctx) != "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeySender, senderID)
}

// ToolSender extracts the turn's sender from ctx, or "" if untagged.
func ToolSender(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeySender).(string)
	return v
}

// WithToolContext returns a child context carrying channel and chatID.
func WithToolContext(ctx context.Context, channel, chatID string) context.Context {
	ctx = context.WithValue(ctx, ctxKeyChannel, channel)
	ctx = context.WithValue(ctx, ctxKeyChatID, chatID)
	return ctx
}

// ToolChannel extracts the channel from ctx, or "" if unset.
func ToolChannel(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyChannel).(string)
	return v
}

// ToolChatID extracts the chatID from ctx, or "" if unset.
func ToolChatID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyChatID).(string)
	return v
}

// AsyncCallback is a function type that async tools use to notify completion.
// When an async tool finishes its work, it calls this callback with the result.
//
// The ctx parameter allows the callback to be canceled if the agent is shutting down.
// The result parameter contains the tool's execution result.
type AsyncCallback func(ctx context.Context, result *ToolResult)

// AsyncExecutor is an optional interface that tools can implement to support
// asynchronous execution with completion callbacks.
//
// Unlike the old AsyncTool pattern (SetCallback + Execute), AsyncExecutor
// receives the callback as a parameter of ExecuteAsync. This eliminates the
// data race where concurrent calls could overwrite each other's callbacks
// on a shared tool instance.
//
// This is useful for:
//   - Long-running operations that shouldn't block the agent loop
//   - Subagent spawns that complete independently
//   - Background tasks that need to report results later
//
// Example:
//
//	func (t *SpawnTool) ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult {
//	    go func() {
//	        result := t.runSubagent(ctx, args)
//	        if cb != nil { cb(ctx, result) }
//	    }()
//	    return AsyncResult("Subagent spawned, will report back")
//	}
type AsyncExecutor interface {
	Tool
	// ExecuteAsync runs the tool asynchronously. The callback cb will be
	// invoked (possibly from another goroutine) when the async operation
	// completes. cb is guaranteed to be non-nil by the caller (registry).
	ExecuteAsync(ctx context.Context, args map[string]any, cb AsyncCallback) *ToolResult
}

func ToolToSchema(tool Tool) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"parameters":  tool.Parameters(),
		},
	}
}
