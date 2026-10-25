package deltaneutral

import (
	"testing"
)

// TestExecutionStateValid tests that all defined execution state constants are valid.
func TestExecutionStateValid(t *testing.T) {
	validStates := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFailed,
		ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg,
		ExecutionStateSecondLegFailed,
		ExecutionStateBothLegsFilled,
		ExecutionStateRecoveryRequired,
		ExecutionStateUnwinding,
		ExecutionStateUnwound,
		ExecutionStateFailed,
		ExecutionStateCancelled,
	}

	for _, state := range validStates {
		if !state.Valid() {
			t.Errorf("ExecutionState %q should be valid", state)
		}
	}

	// Test invalid state
	invalidState := ExecutionState("invalid_state")
	if invalidState.Valid() {
		t.Errorf("ExecutionState %q should be invalid", invalidState)
	}
}

// TestLegStateValid tests that all defined leg state constants are valid.
func TestLegStateValid(t *testing.T) {
	validStates := []LegState{
		LegStatePending,
		LegStatePlacing,
		LegStateOpen,
		LegStatePartiallyFilled,
		LegStateFilled,
		LegStateFailed,
		LegStateCancelled,
		LegStateUnwinding,
		LegStateUnwound,
	}

	for _, state := range validStates {
		if !state.Valid() {
			t.Errorf("LegState %q should be valid", state)
		}
	}

	// Test invalid state
	invalidState := LegState("invalid_leg_state")
	if invalidState.Valid() {
		t.Errorf("LegState %q should be invalid", invalidState)
	}
}

// TestHappyPath tests the happy path: pending -> validating -> awaiting_approval -> placing_first_leg -> first_leg_filled -> placing_second_leg -> both_legs_filled.
func TestHappyPath(t *testing.T) {
	path := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg,
		ExecutionStateBothLegsFilled,
	}

	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("legal transition %s -> %s failed", path[i], path[i+1])
		}
	}
}

// TestFirstLegFailPath tests the first-leg-fail path: placing_first_leg -> first_leg_failed -> failed.
func TestFirstLegFailPath(t *testing.T) {
	path := []ExecutionState{
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFailed,
		ExecutionStateFailed,
	}

	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("legal transition in first-leg-fail path %s -> %s failed", path[i], path[i+1])
		}
	}

	// Verify that after first_leg_failed, we cannot place the second leg
	if CanTransition(ExecutionStateFirstLegFailed, ExecutionStatePlacingSecondLeg) {
		t.Error("should not be able to transition from first_leg_failed to placing_second_leg")
	}
}

// TestSecondLegFailPath tests the second-leg-fail path: placing_second_leg -> second_leg_failed -> recovery_required -> unwinding -> unwound.
func TestSecondLegFailPath(t *testing.T) {
	path := []ExecutionState{
		ExecutionStatePlacingSecondLeg,
		ExecutionStateSecondLegFailed,
		ExecutionStateRecoveryRequired,
		ExecutionStateUnwinding,
		ExecutionStateUnwound,
	}

	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("legal transition in second-leg-fail path %s -> %s failed", path[i], path[i+1])
		}
	}
}

// TestRecoveryFailurePath tests the recovery failure path: recovery_required -> failed.
func TestRecoveryFailurePath(t *testing.T) {
	if !CanTransition(ExecutionStateRecoveryRequired, ExecutionStateFailed) {
		t.Error("should be able to transition from recovery_required to failed")
	}
}

// TestIllegalTransitions tests that various illegal transitions return false.
func TestIllegalTransitions(t *testing.T) {
	illegalTransitions := []struct {
		from ExecutionState
		to   ExecutionState
	}{
		// Cannot skip states
		{ExecutionStatePending, ExecutionStateAwaitingApproval},
		{ExecutionStatePending, ExecutionStateBothLegsFilled},
		// Cannot go backward
		{ExecutionStateValidating, ExecutionStatePending},
		{ExecutionStateAwaitingApproval, ExecutionStateValidating},
		{ExecutionStatePlacingFirstLeg, ExecutionStateAwaitingApproval},
		// Cannot place second leg if first leg failed
		{ExecutionStateFirstLegFailed, ExecutionStatePlacingSecondLeg},
		// Cannot place first leg from both_legs_filled
		{ExecutionStateBothLegsFilled, ExecutionStatePlacingFirstLeg},
		// Cannot transition from terminal states
		{ExecutionStateUnwound, ExecutionStateUnwinding},
		{ExecutionStateFailed, ExecutionStateUnwinding},
		{ExecutionStateCancelled, ExecutionStateValidating},
		// Cannot reach both_legs_filled from first_leg_failed
		{ExecutionStateFirstLegFailed, ExecutionStateBothLegsFilled},
		// Cannot reach unwinding directly from placing_first_leg
		{ExecutionStatePlacingFirstLeg, ExecutionStateUnwinding},
	}

	for _, tt := range illegalTransitions {
		if CanTransition(tt.from, tt.to) {
			t.Errorf("illegal transition %s -> %s should return false", tt.from, tt.to)
		}
	}
}

