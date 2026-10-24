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
// A goal's expectation is compiled the same way: an expression that does not compile,
// calls an unknown function, does not give true or false or is over the bounds is
// refused, and so is an expectation on anything but a goal. An intent that compiles to
// nothing (a goal without an expectation, a preference) is always valid.
func Validate(ctx context.Context, store *intent.Store, it intent.Intent, now time.Time) error {
	if it.Expected != nil && it.Type != intent.TypeGoal {
		return fmt.Errorf("%w: an expectation can only be set on a goal, not on a %s", ErrCannotRun, it.Type)
	}
	if !Compiles(it) {
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
