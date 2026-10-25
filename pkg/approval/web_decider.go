package approval

import "context"

type webDeciderKey struct{}

// WithWebDecider marks ctx as carrying a decision from the authenticated web
// launcher chat. Only the control path's one call of Decide sets it (pinned by
// TestOrderExecutionCallGraph), after checking the message with
// config.IsWebOwnerTurn; Decide checks the route again and refuses the marker
// unless it is active. The marker carries no identity, so a model argument or a
// sender string can neither set it nor smuggle "web" in as a sender.
func WithWebDecider(ctx context.Context) context.Context {
	return context.WithValue(ctx, webDeciderKey{}, true)
}

// IsWebDecider reports whether ctx carries WithWebDecider.
func IsWebDecider(ctx context.Context) bool {
	v, _ := ctx.Value(webDeciderKey{}).(bool)
	return v
}