// TestCancellationPaths tests that cancellation is allowed from early states.
func TestCancellationPaths(t *testing.T) {
	earlyStates := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
	}

	for _, state := range earlyStates {
		if !CanTransition(state, ExecutionStateCancelled) {
			t.Errorf("should be able to cancel from state %s", state)
		}
	}

	// After placing_first_leg, cancellation is not directly allowed (must go through failure/completion flow)
	if CanTransition(ExecutionStatePlacingFirstLeg, ExecutionStateCancelled) {
		t.Error("cancellation after placing_first_leg should not be directly allowed")
	}
}

// TestIsTerminal tests that terminal states are identified correctly.
func TestIsTerminal(t *testing.T) {
	terminalStates := []ExecutionState{
		ExecutionStateUnwound,
		ExecutionStateFailed,
		ExecutionStateCancelled,
		ExecutionStateBothLegsFilled, // Success terminal state
	}

	for _, state := range terminalStates {
		if !IsTerminal(state) {
			t.Errorf("state %s should be terminal", state)
		}
	}

	nonTerminalStates := []ExecutionState{
		ExecutionStatePending,
		ExecutionStateValidating,
		ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg,
		ExecutionStateFirstLegFailed,
		ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg,
		ExecutionStateSecondLegFailed,
		ExecutionStateRecoveryRequired,
		ExecutionStateUnwinding,
	}

	for _, state := range nonTerminalStates {
		if IsTerminal(state) {
			t.Errorf("state %s should not be terminal", state)
		}
	}
}

// TestFirstLegType tests the first leg type selection logic.
func TestFirstLegType(t *testing.T) {
	// Default: futures should be placed first
	legType := FirstLegType(false)
	if legType != LegTypeFutures {
		t.Errorf("FirstLegType(false) should return futures, got %s", legType)
	}

	// Spot less liquid: spot should be placed first
	legType = FirstLegType(true)
	if legType != LegTypeSpot {
		t.Errorf("FirstLegType(true) should return spot, got %s", legType)
	}
}

// TestAllowedTransitions tests that AllowedTransitions returns the correct set for each state.
func TestAllowedTransitions(t *testing.T) {
	tests := []struct {
		state           ExecutionState
		expectedAllowed []ExecutionState
	}{
		{
			ExecutionStatePending,
			[]ExecutionState{ExecutionStateValidating, ExecutionStateCancelled},
		},
		{
			ExecutionStateValidating,
			[]ExecutionState{ExecutionStateAwaitingApproval, ExecutionStateFailed, ExecutionStateCancelled},
		},
		{
			ExecutionStateAwaitingApproval,
			[]ExecutionState{ExecutionStatePlacingFirstLeg, ExecutionStateCancelled},
		},
		{
			ExecutionStatePlacingFirstLeg,
			[]ExecutionState{ExecutionStateFirstLegFilled, ExecutionStateFirstLegFailed, ExecutionStateRecoveryRequired},
		},
		{
			ExecutionStateFirstLegFailed,
			[]ExecutionState{ExecutionStateFailed},
		},
		{
			ExecutionStateFirstLegFilled,
			[]ExecutionState{ExecutionStatePlacingSecondLeg, ExecutionStateRecoveryRequired},
		},
		{
			ExecutionStatePlacingSecondLeg,
			[]ExecutionState{ExecutionStateBothLegsFilled, ExecutionStateSecondLegFailed, ExecutionStateRecoveryRequired},
		},
		{
			ExecutionStateSecondLegFailed,
			[]ExecutionState{ExecutionStateRecoveryRequired},
		},
		{
			ExecutionStateBothLegsFilled,
			[]ExecutionState{ExecutionStateUnwinding},
		},
		{
			ExecutionStateRecoveryRequired,
			[]ExecutionState{ExecutionStateUnwinding, ExecutionStateFailed},
		},
		{
			ExecutionStateUnwinding,
			[]ExecutionState{ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateRecoveryRequired},
		},
		{
			ExecutionStateUnwound,
			[]ExecutionState{},
		},
		{
			ExecutionStateFailed,
			[]ExecutionState{},
		},
		{
			ExecutionStateCancelled,
			[]ExecutionState{},
		},
	}

	for _, tt := range tests {
		allowed := AllowedTransitions(tt.state)
		if len(allowed) != len(tt.expectedAllowed) {
			t.Errorf("AllowedTransitions(%s) returned %d states, expected %d",
				tt.state, len(allowed), len(tt.expectedAllowed))
			continue
		}

		for _, expected := range tt.expectedAllowed {
			found := false
			for _, actual := range allowed {
				if actual == expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("AllowedTransitions(%s) missing expected state %s", tt.state, expected)
			}
		}
	}
}

