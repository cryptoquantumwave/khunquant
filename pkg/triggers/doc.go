// Package triggers is where "conditions worth noticing" live: typed, testable
// triggers that read the world model and return candidates for attention, kept as
// data (a spec in the triggers table) with their state, and able to run in shadow
// mode next to the legacy path before they take over.
//
// A Registry holds the specs, builds a Trigger for each from the Factory of its
// kind, routes every diff of the world model to the triggers that list its event
// kind, caps what they return by mode, isolates their failures and keeps their
// state. It is the pipeline's evaluator and the market source's DemandSource.
//
// The rules, in short (the details are on the types):
//
//   - Effective mode = min(global mode, spec mode), off < shadow < live. Only a
//     live trigger under a live global mode makes a deliverable candidate; the
//     rest are marked Shadow, which attention logs and never delivers.
//   - Evaluate is pure. A trigger reads the View and the Diff and returns
//     candidates; it does no network, no database, reads no clock but View.Now().
//     The import boundary test keeps this package away from the agent, the tools,
//     the providers (so from every broker mutation), notify and the channels.
//   - State is saved after attention has decided, never before and never in its
//     transaction. A change a trigger makes to its State is held back until
//     Committed names all of that evaluation's candidates; a crash in between
//     leaves the old state, and the repeat is absorbed by attention's dedup.
//   - A trigger that errors or panics is isolated and counted; ten errors in a row
//     switch its spec off and send one templated notice.
//   - A candidate is checked before it leaves: the dedup key must read
//     <kind>:<subject>:<variant> with the spec's kind as the prefix, Protective
//     goes only with Critical, evidence is finite and capped. The registry sets
//     ID, TriggerID, IntentID and Shadow, and caps the trust at the event's.
//
// The portfolio kinds (portfolio.go) watch what the owner holds: move_vs_vol,
// drawdown, allocation_drift, liquidation_distance and data_stale. They read the
// portfolio snapshot, the futures positions and the health of the sources from the
// view, test the age and trust of every fact they act on, and fire nothing on input
// that is stale, restored, partial or unknown; a protective Critical is a template
// message built here from numbers. They notify and never act.
//
// Nothing here places an order or calls a tool, and nothing starts a goroutine.
package triggers
