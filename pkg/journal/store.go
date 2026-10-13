package journal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// Component is the sqlitex migration component of this package.
const Component = "journal"

var migrations = []sqlitex.Migration{
	{
		Component: Component,
		Version:   1,
		SQL: `CREATE TABLE workflows (
	id               TEXT    PRIMARY KEY,
	kind             TEXT    NOT NULL,
	input            TEXT    NOT NULL,
	status           TEXT    NOT NULL CHECK (status IN ('pending','running','waiting','succeeded','failed','cancelled')),
	protective       INTEGER NOT NULL DEFAULT 0,
	wake_at_ms       INTEGER,
	error            TEXT    NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0,
	cancel_reason    TEXT    NOT NULL DEFAULT '',
	runner_id        TEXT    NOT NULL DEFAULT '',
	epoch            INTEGER NOT NULL DEFAULT 0,
	lease_until_ms   INTEGER NOT NULL DEFAULT 0,
	created_at_ms    INTEGER NOT NULL,
	updated_at_ms    INTEGER NOT NULL,
	finished_at_ms   INTEGER
);
CREATE INDEX workflows_due ON workflows(status, wake_at_ms, lease_until_ms);
CREATE INDEX workflows_finished ON workflows(finished_at_ms) WHERE finished_at_ms IS NOT NULL;
CREATE TABLE workflow_steps (
	workflow_id         TEXT    NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
	seq                 INTEGER NOT NULL,
	name                TEXT    NOT NULL,
	attempts            INTEGER NOT NULL,
	failures            INTEGER NOT NULL DEFAULT 0,
	started_at_ms       INTEGER NOT NULL,
	first_started_at_ms INTEGER NOT NULL,
	finished_at_ms      INTEGER,
	wake_at_ms          INTEGER,
	output              TEXT,
	error               TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (workflow_id, seq)
)`,
	},
}

// store is every SQL statement of the package. A runner's writes are all fenced:
// they succeed only while (runner, epoch) still holds the workflow's lease.
type store struct {
	db *sql.DB
	// fault, when set (tests only), is asked before every operation and may fail it.
	fault func(op string) error
	// timeout bounds each transaction.
	timeout time.Duration
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func msPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

// fence names the owner of a claimed workflow. The epoch grows on every claim, so
// a runner that lost the workflow and a later claim by the same runner id are
// still told apart.
type fence struct {
	id, runner string
	epoch      int64
}

// tx runs fn in one transaction on a connection of its own. durable makes the
// commit survive a power cut as well as a process crash: the pool's connections
// use synchronous=NORMAL, which in WAL mode can lose the last commits of a power
// failure, and a lost "started" row is a lost reconcile. The connection runs at
// synchronous=FULL for this transaction only and is put back to NORMAL (or
// discarded, if that fails) before it returns to the pool.
func (s *store) tx(ctx context.Context, op string, durable bool, fn func(tx *sql.Tx) error) (err error) {
	if s.fault != nil {
		if ferr := s.fault(op); ferr != nil {
			return ferr
		}
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("journal: %s: %w", op, err)
	}
	defer conn.Close()
	if durable {
		if _, err := conn.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
			return fmt.Errorf("journal: %s: %w", op, err)
		}
		defer func() {
			if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA synchronous=NORMAL"); rerr != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn }) // never pool a FULL connection
				err = errors.Join(err, fmt.Errorf("journal: %s: restore synchronous: %w", op, rerr))
			}
		}()
	}
	t, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("journal: %s: %w", op, err)
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("journal: %s: commit: %w", op, err)
	}
	return nil
}

