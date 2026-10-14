package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Failure-path helpers shared by the open / resize / unwind tools.
//
// Delta-neutral execution places REAL orders. Once an exchange order has been accepted
// the position exists whatever happens to our bookkeeping afterwards, so these helpers
// make sure a bookkeeping failure is (a) retried, (b) traced, and (c) escalated to
// recovery_required instead of being reported as "the order failed".

const (
	// legSaveMaxAttempts is how many times a filled leg row is written before the
	// fill is declared "unrecorded".
	legSaveMaxAttempts = 3

	// bookkeepingTimeout bounds the post-fill persistence calls. They run on a context
	// detached from the caller's cancellation so a cancelled request cannot turn a
	// filled order into an orphan.
	bookkeepingTimeout = 15 * time.Second
)

var (
	// saveExecutionLegFn is the single seam through which filled legs are persisted.
	// Production code never replaces it; tests swap it to simulate a failing store.
	saveExecutionLegFn = func(ctx context.Context, s *deltaneutral.Store, leg *deltaneutral.ExecutionLeg) (int64, error) {
		return s.SaveExecutionLeg(ctx, leg)
	}

	// legSaveBackoff is the pause between leg-save attempts (tests set it to 0).
	legSaveBackoff = 100 * time.Millisecond
)

// legOutcome is what a leg helper reports. The helper's separate error return is the
// ORDER error (no order was accepted, so nothing changed on the exchange). Once an
// order has been accepted Filled is true and the error return is nil; if recording the
// leg then failed, BookkeepingErr carries the trace. Callers must never treat a
// BookkeepingErr as "not filled".
type legOutcome struct {
	Filled         bool
	BookkeepingErr error
}

// bookkeepingContext returns a context for post-fill persistence that survives
// cancellation of the request context.
func bookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
}

// joinMsg appends addition to msg with a separator, ignoring empty parts.
func joinMsg(msg, addition string) string {
	switch {
	case msg == "":
		return addition
	case addition == "":
		return msg
	default:
		return msg + " | " + addition
	}
}

// execErrMsg builds an execution error_msg from msg and re-attaches the traces of any
// unrecorded fills, so a later overwrite of exec.ErrorMsg can never drop them.
func execErrMsg(msg string, bookkeepingErrs ...error) string {
	out := msg
	for _, e := range bookkeepingErrs {
		if e != nil {
			out = joinMsg(out, e.Error())
		}
	}
	return out
}

// bkNote renders an unrecorded-fill trace for a user-facing result ("" when nil).
func bkNote(err error) string {
	if err == nil {
		return ""
	}
	return "\nUnrecorded fill: " + err.Error()
}

// saveLegWithRetry writes a leg, retrying up to legSaveMaxAttempts times with a short
// backoff. It returns the last error, or nil on success.
func saveLegWithRetry(ctx context.Context, store *deltaneutral.Store, exec *deltaneutral.Execution, leg *deltaneutral.ExecutionLeg) error {
	var saveErr error
	for attempt := 1; attempt <= legSaveMaxAttempts; attempt++ {
		if _, saveErr = saveExecutionLegFn(ctx, store, leg); saveErr == nil {
			return nil
		}
		logger.WarnCF("deltaneutral", "Failed to save leg, retrying", map[string]any{
			"execution_id": exec.ID, "order_id": leg.OrderID, "attempt": attempt, "error": saveErr.Error(),
		})
		if attempt < legSaveMaxAttempts && legSaveBackoff > 0 {
			select {
			case <-time.After(legSaveBackoff):
			case <-ctx.Done():
			}
		}
	}
	return saveErr
}

