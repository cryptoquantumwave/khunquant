package tools

import "context"

// ctxKeySessionKey carries the calling turn's session key. It is kept apart
// from the channel/chatID pair in base.go because only session-scoped tools
// (e.g. seahorse short_grep / short_expand) need it, and those must be able
// to tell "no session" apart from "empty session".
var ctxKeySessionKey = &toolCtxKey{"sessionKey"}

// WithToolSessionKey returns a child context carrying the caller's session key.
func WithToolSessionKey(ctx context.Context, sessionKey string) context.Context {
	return context.WithValue(ctx, ctxKeySessionKey, sessionKey)
}

// ToolSessionKey extracts the caller's session key from ctx, or "" if unset.
func ToolSessionKey(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeySessionKey).(string)
	return v
}