// fenced is tx whose first statement proves the caller still owns the workflow:
// it touches the row only while the runner and epoch match, the workflow is
// running and its lease has not run out. Nothing else in the transaction runs
// otherwise.
func (s *store) fenced(ctx context.Context, op string, f fence, now time.Time, fn func(tx *sql.Tx) error) error {
	return s.tx(ctx, op, true, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE workflows SET updated_at_ms = ?
			 WHERE id = ? AND runner_id = ? AND epoch = ? AND status = 'running' AND lease_until_ms > ?`,
			ms(now), f.id, f.runner, f.epoch, ms(now))
		if err != nil {
			return fmt.Errorf("journal: %s: %w", op, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrLeaseLost
		}
		return fn(tx)
	})
}

func (s *store) insertWorkflow(ctx context.Context, w Workflow, now time.Time) error {
	return s.tx(ctx, "insert_workflow", true, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO workflows (id, kind, input, status, protective, created_at_ms, updated_at_ms)
			 VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
			w.ID, w.Kind, string(w.Input), boolInt(w.Protective), ms(now), ms(now))
		if err != nil {
			return fmt.Errorf("journal: insert workflow: %w", err)
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

const workflowCols = `id, kind, input, status, protective, wake_at_ms, error, cancel_requested,
	created_at_ms, updated_at_ms, finished_at_ms`

type rowScanner interface{ Scan(dest ...any) error }

func scanWorkflow(r rowScanner) (Workflow, error) {
	var (
		w                  Workflow
		input, status      string
		prot, cancelReq    int
		wake, fin          sql.NullInt64
		created, updatedMS int64
	)
	if err := r.Scan(&w.ID, &w.Kind, &input, &status, &prot, &wake, &w.Error, &cancelReq,
		&created, &updatedMS, &fin); err != nil {
		return Workflow{}, err
	}
	w.Input = []byte(input)
	w.Status = Status(status)
	w.Protective = prot != 0
	w.CancelRequested = cancelReq != 0
	w.WakeAt = msPtr(wake)
	w.CreatedAt, w.UpdatedAt = fromMS(created), fromMS(updatedMS)
	w.FinishedAt = msPtr(fin)
	return w, nil
}

func (s *store) getWorkflow(ctx context.Context, id string) (Workflow, error) {
	if s.fault != nil {
		if err := s.fault("get_workflow"); err != nil {
			return Workflow{}, err
		}
	}
	w, err := scanWorkflow(s.db.QueryRowContext(ctx, `SELECT `+workflowCols+` FROM workflows WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Workflow{}, ErrNotFound
	}
	if err != nil {
		return Workflow{}, fmt.Errorf("journal: read workflow: %w", err)
	}
	return w, nil
}

// stepRow is a journaled step as the replay needs it.
type stepRow struct {
	seq                int
	name               string
	attempts, failures int
	startedAt, firstAt time.Time
	finished           bool
	finishedAt         time.Time
	wakeAt             *time.Time
	output             []byte
	hasOutput          bool
	errText            string
}

