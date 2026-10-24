package deltaneutral

import (
	"errors"
	"fmt"
)

// ExecutionState represents the state of a two-leg execution attempt.
type ExecutionState string

const (
	ExecutionStatePending          ExecutionState = "pending"
	ExecutionStateValidating       ExecutionState = "validating"
	ExecutionStateAwaitingApproval ExecutionState = "awaiting_approval"
	ExecutionStatePlacingFirstLeg  ExecutionState = "placing_first_leg"
	ExecutionStateFirstLegFailed   ExecutionState = "first_leg_failed"
	ExecutionStateFirstLegFilled   ExecutionState = "first_leg_filled"
	ExecutionStatePlacingSecondLeg ExecutionState = "placing_second_leg"
	ExecutionStateSecondLegFailed  ExecutionState = "second_leg_failed"
	ExecutionStateBothLegsFilled   ExecutionState = "both_legs_filled"
	ExecutionStateRecoveryRequired ExecutionState = "recovery_required"
	ExecutionStateUnwinding        ExecutionState = "unwinding"
	ExecutionStateUnwound          ExecutionState = "unwound"
	ExecutionStateFailed           ExecutionState = "failed"
	ExecutionStateCancelled        ExecutionState = "cancelled"
)

// UnrecordedFillMarker prefixes the execution error_msg trace that is written when an
// exchange order went through but its leg row could not be persisted. The fill is then
// invisible to Store.OpenLegExposure, so any plan carrying this marker is treated as
// having open exposure and must be reconciled manually on the exchange.
const UnrecordedFillMarker = "UNRECORDED_FILL"

// LegState represents the state of a single execution leg (futures or spot).
type LegState string

const (
	LegStatePending         LegState = "pending"
	LegStatePlacing         LegState = "placing"
	LegStateOpen            LegState = "open"
	LegStatePartiallyFilled LegState = "partially_filled"
	LegStateFilled          LegState = "filled"
	LegStateFailed          LegState = "failed"
	LegStateCancelled       LegState = "cancelled"
	LegStateUnwinding       LegState = "unwinding"
	LegStateUnwound         LegState = "unwound"
)

// LegType represents the type of an execution leg.
type LegType string

const (
	LegTypeFutures LegType = "futures"
	LegTypeSpot    LegType = "spot"
)

// allExecutionStates is every state, for the store's "which states may move to X".
var allExecutionStates = []ExecutionState{
	ExecutionStatePending, ExecutionStateValidating, ExecutionStateAwaitingApproval, ExecutionStatePlacingFirstLeg,
	ExecutionStateFirstLegFailed, ExecutionStateFirstLegFilled, ExecutionStatePlacingSecondLeg,
	ExecutionStateSecondLegFailed, ExecutionStateBothLegsFilled, ExecutionStateRecoveryRequired,
	ExecutionStateUnwinding, ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateCancelled,
}

// ErrIllegalTransition is returned by Store.UpdateExecution (and wrapped from
// Execution.Transition) for a state change the table does not allow.
var ErrIllegalTransition = errors.New("deltaneutral: illegal execution state transition")

// AllowedTransitions returns the set of legal states reachable from the given state.
// If the state is invalid or terminal, returns an empty slice.
func AllowedTransitions(from ExecutionState) []ExecutionState {
	switch from {
	case ExecutionStatePending:
		return []ExecutionState{ExecutionStateValidating, ExecutionStateCancelled}
	case ExecutionStateValidating:
		return []ExecutionState{ExecutionStateAwaitingApproval, ExecutionStateFailed, ExecutionStateCancelled}
	case ExecutionStateAwaitingApproval:
		return []ExecutionState{ExecutionStatePlacingFirstLeg, ExecutionStateCancelled}
	// The three recovery_required edges below (from placing_first_leg,
	// first_leg_filled and placing_second_leg) are the ones MR-309 added when
	// UpdateExecution began to enforce the table: every one is a point at which an
	// order has been or may have been accepted by the exchange while the machine
	// cannot name an outcome (an unprovable placement, a leg record that could not
	// be saved), and the code already ends there by way of escalateToRecovery. The
	// alternatives, failed or a refused write, would hide a position.
	case ExecutionStatePlacingFirstLeg:
		return []ExecutionState{ExecutionStateFirstLegFilled, ExecutionStateFirstLegFailed, ExecutionStateRecoveryRequired}
	case ExecutionStateFirstLegFailed:
		return []ExecutionState{ExecutionStateFailed}
	case ExecutionStateFirstLegFilled:
		return []ExecutionState{ExecutionStatePlacingSecondLeg, ExecutionStateRecoveryRequired}
	case ExecutionStatePlacingSecondLeg:
		return []ExecutionState{ExecutionStateBothLegsFilled, ExecutionStateSecondLegFailed, ExecutionStateRecoveryRequired}
	case ExecutionStateSecondLegFailed:
		return []ExecutionState{ExecutionStateRecoveryRequired}
	case ExecutionStateBothLegsFilled:
		return []ExecutionState{ExecutionStateUnwinding}
	case ExecutionStateRecoveryRequired:
		return []ExecutionState{ExecutionStateUnwinding, ExecutionStateFailed}
	case ExecutionStateUnwinding:
		return []ExecutionState{ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateRecoveryRequired}
	case ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateCancelled:
		// Terminal states - no outgoing transitions
		return []ExecutionState{}
	default:
		return []ExecutionState{}
	}
}

