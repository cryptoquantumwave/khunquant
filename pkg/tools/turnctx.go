package tools

import (
	"context"

	"github.com/google/uuid"
)

// --- Turn identity ---
//
// A turn is one run of the agent loop for one inbound message, cron job or
// heartbeat. Several turns can run at once (a heartbeat alongside a user
// message), and they share singleton tool instances, so any per-turn state a
// tool keeps has to be keyed by the turn that produced it. The turn ID travels
// in the context for that purpose.
//
// A sub-turn carries its parent's turn ID: what a subagent does on the parent's
// behalf counts as done in the parent's turn.

var ctxKeyTurnID = &toolCtxKey{"turnID"}

// NewTurnID returns a new unique turn ID.
func NewTurnID() string {
	return uuid.NewString()
}

// WithTurnID returns a child context carrying the turn ID. An empty id leaves
// ctx unchanged.
//
// The agent loop adopts an ID it finds in the context instead of minting its
// own, and then leaves the per-turn state under that ID alone when the turn
// returns. A caller that needs to ask about the turn afterwards (did the
// message tool already reply to this chat?) therefore sets the ID itself,
// before starting the turn, and is responsible for clearing that state.
func WithTurnID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyTurnID, id)
}

// TurnID extracts the turn ID from ctx, or "" if the context belongs to no turn.
func TurnID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyTurnID).(string)
	return v
}