func (s *store) loadSteps(ctx context.Context, id string) ([]stepRow, error) {
	if s.fault != nil {
		if err := s.fault("load_steps"); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, name, attempts, failures, started_at_ms, first_started_at_ms, finished_at_ms, wake_at_ms, output, error
		 FROM workflow_steps WHERE workflow_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("journal: read steps: %w", err)
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var (
			r              stepRow
			started, first int64
			fin, wak       sql.NullInt64
			output         sql.NullString
		)
		if err := rows.Scan(&r.seq, &r.name, &r.attempts, &r.failures, &started, &first, &fin, &wak, &output, &r.errText); err != nil {
			return nil, fmt.Errorf("journal: read steps: %w", err)
		}
		r.startedAt, r.firstAt = fromMS(started), fromMS(first)
		if fin.Valid {
			r.finished, r.finishedAt = true, fromMS(fin.Int64)
		}
		r.wakeAt = msPtr(wak)
		if output.Valid {
			r.hasOutput, r.output = true, []byte(output.String)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: read steps: %w", err)
	}
	return out, nil
}

func (r stepRow) record(workflowID string) StepRecord {
	rec := StepRecord{
		WorkflowID: workflowID, Seq: r.seq, Name: r.name, Error: r.errText,
		Attempts: r.attempts, Failures: r.failures, StartedAt: r.startedAt, FirstStartedAt: r.firstAt,
		FinishedAt: r.finishedAt, WakeAt: r.wakeAt,
	}
	if r.hasOutput {
		rec.Output = append([]byte(nil), r.output...)
	}
	return rec
}

func inClause(n int) string {
	if n == 0 {
		return "(NULL)"
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

func kindArgs(kinds []string) []any {
	out := make([]any, len(kinds))
	for i, k := range kinds {
		out[i] = k
	}
	return out
}

// due lists workflows a runner could claim now: unheld ones, waiting ones whose
// wake time has come, and running ones whose lease ran out more than grace ago.
// Only kinds this engine has a definition for are listed; another process may
// know kinds this one does not.
func (s *store) due(ctx context.Context, kinds []string, now time.Time, grace time.Duration, limit int) ([]string, error) {
	if s.fault != nil {
		if err := s.fault("due"); err != nil {
			return nil, err
		}
	}
	args := append(kindArgs(kinds), ms(now), ms(now), grace.Milliseconds(), limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM workflows
		 WHERE kind IN `+inClause(len(kinds))+` AND (
		   status = 'pending'
		   OR (status = 'waiting' AND wake_at_ms <= ?)
		   OR (status = 'running' AND ? >= lease_until_ms + ?)
		 )
		 ORDER BY created_at_ms LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("journal: scan: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("journal: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// claim takes the lease of id for runner. It succeeds only for a workflow of one
// of kinds that is unheld, due from waiting, or running under a lease that ended
// more than grace ago. One conditional UPDATE decides, so two runners racing for
// the same workflow cannot both win. The returned epoch is the fence of every
// later write.
func (s *store) claim(ctx context.Context, id, runner string, kinds []string, now time.Time,
	lease, grace time.Duration) (epoch int64, ok bool, err error) {
	err = s.tx(ctx, "claim", false, func(tx *sql.Tx) error {
		args := []any{runner, ms(now.Add(lease)), ms(now), id}
		args = append(args, kindArgs(kinds)...)
		args = append(args, ms(now), ms(now), grace.Milliseconds())
		res, err := tx.ExecContext(ctx,
			`UPDATE workflows SET status = 'running', runner_id = ?, epoch = epoch + 1,
			   lease_until_ms = ?, wake_at_ms = NULL, updated_at_ms = ?
			 WHERE id = ? AND kind IN `+inClause(len(kinds))+` AND (
			   status = 'pending'
			   OR (status = 'waiting' AND wake_at_ms <= ?)
			   OR (status = 'running' AND ? >= lease_until_ms + ?)
			 )`, args...)
		if err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		ok = true
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM workflows WHERE id = ?`, id).Scan(&epoch); err != nil {
			return fmt.Errorf("journal: claim: %w", err)
		}
		return nil
	})
	return epoch, ok, err
}

// renew extends the lease while f still owns it and the lease has not run out.
func (s *store) renew(ctx context.Context, f fence, now time.Time, lease time.Duration) (bool, error) {
	var ok bool
	err := s.tx(ctx, "renew", false, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE workflows SET lease_until_ms = ?, updated_at_ms = ?
			 WHERE id = ? AND runner_id = ? AND epoch = ? AND status = 'running' AND lease_until_ms > ?`,
			ms(now.Add(lease)), ms(now), f.id, f.runner, f.epoch, ms(now))
		if err != nil {
			return fmt.Errorf("journal: renew: %w", err)
		}
		n, _ := res.RowsAffected()
		ok = n == 1
		return nil
	})
	return ok, err
}

// releasedByOwner reports whether the workflow is still at f's epoch with no
// runner: the owner itself suspended, finished or released it. A renewal that finds
// the lease gone checks this to tell its own release from a takeover.
func (s *store) releasedByOwner(ctx context.Context, f fence) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM workflows WHERE id = ? AND epoch = ? AND runner_id = ''`, f.id, f.epoch).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("journal: read owner: %w", err)
	}
	return true, nil
}

// release hands a workflow back at a clean stop: no runner, status pending, so the
// next claimer does not wait out the grace period. The step that was running (if
// any) stays unfinished and is re-run with the next attempt number.
func (s *store) release(ctx context.Context, f fence, now time.Time) error {
	return s.tx(ctx, "release", false, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE workflows SET status = 'pending', runner_id = '', lease_until_ms = 0, updated_at_ms = ?
			 WHERE id = ? AND runner_id = ? AND epoch = ? AND status = 'running'`,
			ms(now), f.id, f.runner, f.epoch)
		if err != nil {
			return fmt.Errorf("journal: release: %w", err)
		}
		return nil
	})
}

// terminalTx moves the workflow to a terminal status and clears its lease, inside
// a transaction that already passed the fence.
func terminalTx(ctx context.Context, tx *sql.Tx, id string, status Status, msg string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE workflows SET status = ?, error = ?, runner_id = '', lease_until_ms = 0, wake_at_ms = NULL,
		   cancel_requested = 0, updated_at_ms = ?, finished_at_ms = ?
		 WHERE id = ?`, string(status), truncate(msg, maxErrorBytes), ms(now), ms(now), id)
	if err != nil {
		return fmt.Errorf("journal: finish workflow: %w", err)
	}
	return nil
}

// suspendTx persists the workflow as waiting until wake and releases the lease.
func suspendTx(ctx context.Context, tx *sql.Tx, id string, wake, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE workflows SET status = 'waiting', wake_at_ms = ?, runner_id = '', lease_until_ms = 0, updated_at_ms = ?
		 WHERE id = ?`, ms(wake), ms(now), id)
	if err != nil {
		return fmt.Errorf("journal: suspend workflow: %w", err)
	}
	return nil
}

