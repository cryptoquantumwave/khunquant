package tools

import (
	"context"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

var ctxKeyTurnKind = &toolCtxKey{"turnKind"}

// WithProactiveTurn tags ctx as a proactive turn of the given kind.
//
// Unlike WithToolOrigin it is not set-once, on purpose: it is the one place a
// turn chooses its own ceiling, and the ceiling it chooses is the strictest
// there is. A set-once tag would let a context that already carried a person's
// origin ("remote", "internal") start a proactive turn that keeps that origin,
// and with it a ceiling that allows everything. Overriding can only tighten.
func WithProactiveTurn(ctx context.Context, kind proactive.TurnKind) context.Context {
	ctx = context.WithValue(ctx, ctxKeyOrigin, OriginProactive)
	return context.WithValue(ctx, ctxKeyTurnKind, kind)
}

// ProactiveTurnKind returns the kind a proactive turn was started with, or ""
// when ctx carries none. It is meaningful only under OriginProactive.
func ProactiveTurnKind(ctx context.Context) proactive.TurnKind {
	v, _ := ctx.Value(ctxKeyTurnKind).(proactive.TurnKind)
	return v
}