// saveFilledLeg persists a leg whose exchange order has ALREADY been accepted.
//
// It retries SaveExecutionLeg up to legSaveMaxAttempts times. When every attempt fails
// it logs at ERROR, writes a minimal trace (marker, order id, symbol, side, quantity)
// into the execution's error_msg via UpdateExecution, and returns a non-nil error whose
// text is that trace. The caller must treat the leg as FILLED regardless and finish the
// operation in recovery_required.
func saveFilledLeg(ctx context.Context, store *deltaneutral.Store, exec *deltaneutral.Execution, leg *deltaneutral.ExecutionLeg) error {
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()

	saveErr := saveLegWithRetry(bctx, store, exec, leg)
	if saveErr == nil {
		return nil
	}

	trace := fmt.Sprintf("%s order_id=%s leg=%s symbol=%s side=%s filled_qty=%.8g (leg record not saved after %d attempts: %v)",
		deltaneutral.UnrecordedFillMarker, leg.OrderID, leg.LegType, leg.Symbol, leg.Side,
		leg.FilledQuantity, legSaveMaxAttempts, saveErr)
	logger.ErrorCF("deltaneutral", "ORDER FILLED BUT LEG RECORD NOT SAVED", map[string]any{
		"execution_id": exec.ID, "plan_id": exec.PlanID, "order_id": leg.OrderID, "leg": leg.LegType,
		"symbol": leg.Symbol, "side": leg.Side, "filled_qty": leg.FilledQuantity, "error": saveErr.Error(),
	})

	// Fallback trace on the execution row.
	exec.ErrorMsg = joinMsg(exec.ErrorMsg, trace)
	if err := store.UpdateExecution(bctx, exec); err != nil {
		logger.ErrorCF("deltaneutral", "Could not persist unrecorded-fill trace on execution", map[string]any{
			"execution_id": exec.ID, "order_id": leg.OrderID, "error": err.Error(),
		})
	}
	return errors.New(trace)
}

// escalateToRecovery is the best-effort "something went wrong after an order was
// accepted" exit: it moves the execution (when non-nil) and the plan to
// recovery_required with errors logged at ERROR rather than discarded. It returns a
// note, empty on success, that the caller appends to the user-facing result when the
// escalation itself could not be fully persisted.
func escalateToRecovery(ctx context.Context, store *deltaneutral.Store, planID int64, exec *deltaneutral.Execution, msg string) string {
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()

	var failures []string
	if exec != nil {
		transition(exec, deltaneutral.ExecutionStateRecoveryRequired)
		exec.ErrorMsg = msg
		if err := store.UpdateExecution(bctx, exec); err != nil {
			logger.ErrorCF("deltaneutral", "Could not move execution to recovery_required", map[string]any{
				"execution_id": exec.ID, "plan_id": planID, "error": err.Error(),
			})
			failures = append(failures, fmt.Sprintf("execution %d: %v", exec.ID, err))
		}
	}
	if err := store.UpdatePlanStatus(bctx, planID, deltaneutral.PlanStatusRecoveryRequired); err != nil {
		logger.ErrorCF("deltaneutral", "Could not move plan to recovery_required", map[string]any{
			"plan_id": planID, "error": err.Error(),
		})
		failures = append(failures, fmt.Sprintf("plan %d: %v", planID, err))
	}
	if len(failures) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"\nWARNING: could not persist recovery_required state (%s). Treat plan %d as recovery_required: "+
			"do NOT re-open it; verify the exchange positions manually.",
		strings.Join(failures, "; "), planID)
}

// transition moves exec through the state machine (Execution.Transition) and is the
// only way these tools change an execution's state. An illegal transition is
// logged at ERROR and leaves exec.State where it was, so the UpdateExecution that
// follows rewrites the same state: it cannot slip past the table.
func transition(exec *deltaneutral.Execution, to deltaneutral.ExecutionState) {
	if err := exec.Transition(to); err != nil {
		logger.ErrorCF("deltaneutral", "Illegal execution state transition refused", map[string]any{
			"execution_id": exec.ID, "from": exec.State, "to": string(to), "error": err.Error(),
		})
	}
}

// setPlanStatusLogged updates a plan status, logging (never discarding) a failure.
func setPlanStatusLogged(ctx context.Context, store *deltaneutral.Store, planID int64, status string) {
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := store.UpdatePlanStatus(bctx, planID, status); err != nil {
		logger.ErrorCF("deltaneutral", "Could not update plan status", map[string]any{
			"plan_id": planID, "status": status, "error": err.Error(),
		})
	}
}

// updateExecutionLogged persists exec, logging (never discarding) a failure.
func updateExecutionLogged(ctx context.Context, store *deltaneutral.Store, exec *deltaneutral.Execution) {
	bctx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := store.UpdateExecution(bctx, exec); err != nil {
		logger.ErrorCF("deltaneutral", "Could not update execution", map[string]any{
			"execution_id": exec.ID, "state": exec.State, "error": err.Error(),
		})
	}
}
