// Package policy answers one question for every tool call: may this action
// happen, and how - act, ask, notify only, or deny?
//
// Today the answer is spread over the class ceiling, the proactive ceiling,
// propose_order's opening, the kill flag and each executor's own gates. The
// Engine gathers them behind Authorize. In this release it runs in SHADOW beside
// the existing enforcement: pkg/tools builds an ActionRequest for the call, the
// Recorder records what the engine would have decided, and the call proceeds
// exactly as the existing checks decide. The engine is stricter than today or
// equal to it, never looser: where it cannot tell, it asks or denies.
//
// The package imports no tool, agent or approval code, so pkg/tools (which calls
// it) and pkg/approval (which will) can both import it. A request therefore
// carries plain values; pkg/tools fills them in.
package policy

import "github.com/cryptoquantumwave/khunquant/pkg/proactive"

// Tier is how much an action can cost, ordered by risk.
type Tier int

// The tiers. T1 only reduces exposure, T2 is covered by a mandate the owner
// signed, T3 is a discretionary trade (or any other consequential action) and T4
// moves funds.
const (
	T0Read Tier = iota
	T1Protective
	T2Mandated
	T3Discretionary
	T4Funds
)

var tierNames = [...]string{"T0", "T1", "T2", "T3", "T4"}

func (t Tier) String() string {
	if t >= 0 && int(t) < len(tierNames) {
		return tierNames[t]
	}
	return "T?"
}

// Mode is what may happen to an action.
type Mode string

// The modes, from least to most restrictive.
const (
	// ModeAct lets the action run.
	ModeAct Mode = "act"
	// ModeAsk turns the action into a question for the owner (a code-built card).
	ModeAsk Mode = "ask"
	// ModeNotifyOnly tells the owner what was wanted and does nothing.
	ModeNotifyOnly Mode = "notify_only"
	// ModeDeny refuses the action.
	ModeDeny Mode = "deny"
)

// Rank orders modes by restriction: act 0, ask 1, notify_only 2, deny 3. An
// unknown mode ranks as deny, so a corrupted value is never the permissive one.
func (m Mode) Rank() int {
	switch m {
	case ModeAct:
		return 0
	case ModeAsk:
		return 1
	case ModeNotifyOnly:
		return 2
	}
	return 3
}

// Class is what a tool can do, as pkg/tools classifies it (Class.String()). It is
// the same vocabulary as plain text so that this package does not import tools.
type Class string

// The classes.
const (
	ClassUnknown    Class = "unknown"
	ClassRead       Class = "read"
	ClassNotify     Class = "notify"
	ClassLocalState Class = "local_state"
	ClassAutomation Class = "automation"
	ClassPropose    Class = "propose"
	ClassTrade      Class = "trade"
	ClassFunds      Class = "funds"
	ClassSystem     Class = "system"
)

// movesMoney is the set of classes the provenance gate, the kill switch and the
// hard limits apply to.
func (c Class) movesMoney() bool {
	return c == ClassTrade || c == ClassFunds || c == ClassPropose
}

// Actor says who, if anyone, is behind the turn.
type Actor int

// The actors. The zero value is nobody, so a request that does not say is treated
// as an autonomous one.
const (
	// ActorNone is a turn the assistant started by itself (or one whose origin is
	// unknown).
	ActorNone Actor = iota
	// ActorOwner is the owner's own root conversation turn.
	ActorOwner
	// ActorOther is a person who is not the owner, or one the owner check cannot
	// establish.
	ActorOther
)

// Order is what an order-shaped action asks for. All values come from the tool's
// arguments or are filled in by code; none is model prose.
type Order struct {
	Symbol string
	Side   string
	Amount float64
	Price  *float64
	// USD is the order's value in USD when the request already knows it (a
	// proposal sized in USD); USDKnown says so. Otherwise the engine asks its
	// Valuer, and an order it cannot value is refused while a USD cap is set.
	USD      float64
	USDKnown bool
	// Placing is true when the call goes on to place the order (confirm set), not
	// when it only previews it. Only placing orders count towards the rate and
	// duplicate brakes.
	Placing bool
}

// ActionRequest is the question put to the engine.
type ActionRequest struct {
	Tool  string
	Class Class
	// TurnKind is what started the turn; empty when it cannot be told.
	TurnKind proactive.TurnKind
	Actor    Actor
	// Trust is the lowest trust of everything that influenced the turn.
	Trust proactive.Trust

	Provider string
	Account  string
	Order    *Order

	// ReduceOnly is an order that can only shrink exposure; Protective is an exit
	// tool (close, reduce, cancel, flatten, unwind). Both are exempt from every
	// brake and from the kill switch.
	ReduceOnly bool
	Protective bool
	// Futures marks a futures mutation, which the leverage switch gates.
	Futures bool
	// IsProposal marks a tool whose call IS the question to the owner
	// (propose_order): an "ask" for it is the call itself.
	IsProposal bool
	// PlanID names the DCA plan of an execute_dca_order call, for the mandate lookup.
	PlanID int64
}

// Verdict is the engine's answer.
type Verdict struct {
	Mode Mode
	Tier Tier
	// Reasons say, in short stable text, which gate decided and why. Never arguments.
	Reasons []string
}
