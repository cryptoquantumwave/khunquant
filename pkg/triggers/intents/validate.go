package intents

import (
	"context"
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"time"
)

// Validate checks, before an intent becomes active, that it can be carried out: a
// watch or standing order has a trigger of a kind that works and parameters its own
// kind accepts, a reminder says when exactly once and not in the past and no tighter
// than once a minute. It is the same compile the compiler runs, so a bad threshold is
// heard about in the turn the owner asked, not never. It needs no registry: it works
// with triggers off, where the intent is recorded and nothing runs it.
//
// An intent type that compiles to nothing (a goal, a preference) is always valid.
func Validate(ctx context.Context, store *intent.Store, it intent.Intent, now time.Time) error {
	if !Compilable(it.Type) {
		return nil
	}
	plan, err := Compile(it, Env{Now: now, Loc: ProfileLocation(ctx, store), ActivatedAt: now})
	if err != nil {
		return fmt.Errorf("%w: %s", ErrCannotRun, err.Error())
	}
	if !plan.Due.IsZero() && !plan.Due.After(now) {
		return fmt.Errorf("%w: that time has already passed", ErrCannotRun)
	}
	return nil
}

// ErrCannotRun marks a refusal by Validate; the rest of the text says why.
var ErrCannotRun = errors.New("it cannot be carried out")
