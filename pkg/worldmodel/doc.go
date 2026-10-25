// Package worldmodel is the agent's memory of the present: the last price of each
// watched symbol, the portfolio and its day-start baseline, the open futures
// positions, the health of plans and sources, and a small set of detectors that
// say when something is unusual. It is held outside the model, so that a trigger
// asks it a question instead of fetching the data again, and so that a background
// turn can be told "what changed since you last looked" instead of being handed
// raw tool output.
//
//	events ──▶ Store.Apply ──▶ Diff ──▶ (evaluator)        pipeline goroutine
//	                │
//	                ├──▶ View()          read by triggers, the heartbeat, a turn
//	                └──▶ Checkpoint()    every 5 minutes and on stop, off the pipeline
//
// # Who may call what
//
// Apply has exactly one caller, the pipeline goroutine. View, SinceMark, SetMark
// and the Stats are read from other goroutines (the heartbeat sweep, the prompt
// contributor inside a user or background turn, the stats route). The Store
// guards its state with one reader-writer lock: Apply decodes and validates an
// event with no lock, then takes the write lock for the few microseconds the
// mutation needs; every View method takes the read lock, copies out what it
// returns and releases it. No method holds the lock across a database call, a
// callback or a clock read, and no View method returns a map or slice that the
// store still holds. A sequence of View calls is not one atomic snapshot - each
// call is - but on the pipeline goroutine, the only writer, nothing changes
// between them.
//
// # What is persisted, and when
//
// In-memory state is authoritative while the process runs. Apply never touches
// the database. A checkpoint (Store.Checkpoint, triggered from Housekeep and on
// Close) captures the changed rows under the lock, writes them in one transaction
// with no lock held, and clears the "changed" marks only after the commit, so a
// failed checkpoint loses nothing and is retried. State loaded at start keeps its
// original timestamps: a restored price is old until a new tick refreshes it, and
// View.Fresh is how a trigger tells.
//
// # Trust and the rules about missing numbers
//
// Each fact records the trust of the event that set it; a Change and a Signal
// carry the lowest trust among their inputs. A number whose payload flag says it
// is unknown is absent from the model, never stored as zero. Two portfolio
// snapshots whose unpriced-asset sets differ are not compared: the model keeps no
// change fact for the pair and re-bases its baselines on the newer one.
//
// # Dependencies
//
// This package imports pkg/events, pkg/proactive, pkg/perception (the Clock and
// SourceHealth types), pkg/perception/orderfeed (a leaf), pkg/sqlitex and
// pkg/logger. It does not import the sources package: payloads are decoded
// through small mirror types with the same JSON tags, and a test in the gateway
// pins the two to each other. It does not import pkg/agent, and the prompt
// contributor is handed to the agent through an interface the gateway wires.
package worldmodel
