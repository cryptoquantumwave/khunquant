package tools

import "context"

var ctxKeySubTurn = &toolCtxKey{"subTurn"}

// WithSubTurn tags ctx as belonging to a turn that another turn started (a spawned
// subagent, a delegated task): its task text was written by a model, which may
// have read a web page, and not by the person whose origin and sender it carries.
//
// The tag only ever restricts. A sub-turn keeps the origin and sender of the turn
// that started it (so a cron job's child is still cron), which makes it look like
// that turn; this is what tells the two apart. Both ways a sub-turn is started
// set it: the agent loop's SubTurnSpawner and the SubagentManager's tool loop.
func WithSubTurn(ctx context.Context) context.Context {
	if IsSubTurn(ctx) {
		return ctx
	}
	return context.WithValue(ctx, ctxKeySubTurn, true)
}

// IsSubTurn reports whether ctx belongs to a sub-turn.
func IsSubTurn(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeySubTurn).(bool)
	return v
}