// finishWorkflow records the end of a workflow the function ended on its own
// (returned nil, returned an error, panicked, or did not match its journal).
func (s *store) finishWorkflow(ctx context.Context, f fence, now time.Time, status Status, msg string) error {
	return s.fenced(ctx, "finish_workflow", f, now, func(tx *sql.Tx) error {
		return terminalTx(ctx, tx, f.id, status, msg, now)
	})
}

// cancelBoundary reads the durable cancel request. If one is set it finishes the
// workflow as cancelled in the same transaction and reports it.
func cancelBoundary(ctx context.Context, tx *sql.Tx, id string, now time.Time) (cancelled bool, err error) {
	var req int
	var reason string
	if err := tx.QueryRowContext(ctx,
		`SELECT cancel_requested, cancel_reason FROM workflows WHERE id = ?`, id).Scan(&req, &reason); err != nil {
		return false, fmt.Errorf("journal: read cancel request: %w", err)
	}
	if req == 0 {
		return false, nil
	}
	if err := terminalTx(ctx, tx, id, StatusCancelled, reason, now); err != nil {
		return false, err
	}
	return true, nil
}

// beginResult is the outcome of journaling a step as started.
type beginResult struct {
	attempt   int
	failures  int
	firstAt   time.Time
	cancelled bool
	failed    string // set when the workflow was failed instead (attempt limit)
}

