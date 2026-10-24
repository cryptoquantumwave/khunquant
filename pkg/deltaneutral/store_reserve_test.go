package deltaneutral

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func reserveRig(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.SavePlan(context.Background(), concurrencyTestPlan("reserve"))
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

func newReservation(planID int64, attempt string, state ExecutionState) *Execution {
	now := time.Now()
	return &Execution{PlanID: planID, AttemptID: attempt, State: string(state), RequestedAt: now, CreatedAt: now, UpdatedAt: now}
}

// Of many callers racing to take one plan, exactly one gets it; the others are told the
// plan is busy and write nothing.
func TestReserveExecution_ExactlyOneWinsARace(t *testing.T) {
	st, planID := reserveRig(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = st.ReserveExecution(context.Background(), newReservation(planID, "attempt-"+string(rune('a'+i)), ExecutionStateValidating))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrPlanBusy):
			if !strings.Contains(err.Error(), "already has an execution in progress") {
				t.Errorf("message: %v", err)
			}
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d callers reserved the plan, want exactly 1", won)
	}
	if rows, _ := st.ListExecutions(context.Background(), planID, 50, 0); len(rows) != 1 {
		t.Fatalf("%d rows written, want 1", len(rows))
	}
}

// A re-run of the workflow that holds the plan finds its own row and is not refused.
func TestReserveExecution_OwnAttemptIsNotRefused(t *testing.T) {
	st, planID := reserveRig(t)
	first := newReservation(planID, "same", ExecutionStatePlacingFirstLeg)
	if err := st.ReserveExecution(context.Background(), first); err != nil || first.ID == 0 {
		t.Fatalf("first: %v %+v", err, first)
	}
	again := newReservation(planID, "same", ExecutionStateValidating)
	if err := st.ReserveExecution(context.Background(), again); err != nil {
		t.Fatalf("a re-run refused itself: %v", err)
	}
	if again.ID != first.ID || again.State != string(ExecutionStatePlacingFirstLeg) {
		t.Errorf("want the stored row back, got %+v", again)
	}
	if err := st.ReserveExecution(context.Background(), newReservation(planID, "other", ExecutionStateValidating)); !errors.Is(err, ErrPlanBusy) {
		t.Fatalf("another attempt must be refused: %v", err)
	}
}

// Only an operation under way holds the plan: settled outcomes, other plans and an
// abandoned (stale) row do not.
func TestReserveExecution_WhatHoldsAPlan(t *testing.T) {
	ctx := context.Background()
	for _, st := range inFlightStates {
		t.Run("holds_"+string(st), func(t *testing.T) {
			s, planID := reserveRig(t)
			if err := s.ReserveExecution(ctx, newReservation(planID, "holder", st)); err != nil {
				t.Fatal(err)
			}
			if err := s.ReserveExecution(ctx, newReservation(planID, "next", ExecutionStateValidating)); !errors.Is(err, ErrPlanBusy) {
				t.Fatalf("%s must hold the plan: %v", st, err)
			}
		})
	}
	for _, st := range []ExecutionState{ExecutionStateBothLegsFilled, ExecutionStateRecoveryRequired, ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateCancelled} {
		t.Run("releases_"+string(st), func(t *testing.T) {
			s, planID := reserveRig(t)
			if err := s.ReserveExecution(ctx, newReservation(planID, "holder", st)); err != nil {
				t.Fatal(err)
			}
			if err := s.ReserveExecution(ctx, newReservation(planID, "next", ExecutionStateValidating)); err != nil {
				t.Fatalf("%s must not hold the plan: %v", st, err)
			}
		})
	}
	t.Run("other_plan", func(t *testing.T) {
		s, planID := reserveRig(t)
		other, err := s.SavePlan(ctx, concurrencyTestPlan("other"))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ReserveExecution(ctx, newReservation(planID, "a", ExecutionStateValidating)); err != nil {
			t.Fatal(err)
		}
		if err := s.ReserveExecution(ctx, newReservation(other, "b", ExecutionStateValidating)); err != nil {
			t.Fatalf("another plan is free: %v", err)
		}
	})
	setUpdated := func(t *testing.T, s *Store, id int64, updated string) {
		t.Helper()
		if _, err := s.db.Exec(`UPDATE delta_neutral_executions SET updated_at = ? WHERE id = ?`, updated, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("stale_row_does_not_lock_the_plan_for_good", func(t *testing.T) {
		s, planID := reserveRig(t)
		h := newReservation(planID, "abandoned", ExecutionStatePlacingFirstLeg)
		if err := s.ReserveExecution(ctx, h); err != nil {
			t.Fatal(err)
		}
		setUpdated(t, s, h.ID, time.Now().Add(-busyFor-time.Minute).Format(time.RFC3339))
		if err := s.ReserveExecution(ctx, newReservation(planID, "next", ExecutionStateValidating)); err != nil {
			t.Fatalf("a row untouched for longer than busyFor must not hold the plan: %v", err)
		}
	})
	t.Run("fresh_row_in_another_zone_still_holds", func(t *testing.T) {
		s, planID := reserveRig(t)
		h := newReservation(planID, "live", ExecutionStatePlacingFirstLeg)
		if err := s.ReserveExecution(ctx, h); err != nil {
			t.Fatal(err)
		}
		setUpdated(t, s, h.ID, time.Now().In(time.FixedZone("x", 7*3600)).Add(-time.Minute).Format(time.RFC3339))
		if err := s.ReserveExecution(ctx, newReservation(planID, "next", ExecutionStateValidating)); !errors.Is(err, ErrPlanBusy) {
			t.Fatalf("zone offsets must not shift the window: %v", err)
		}
	})
	t.Run("unreadable_timestamp_holds", func(t *testing.T) {
		s, planID := reserveRig(t)
		h := newReservation(planID, "odd", ExecutionStatePlacingFirstLeg)
		if err := s.ReserveExecution(ctx, h); err != nil {
			t.Fatal(err)
		}
		setUpdated(t, s, h.ID, "not a time")
		if err := s.ReserveExecution(ctx, newReservation(planID, "next", ExecutionStateValidating)); !errors.Is(err, ErrPlanBusy) {
			t.Fatalf("an unreadable timestamp must fail closed: %v", err)
		}
	})
}