// CanTransition returns true if a transition from 'from' to 'to' is legal.
func CanTransition(from, to ExecutionState) bool {
	allowed := AllowedTransitions(from)
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// IsTerminal returns true if the state is a terminal state (no further transitions possible).
// Terminal states are: unwound, failed, cancelled, and both_legs_filled (success).
func IsTerminal(s ExecutionState) bool {
	switch s {
	case ExecutionStateUnwound, ExecutionStateFailed, ExecutionStateCancelled, ExecutionStateBothLegsFilled:
		return true
	default:
		return false
	}
}

// Valid returns true if the execution state is a recognized state constant.
func (s ExecutionState) Valid() bool {
	switch s {
	case ExecutionStatePending, ExecutionStateValidating, ExecutionStateAwaitingApproval,
		ExecutionStatePlacingFirstLeg, ExecutionStateFirstLegFailed, ExecutionStateFirstLegFilled,
		ExecutionStatePlacingSecondLeg, ExecutionStateSecondLegFailed, ExecutionStateBothLegsFilled,
		ExecutionStateRecoveryRequired, ExecutionStateUnwinding, ExecutionStateUnwound,
		ExecutionStateFailed, ExecutionStateCancelled:
		return true
	default:
		return false
	}
}

// Valid returns true if the leg state is a recognized state constant.
func (l LegState) Valid() bool {
	switch l {
	case LegStatePending, LegStatePlacing, LegStateOpen, LegStatePartiallyFilled,
		LegStateFilled, LegStateFailed, LegStateCancelled, LegStateUnwinding, LegStateUnwound:
		return true
	default:
		return false
	}
}

// FirstLegType returns the leg type that should be placed first.
// By default, returns LegTypeFutures (place the futures hedge first).
// Returns LegTypeSpot only if spotLessLiquid is true, indicating spot is materially less liquid
// and should be hedged first to avoid drift.
func FirstLegType(spotLessLiquid bool) LegType {
	if spotLessLiquid {
		return LegTypeSpot
	}
	return LegTypeFutures
}

// AttemptError represents a state machine transition error.
type AttemptError struct {
	From    ExecutionState
	To      ExecutionState
	Message string
}

func (e *AttemptError) Error() string {
	return "illegal transition: " + string(e.From) + " -> " + string(e.To) + " (" + e.Message + ")"
}

// Attempt is an optional lightweight state holder for in-flight execution logic.
// It does not interact with the database; callers must persist state changes via Store.
type Attempt struct {
	State ExecutionState
}

// Transition validates and applies a state transition.
// Returns nil on success, or an AttemptError if the transition is illegal.
func (a *Attempt) Transition(to ExecutionState) error {
	if !to.Valid() {
		return &AttemptError{
			From:    a.State,
			To:      to,
			Message: "target state is invalid",
		}
	}
	if !CanTransition(a.State, to) {
		return &AttemptError{
			From:    a.State,
			To:      to,
			Message: "transition not allowed",
		}
	}
	a.State = to
	return nil
}

// Transition moves the execution to the given state through the state machine:
// the only way code outside this package changes Execution.State. It is a no-op
// when the execution is already there (a step that runs twice repeats it), and
// otherwise returns an error wrapping ErrIllegalTransition and leaves the state as
// it was. Persisting the change is UpdateExecution, which checks the table again
// against the stored row.
func (e *Execution) Transition(to ExecutionState) error {
	if ExecutionState(e.State) == to {
		return nil
	}
	a := Attempt{State: ExecutionState(e.State)}
	if err := a.Transition(to); err != nil {
		return fmt.Errorf("%w: %v", ErrIllegalTransition, err)
	}
	e.State = string(a.State)
	return nil
}
