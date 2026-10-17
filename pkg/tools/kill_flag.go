package tools

import (
	"context"
	"sync/atomic"
)

// killFlagHolder is the kill switch as the tools that can act on their own read it.
// The tools are registered with the agent loop before the gateway builds the control
// path (the flag lives in the shared proactive database), so the flag is late-bound
// exactly like the order proposer: the gateway arms it on every build and reload with
// the SAME control.State instance the approval service and the delta-neutral engine
// get, and nil (proactive off, or the state could not be opened) means never killed,
// as it does everywhere else.
var killFlagHolder atomic.Pointer[func() bool]

// SetKillFlag arms the kill flag; nil disarms it.
func SetKillFlag(killed func() bool) {
	if killed == nil {
		killFlagHolder.Store(nil)
		return
	}
	killFlagHolder.Store(&killed)
}

// killEngaged reports whether the kill switch is engaged. control.State.Killed fails
// closed, so an unreadable flag answers true.
func killEngaged() bool {
	if f := killFlagHolder.Load(); f != nil {
		return (*f)()
	}
	return false
}

// killRefusal is execute_dca_order's check of the kill switch. The switch stops the
// assistant acting on its own: a turn the assistant started itself (a scheduled "dca:"
// job above all, which no mandate or approval covers), by the same predicate the
// class ceiling uses for "autonomous". A turn a person started is not affected: the
// kill switch does not take the owner's hands off.
func killRefusal(ctx context.Context) *ToolResult {
	if !IsAutonomousOrigin(ToolOrigin(ctx)) || !killEngaged() {
		return nil
	}
	return policyRefusal(NameExecuteDCAOrder,
		NameExecuteDCAOrder+" is refused: the kill switch is engaged (/stop), so scheduled and automatic purchases are stopped. "+
			"Nothing was bought. Send /resume to allow them again; you can still ask for a purchase yourself.")
}
