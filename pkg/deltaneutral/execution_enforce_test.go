package deltaneutral

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedExec(t *testing.T, store *Store, name string, state ExecutionState) *Execution {
	t.Helper()
	ctx := context.Background()
	planID, err := store.SavePlan(ctx, createTestPlan(t, name))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := &Execution{PlanID: planID, AttemptID: "a-" + name, State: string(state), RequestedAt: now, CreatedAt: now, UpdatedAt: now}
	if _, err := store.SaveExecution(ctx, e); err != nil {
		t.Fatal(err)
	}
	return e
}

// TestIllegalTransitionRejected: UpdateExecution is the enforcement. For EVERY
// (from, to) pair of the 14 states it applies exactly the transitions the table
// allows (and the rewrite of the same state), and refuses all the others with
// ErrIllegalTransition, leaving the stored row as it was.
func TestIllegalTransitionRejected(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	for _, from := range allExecutionStates {
		for _, to := range allExecutionStates {
			e := seedExec(t, store, string(from)+"-"+string(to), from)
			e.State = string(to)
			e.ErrorMsg = "msg"
			err := store.UpdateExecution(ctx, e)
			legal := from == to || CanTransition(from, to)
			got, gerr := store.GetExecution(ctx, e.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if legal {
				if err != nil || got.State != string(to) || got.ErrorMsg != "msg" {
					t.Errorf("%s -> %s is legal: err=%v stored=%q/%q", from, to, err, got.State, got.ErrorMsg)
				}
				continue
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("%s -> %s: err = %v, want ErrIllegalTransition", from, to, err)
			}
			if got.State != string(from) || got.ErrorMsg != "" {
				t.Errorf("%s -> %s was refused but the row changed: %q/%q", from, to, got.State, got.ErrorMsg)
			}
		}
	}
}

// TestPerformedTransitionsAreLegal is the inventory of every state change the open,
// resize, unwind and recovery code performs (including escalateToRecovery, which
// is called from wherever an order may have been accepted). Enforcement refuses
// anything outside the table, so a pair missing here is a flow that would be
// refused in production.
func TestPerformedTransitionsAreLegal(t *testing.T) {
	S := func(a ...ExecutionState) []ExecutionState { return a }
	flows := map[string][]ExecutionState{
		"open/resize, first leg refused":         S(ExecutionStateValidating, ExecutionStateAwaitingApproval, ExecutionStatePlacingFirstLeg, ExecutionStateFirstLegFailed, ExecutionStateFailed),
		"open/resize, validation refused":        S(ExecutionStateValidating, ExecutionStateFailed),
		"open/resize, success":                   S(ExecutionStateValidating, ExecutionStateAwaitingApproval, ExecutionStatePlacingFirstLeg, ExecutionStateFirstLegFilled, ExecutionStatePlacingSecondLeg, ExecutionStateBothLegsFilled),
		"open/resize, second leg failed":         S(ExecutionStatePlacingSecondLeg, ExecutionStateSecondLegFailed, ExecutionStateRecoveryRequired),
		"escalate, first leg outcome unknown":    S(ExecutionStatePlacingFirstLeg, ExecutionStateRecoveryRequired),
		"escalate, bookkeeping after 1st leg":    S(ExecutionStateFirstLegFilled, ExecutionStateRecoveryRequired),
		"escalate, second leg unknown/unsaved":   S(ExecutionStatePlacingSecondLeg, ExecutionStateRecoveryRequired),
		"unwind, success":                        S(ExecutionStateUnwinding, ExecutionStateUnwound),
		"unwind, nothing closed":                 S(ExecutionStateUnwinding, ExecutionStateFailed),
		"unwind, half closed / guard":            S(ExecutionStateUnwinding, ExecutionStateRecoveryRequired),
		"recovery ends (dn.recover finalize)":    S(ExecutionStateRecoveryRequired, ExecutionStateFailed),
		"recovery, owner unwinds by hand":        S(ExecutionStateRecoveryRequired, ExecutionStateUnwinding),
		"escalate repeated (same-state rewrite)": S(ExecutionStateRecoveryRequired, ExecutionStateRecoveryRequired),
	}
	for name, path := range flows {
		for i := 0; i+1 < len(path); i++ {
			if path[i] != path[i+1] && !CanTransition(path[i], path[i+1]) {
				t.Errorf("%s: %s -> %s is performed by the code but not in the table", name, path[i], path[i+1])
			}
		}
	}
}

func TestUpdateExecution_UnknownStateAndMissingRow(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	e := seedExec(t, store, "bogus", ExecutionStatePending)
	e.State = "bogus"
	if err := store.UpdateExecution(ctx, e); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("unknown state: %v", err)
	}
	e.State, e.ID = string(ExecutionStatePending), 99999
	if err := store.UpdateExecution(ctx, e); err == nil || errors.Is(err, ErrIllegalTransition) {
		t.Errorf("missing row: %v", err)
	}
}