// beginStep journals "step seq started, attempt n" before the step function
// runs, in a fenced transaction that also reads the durable cancel request. A
// step that is already journaled (a crash re-run, or the next try after a
// failure) gets attempts+1; one that has used up MaxStepAttempts fails the
// workflow instead of running again.
func (s *store) beginStep(ctx context.Context, f fence, now time.Time, seq int, name string, existing bool) (beginResult, error) {
	var res beginResult
	err := s.fenced(ctx, "begin_step", f, now, func(tx *sql.Tx) error {
		cancelled, err := cancelBoundary(ctx, tx, f.id, now)
		if err != nil {
			return err
		}
		if cancelled {
			res.cancelled = true
			return nil
		}
		if !existing {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms)
				 VALUES (?, ?, ?, 1, ?, ?)`, f.id, seq, name, ms(now), ms(now)); err != nil {
				return fmt.Errorf("journal: journal step start: %w", err)
			}
			// Millisecond precision, as every later attempt will read it back from the row.
			res.attempt, res.firstAt = 1, fromMS(ms(now))
			return nil
		}
		var attempts, failures int
		var first int64
		if err := tx.QueryRowContext(ctx,
			`SELECT attempts, failures, first_started_at_ms FROM workflow_steps WHERE workflow_id = ? AND seq = ? AND name = ?`,
			f.id, seq, name).Scan(&attempts, &failures, &first); err != nil {
			return fmt.Errorf("journal: read step: %w", err)
		}
		if attempts >= MaxStepAttempts {
			res.failed = fmt.Sprintf("step %q was started %d times without finishing; giving up", name, attempts)
			return terminalTx(ctx, tx, f.id, StatusFailed, res.failed, now)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_steps SET attempts = attempts + 1, started_at_ms = ?, wake_at_ms = NULL
			 WHERE workflow_id = ? AND seq = ?`, ms(now), f.id, seq); err != nil {
			return fmt.Errorf("journal: journal step start: %w", err)
		}
		res.attempt, res.failures, res.firstAt = attempts+1, failures, fromMS(first)
		return nil
	})
	return res, err
}

// finishStep journals the end of a step: its output, or its final error. A
// non-empty failWorkflow also fails the workflow in the same transaction, for
// outcomes after which the step must never run again whatever the function does
// next (an output that cannot be stored, a panic).
func (s *store) finishStep(ctx context.Context, f fence, now time.Time, seq int, output []byte, errText, failWorkflow string) error {
	return s.fenced(ctx, "finish_step", f, now, func(tx *sql.Tx) error {
		var out any
		if errText == "" {
			out = string(output)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_steps SET output = ?, error = ?, finished_at_ms = ?, wake_at_ms = NULL
			 WHERE workflow_id = ? AND seq = ?`, out, truncate(errText, maxErrorBytes), ms(now), f.id, seq); err != nil {
			return fmt.Errorf("journal: journal step end: %w", err)
		}
		if failWorkflow != "" {
			return terminalTx(ctx, tx, f.id, StatusFailed, failWorkflow, now)
		}
		return nil
	})
}

// retryStep journals a failed attempt that will be retried at retryAt and, when
// that is in the future, suspends the workflow until then.
func (s *store) retryStep(ctx context.Context, f fence, now time.Time, seq int, errText string, retryAt time.Time, suspend bool) error {
	return s.fenced(ctx, "retry_step", f, now, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflow_steps SET error = ?, failures = failures + 1, wake_at_ms = ?
			 WHERE workflow_id = ? AND seq = ?`, truncate(errText, maxErrorBytes), ms(retryAt), f.id, seq); err != nil {
			return fmt.Errorf("journal: journal step retry: %w", err)
		}
		if suspend {
			return suspendTx(ctx, tx, f.id, retryAt, now)
		}
		return nil
	})
}

// suspend persists the workflow as waiting until wake (a step whose backoff has
// not yet elapsed when the workflow resumed).
func (s *store) suspend(ctx context.Context, f fence, now, wake time.Time) error {
	return s.fenced(ctx, "suspend", f, now, func(tx *sql.Tx) error {
		return suspendTx(ctx, tx, f.id, wake, now)
	})
}

// sleepOutcome is what journaling a SleepUntil did.
type sleepOutcome int

const (
	sleepDone sleepOutcome = iota
	sleepSuspended
	sleepCancelled
)

