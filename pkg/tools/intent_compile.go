package tools

import (
	"context"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/intents"
)

// IntentCompiler is what the intent tools know of the compiler that turns an active
// watch or reminder into a trigger (pkg/triggers/intents). The gateway supplies it,
// late-bound and per generation (the tools are built before the trigger registry
// exists, and a config reload builds both again); a CLI agent has none.
type IntentCompiler interface {
	// Request asks for a pass; it never blocks.
	Request()
	// Note is the code-built status note for an intent that could not be carried out,
	// or "".
	Note(id string) string
	// Mode is the trigger registry's global mode: "live", "shadow" or "off".
	Mode() string
	// Running reports whether the compiler is running.
	Running() bool
}

// IntentCompilerFunc resolves the compiler of the running generation, or nil when
// there is none (triggers off, perception off, the registry could not be built).
type IntentCompilerFunc func() IntentCompiler

func resolveCompiler(f IntentCompilerFunc) IntentCompiler {
	if f == nil {
		return nil
	}
	c := f()
	if c == nil {
		return nil
	}
	return c
}

// requestCompile asks for a pass when there is a compiler.
func requestCompile(f IntentCompilerFunc) {
	if c := resolveCompiler(f); c != nil {
		c.Request()
	}
}

// Words the answers use. They are the whole of what the owner is told about whether
// an active intent works, and are built by code.
const (
	intentNobodyWatching = "Recorded, but nothing watches it: intent triggers are not running (triggers are off, or perception is), " +
		"so it does nothing until proactive triggers are switched on."
	intentNobodyReminding = "Recorded, but nothing will remind you: intent triggers are not running (triggers are off, or perception is), " +
		"so it does nothing until proactive triggers are switched on."
	intentRehearsal = "Recorded. Triggers are in shadow mode: it is rehearsed and logged but nothing is delivered to the owner until triggers are live."
	intentWatching  = "It is set up and watching; the owner is told when it happens."
	intentReminding = "It is set up; the owner is reminded at the time."
	intentOrderLive = "It is set up as a notice: when its trigger fires the owner is told which standing order it is. " +
		"Nothing is proposed or done automatically yet."
	intentGoalNote = "It is kept in front of the assistant in every conversation; nothing checks it against the market automatically yet."
)

// liveNote says whether an active watch, reminder or standing order is working.
func liveNote(it intent.Intent, comp IntentCompiler) string {
	reminder := it.Type == intent.TypeReminder
	if comp == nil || !comp.Running() || comp.Mode() == triggers.ModeOff {
		if reminder {
			return intentNobodyReminding
		}
		return intentNobodyWatching
	}
	if comp.Mode() == triggers.ModeShadow {
		return intentRehearsal
	}
	switch it.Type {
	case intent.TypeReminder:
		return intentReminding
	case intent.TypeStandingOrder:
		return intentOrderLive
	}
	return intentWatching
}

// refusal turns a Validate error into the sentence the owner hears.
func intentRefusal(err error) string {
	return strings.TrimPrefix(err.Error(), "intents: ")
}

// validateIntent runs the compiler's own check on an intent that is about to be
// active. A type that compiles to nothing passes.
func validateIntent(ctx context.Context, store *intent.Store, it intent.Intent) error {
	return intents.Validate(ctx, store, it, time.Now())
}