// TestAttemptTransition tests the Attempt.Transition method.
func TestAttemptTransition(t *testing.T) {
	attempt := &Attempt{State: ExecutionStatePending}

	// Legal transition should succeed
	err := attempt.Transition(ExecutionStateValidating)
	if err != nil {
		t.Errorf("legal transition failed: %v", err)
	}
	if attempt.State != ExecutionStateValidating {
		t.Errorf("state should be validating after transition, got %s", attempt.State)
	}

	// Illegal transition should fail
	err = attempt.Transition(ExecutionStateBothLegsFilled)
	if err == nil {
		t.Error("illegal transition should have returned an error")
	}
	if attempt.State != ExecutionStateValidating {
		t.Errorf("state should remain validating after failed transition, got %s", attempt.State)
	}

	// Invalid target state should fail
	err = attempt.Transition(ExecutionState("unknown"))
	if err == nil {
		t.Error("transition to invalid state should have returned an error")
	}
}

// TestAttemptErrorType tests that transition errors have the expected format.
func TestAttemptErrorType(t *testing.T) {
	attempt := &Attempt{State: ExecutionStatePending}
	err := attempt.Transition(ExecutionStateBothLegsFilled)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	attemptErr, ok := err.(*AttemptError)
	if !ok {
		t.Fatalf("expected *AttemptError, got %T", err)
	}

	if attemptErr.From != ExecutionStatePending {
		t.Errorf("error from state should be %s, got %s", ExecutionStatePending, attemptErr.From)
	}
	if attemptErr.To != ExecutionStateBothLegsFilled {
		t.Errorf("error to state should be %s, got %s", ExecutionStateBothLegsFilled, attemptErr.To)
	}
	if attemptErr.Message == "" {
		t.Error("error message should not be empty")
	}
}

// TestUnwindingFailureEdges: an unwind that cannot finish must be able to leave
// "unwinding" for failed (nothing closed) or recovery_required (partially closed).
func TestUnwindingFailureEdges(t *testing.T) {
	for _, to := range []ExecutionState{ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateRecoveryRequired} {
		if !CanTransition(ExecutionStateUnwinding, to) {
			t.Errorf("unwinding -> %s must be legal", to)
		}
	}
	if CanTransition(ExecutionStateUnwinding, ExecutionStateBothLegsFilled) {
		t.Error("unwinding -> both_legs_filled must stay illegal")
	}
}

// TestUnwindingPath tests that successful executions can transition to unwinding.
func TestUnwindingPath(t *testing.T) {
	// After both legs filled, can unwind
	if !CanTransition(ExecutionStateBothLegsFilled, ExecutionStateUnwinding) {
		t.Error("should be able to transition from both_legs_filled to unwinding")
	}

	// Complete unwind path
	path := []ExecutionState{
		ExecutionStateBothLegsFilled,
		ExecutionStateUnwinding,
		ExecutionStateUnwound,
	}

	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("legal unwinding transition %s -> %s failed", path[i], path[i+1])
		}
	}
}

// TestStateStringValues tests that state constants have the correct string values.
func TestStateStringValues(t *testing.T) {
	tests := []struct {
		state    ExecutionState
		expected string
	}{
		{ExecutionStatePending, "pending"},
		{ExecutionStateValidating, "validating"},
		{ExecutionStateAwaitingApproval, "awaiting_approval"},
		{ExecutionStatePlacingFirstLeg, "placing_first_leg"},
		{ExecutionStateFirstLegFailed, "first_leg_failed"},
		{ExecutionStateFirstLegFilled, "first_leg_filled"},
		{ExecutionStatePlacingSecondLeg, "placing_second_leg"},
		{ExecutionStateSecondLegFailed, "second_leg_failed"},
		{ExecutionStateBothLegsFilled, "both_legs_filled"},
		{ExecutionStateRecoveryRequired, "recovery_required"},
		{ExecutionStateUnwinding, "unwinding"},
		{ExecutionStateUnwound, "unwound"},
		{ExecutionStateFailed, "failed"},
		{ExecutionStateCancelled, "cancelled"},
	}

	for _, tt := range tests {
		if string(tt.state) != tt.expected {
			t.Errorf("state %s has wrong value, expected %s", tt.state, tt.expected)
		}
	}
}

// TestLegStateStringValues tests that leg state constants have the correct string values.
func TestLegStateStringValues(t *testing.T) {
	tests := []struct {
		state    LegState
		expected string
	}{
		{LegStatePending, "pending"},
		{LegStatePlacing, "placing"},
		{LegStateOpen, "open"},
		{LegStatePartiallyFilled, "partially_filled"},
		{LegStateFilled, "filled"},
		{LegStateFailed, "failed"},
		{LegStateCancelled, "cancelled"},
		{LegStateUnwinding, "unwinding"},
		{LegStateUnwound, "unwound"},
	}

	for _, tt := range tests {
		if string(tt.state) != tt.expected {
			t.Errorf("leg state %s has wrong value, expected %s", tt.state, tt.expected)
		}
	}
}