// sleepStep journals a "$sleep" record. A new record stores the wake time; one
// from an earlier run keeps its stored time. If the time has come the record is
// finished, otherwise the workflow is suspended until then, in the same
// transaction.
func (s *store) sleepStep(ctx context.Context, f fence, now time.Time, seq int, existing bool, wake time.Time) (sleepOutcome, error) {
	out := sleepDone
	err := s.fenced(ctx, "sleep_step", f, now, func(tx *sql.Tx) error {
		cancelled, err := cancelBoundary(ctx, tx, f.id, now)
		if err != nil {
			return err
		}
		if cancelled {
			out = sleepCancelled
			return nil
		}
		due := !wake.After(now)
		if !existing {
			if due {
				_, err = tx.ExecContext(ctx,
					`INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms, finished_at_ms, wake_at_ms, output)
					 VALUES (?, ?, '$sleep', 1, ?, ?, ?, ?, 'null')`, f.id, seq, ms(now), ms(now), ms(now), ms(wake))
			} else {
				_, err = tx.ExecContext(ctx,
					`INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms, wake_at_ms)
					 VALUES (?, ?, '$sleep', 1, ?, ?, ?)`, f.id, seq, ms(now), ms(now), ms(wake))
			}
		} else if due {
			_, err = tx.ExecContext(ctx,
				`UPDATE workflow_steps SET finished_at_ms = ?, output = 'null' WHERE workflow_id = ? AND seq = ?`,
				ms(now), f.id, seq)
		}
		if err != nil {
			return fmt.Errorf("journal: journal sleep: %w", err)
		}
		if due {
			return nil
		}
		out = sleepSuspended
		return suspendTx(ctx, tx, f.id, wake, now)
	})
	return out, err
}

// nowStep journals a "$now" record holding the time the workflow first asked.
func (s *store) nowStep(ctx context.Context, f fence, now time.Time, seq int) error {
	return s.fenced(ctx, "now_step", f, now, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_steps (workflow_id, seq, name, attempts, started_at_ms, first_started_at_ms, finished_at_ms, output)
			 VALUES (?, ?, '$now', 1, ?, ?, ?, ?)`, f.id, seq, ms(now), ms(now), ms(now), fmt.Sprint(ms(now)))
		if err != nil {
			return fmt.Errorf("journal: journal now: %w", err)
		}
		return nil
	})
}

// cancel records a cancel request. A workflow nobody holds (pending or waiting)
// is finished as cancelled at once; a running one is flagged and its runner stops
// at its next step boundary. The result is the status the workflow now has.
func (s *store) cancel(ctx context.Context, id, reason string, now time.Time) (Status, bool, error) {
	var (
		status  Status
		changed bool
	)
	err := s.tx(ctx, "cancel", true, func(tx *sql.Tx) error {
		var cur string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM workflows WHERE id = ?`, id).Scan(&cur); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("journal: cancel: %w", err)
		}
		status = Status(cur)
		switch status {
		case StatusPending, StatusWaiting:
			if err := terminalTx(ctx, tx, id, StatusCancelled, reason, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE workflows SET cancel_reason = ? WHERE id = ?`, truncate(reason, maxReasonBytes), id); err != nil {
				return fmt.Errorf("journal: cancel: %w", err)
			}
			status, changed = StatusCancelled, true
		case StatusRunning:
			if _, err := tx.ExecContext(ctx,
				`UPDATE workflows SET cancel_requested = 1, cancel_reason = ?, updated_at_ms = ? WHERE id = ?`,
				truncate(reason, maxReasonBytes), ms(now), id); err != nil {
				return fmt.Errorf("journal: cancel: %w", err)
			}
		}
		return nil
	})
	return status, changed, err
}

// prune deletes up to limit finished workflows that ended before cutoff, with
// their steps. Unfinished workflows are never touched.
func (s *store) prune(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	n := 0
	err := s.tx(ctx, "prune", false, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM workflows
			 WHERE status IN ('succeeded','failed','cancelled') AND finished_at_ms < ?
			 ORDER BY finished_at_ms, id LIMIT ?`, ms(cutoff), limit)
		if err != nil {
			return fmt.Errorf("journal: prune: %w", err)
		}
		var ids []any
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("journal: prune: %w", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("journal: prune: %w", err)
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil
		}
		in := inClause(len(ids))
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_steps WHERE workflow_id IN `+in, ids...); err != nil {
			return fmt.Errorf("journal: prune: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id IN `+in, ids...); err != nil {
			return fmt.Errorf("journal: prune: %w", err)
		}
		n = len(ids)
		return nil
	})
	return n, err
}
