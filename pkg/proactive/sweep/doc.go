// Package sweep is the heartbeat as a periodic safety net (proactive.heartbeat.mode
// "sweep"): instead of asking the model on every tick whether anything is worth
// saying, it asks the sources to re-read their state, looks at what changed in the
// world model, and calls the model only when something material did.
//
// One sweep (Runner.Run) does five things, in this order:
//
//  1. reconcile: every perception source re-emits its authoritative state, all of
//     them together under one deadline, so that anything a dropped connection
//     missed is caught; a source that fails or is slow is reported and the sweep
//     goes on with what it has;
//  2. settle: it waits for the pipeline to process what that queued, so that the
//     world model has moved and the triggers that fired are decided;
//  3. diff: what changed since the sweep's own mark, less what is below the noise
//     floor (Material). With nothing material there is no model call, and one INFO
//     line says so;
//  4. ask: the model is given the changes, the topics the triggers already decided
//     (as advice) and the owner's HEARTBEAT.md, and answers with respond_proactive
//     decisions. A decision that repeats a topic a trigger decided is dropped in
//     code (Filter), whatever the model did with the advice; the rest go to
//     attention, which is the only way anything reaches the owner;
//  5. mark: only after the decisions were handed over does the mark move. A failed
//     or interrupted turn leaves it, so the next sweep sees the same changes; after
//     several failures over the same range the sweep moves past it and says which.
//
// The package decides nothing about delivery and runs no tool: the model turn and
// the hand-over to attention are functions the gateway gives it.
//
// Dependency rule: this package imports pkg/logger, pkg/perception, pkg/proactive
// and pkg/worldmodel. It does not import the agent, the tools, the providers, the
// notify service, the channels, attention or the engine (the boundary test keeps it
// so): what it needs from them are the small interfaces in runner.go.
//
// Goroutines: none of its own. Run is called on the heartbeat service's goroutine
// and may block on the sources (bounded by the reconcile budget), on the pipeline
// (bounded by the settle budget) and on the model turn (bounded by the turn's
// deadline). It never touches the pipeline goroutine.
package sweep
