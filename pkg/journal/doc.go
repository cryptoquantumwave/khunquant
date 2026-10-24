// Package journal runs multi-step actions durably, on SQLite: two tables in the
// shared proactive database and no server. After a crash or a restart a workflow
// resumes where it stopped, a step that completed is never run again, and a step
// that was cut short is never forgotten. It carries every action that places
// orders (an approved order, a two-leg delta-neutral open, an unwind, a scheduled
// purchase), so a bug here is a duplicated order.
//
// # Model
//
// A Definition names a kind of workflow and gives its function, which receives a
// *Ctx. The function calls Ctx.Step for every side effect. Step writes a journal
// row with the step's name and attempt number, committed, BEFORE the step function
// runs, and the step's output (or error) after. When a workflow resumes, its
// function runs again from the top: Step returns the stored output of every
// completed step without calling its function, and carries on for real at the first
// step that has no output.
//
// # Rules for workflow authors
//
//  1. All side effects go inside Step.
//  2. No time.Now() or randomness outside steps. Use Ctx.Now, which is recorded on
//     the first run and replayed. Between steps the function must be deterministic:
//     the same steps with the same names in the same order, whatever the clock,
//     the network or the environment say. A replay that reaches a different step
//     than the journaled one fails the workflow with ErrNonDeterministic and runs
//     nothing further.
//  3. A step that places an order must send Ctx.IdempotencyKey with it, and must
//     reconcile when attempt > 1 (below).
//  4. A step's output must be JSON-serialisable and at most 64 KB. A workflow's
//     input is also at most 64 KB. Both are stored in clear text in the database
//     and show in backups: never put a credential in either. Names of steps and
//     kinds use letters, digits, '_', '.', '-' (64 characters at most); a workflow
//     has at most 200 journal records (steps, Now and SleepUntil calls).
//
// Also: the function must not start goroutines that outlive it or call Ctx from
// more than one goroutine; and it must return the error Step, SleepUntil or Now
// reports (ErrSuspended, ErrCancelled, ErrLeaseLost, ErrInterrupted) at once. The
// engine does not rely on that: after such an error every further Step returns it
// without running anything, and the outcome is decided by what the Ctx halted on
// rather than by what the function returns.
//
// # What a step function must do on attempt > 1
//
// The step function gets its attempt number. Attempt 1 is a first run. A higher
// number means an earlier attempt was journaled as started and never finished: the
// process died, or the lease was lost, somewhere between "started" and "finished",
// and the side effect may or may not have happened. The function must look before
// it acts: ask the other side what exists for Ctx.IdempotencyKey (or, for a system
// without client ids, which objects were created since Ctx.StepStartedAt, the time
// the FIRST attempt began) and return what it finds instead of repeating the
// effect. Only a failed attempt, which returned an error, is retried by Retries;
// a re-run after a crash is not counted against Retries, but a step is never
// started more than MaxStepAttempts times.
//
// # Crash semantics
//
//   - Before the "started" row is committed: nothing was journaled; the resume runs
//     the step as attempt 1.
//   - After it, while the function runs, after it returned and before the output is
//     committed (the three cannot be told apart): the resume runs the step again as
//     attempt+1.
//   - After the output is committed: the step is replayed from its output, whatever
//     happens next.
//
// The "started" row, the output row and every other record of a step are committed
// at synchronous=FULL on their own connection, so they also survive a power cut,
// not only a process crash; the shared database otherwise runs at NORMAL.
//
// # Leases
//
// The gateway and a `khunquant agent` process may open the same database. A runner
// claims a workflow in one transaction (its runner id, a lease end 30 s away and a
// new epoch), renews the lease every third of its length from a goroutine that
// does not depend on the step returning, and writes every journal record in a
// transaction whose first statement proves it still owns (runner, epoch) and that
// the lease has not run out. Another runner takes a running workflow over only
// after the lease end PLUS a grace period (10 s by default) has passed; a runner
// that releases a workflow on purpose (it suspends, finishes, or stops cleanly)
// clears the owner and the next claimer does not wait.
//
// A runner that cannot renew its lease, or finds another owner, or finds its own
// lease already over, cancels the context of the step function that is running,
// at once, and writes nothing more for that workflow except a step output that
// still passes the fence. The grace period exists so that a runner that is merely
// slow has noticed and cancelled its step before a new owner starts it: the
// default grace equals one renewal interval (lease/3), and the runner also checks
// its own clock against the last lease it confirmed before every renewal, so a
// stalled runner does not outlive its lease by more than the time it is stalled.
//
// What remains possible: a step function that ignores its context can still act
// after a takeover. Order steps must therefore pass the context to the exchange
// call and reconcile on attempt > 1, which is what makes a takeover safe. A
// process suspended for longer than the lease plus the grace can send a request it
// had already begun. The engine narrows these windows; it cannot close them.
//
// # Suspension, cancel, retries
//
// SleepUntil and a retry Backoff persist their wake time and suspend the workflow
// as waiting: the lease is released, no worker is held, and Run resumes it when it
// is due. A restart in the middle of either does not reset it and does not shorten
// it. Cancel is durable and takes effect at the next step boundary: the step that
// is running is not interrupted. Retries are opt-in and bounded: by default a step
// that returns an error fails the workflow.
//
// # Events and counters
//
// The engine publishes Events (ids, kind, step name, sequence, attempt, status,
// durations) through a Publisher; never a workflow's input, a step's output or an
// error message. Stats exposes counters.
//
// # Dependencies
//
// This package imports the standard library, pkg/sqlitex and pkg/logger only. The
// clock and the publisher are interfaces defined here; a boundary test enforces
// the import list.
package journal