// A caller holding a stale copy cannot jump the machine: the comparison is with the row.
func TestUpdateExecution_StaleCopyCannotSkip(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	e := seedExec(t, store, "stale", ExecutionStateValidating)
	stale := *e
	e.State = string(ExecutionStateFailed)
	if err := store.UpdateExecution(ctx, e); err != nil {
		t.Fatal(err)
	}
	stale.State = string(ExecutionStateAwaitingApproval) // legal from the copy's validating, not from the row's failed
	if err := store.UpdateExecution(ctx, &stale); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("stale copy moved a terminal execution: %v", err)
	}
}

func TestExecutionTransition(t *testing.T) {
	e := &Execution{State: string(ExecutionStatePending)}
	if err := e.Transition(ExecutionStateBothLegsFilled); !errors.Is(err, ErrIllegalTransition) || e.State != "pending" {
		t.Errorf("pending -> both_legs_filled: err=%v state=%q", err, e.State)
	}
	if err := e.Transition(ExecutionStateValidating); err != nil || e.State != "validating" {
		t.Errorf("pending -> validating: err=%v state=%q", err, e.State)
	}
	if err := e.Transition(ExecutionStateValidating); err != nil {
		t.Errorf("repeating a transition must be a no-op: %v", err)
	}
	if err := e.Transition("nope"); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("unknown target: %v", err)
	}
}

func TestExecutionByAttempt(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	if got, err := store.ExecutionByAttempt(ctx, "nope"); err != nil || got != nil {
		t.Fatalf("missing attempt: %v %v", got, err)
	}
	e := seedExec(t, store, "byattempt", ExecutionStatePending)
	got, err := store.ExecutionByAttempt(ctx, e.AttemptID)
	if err != nil || got == nil || got.ID != e.ID {
		t.Fatalf("lookup: %v %v", got, err)
	}
}

// A partially filled leg is real exposure: it must count toward the open
// quantities, the exposure check that blocks a re-open, and the fee total.
func TestOpenLegExposure_CountsPartiallyFilledLegs(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	e := seedExec(t, store, "partial", ExecutionStatePlacingSecondLeg)
	now := time.Now()
	leg := &ExecutionLeg{ExecutionID: e.ID, LegType: string(LegTypeFutures), Symbol: "X", Side: "sell", FilledQuantity: 7,
		FeeUSDT: -1.5, State: string(LegStatePartiallyFilled), CreatedAt: now, UpdatedAt: now}
	if _, err := store.SaveExecutionLeg(ctx, leg); err != nil {
		t.Fatal(err)
	}
	fut, spot, err := store.OpenLegQuantities(ctx, e.PlanID)
	if err != nil || fut != 7 || spot != 0 {
		t.Errorf("OpenLegQuantities = %v, %v, %v; want 7, 0", fut, spot, err)
	}
	if has, err := store.HasOpenExposure(ctx, e.PlanID); err != nil || !has {
		t.Errorf("HasOpenExposure = %v, %v; a partial fill is an open position", has, err)
	}
	if fees, err := store.SumPlanExecutionFees(ctx, e.PlanID); err != nil || fees != -1.5 {
		t.Errorf("fees = %v, %v", fees, err)
	}
}
