package tools

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// ToolPolicy declares the guarantees a tool must satisfy before it runs.
//
// C-2: safety checks live inside each tool today, which means a tool added
// without them is unguarded and nothing says so. CheckLeverage's doc comment -
// "every live futures mutation tool must call this before executing, passing the
// direction of the action" - is that convention written down, and a convention
// is exactly what a new tool forgets. A policy declared here applies whether or
// not the tool remembers.
//
// The in-tool risk-adding gates (CheckLeverage, CheckDailyLossFor) apply to
// broker.RiskIncrease actions only; exits pass broker.RiskReduce and skip them,
// so a safety brake never stops the user getting out of a position.
type ToolPolicy struct {
	// BlockedOrigins refuses the call when the turn's origin matches. A
	// scheduled job has no human present to answer for an order, so financial
	// tools list OriginCron here.
	BlockedOrigins []string

	// RefuseUnderPaperMode marks a tool that moves real money and has no paper
	// implementation of its own.
	//
	// This is a backstop, not a redirect. create_order and execute_dca_order
	// already honour paper mode and produce their own simulated output, so they
	// are deliberately NOT listed: routing them through here would either lose
	// their diagnostics or require this file to know each tool's paper
	// representation. What it catches is the tool that checks nothing - which
	// would otherwise place live orders under paper mode in silence. Options
	// have no simulation, so option_create_order is listed.
	RefuseUnderPaperMode bool

	// RequiresApproval holds the call until the operator replies /approve with
	// a one-time code (see approval.go). No shipped entry sets it: approval is
	// opt-in, per tool, via tools.policies.<tool>.require_approval.
	RequiresApproval bool
}

// defaultToolPolicies is the policy table.
//
// Keys are the Name* constants from names.go rather than string literals, so a
// typo is a compile error instead of a policy that silently never applies - the
// failure mode that let the option tools ship unconfigurable.
//
// Coverage is validated against the tool catalog by TestPolicyKeysAreRealTools.
func defaultToolPolicies() map[string]ToolPolicy {
	cronBlocked := []string{OriginCron}

	return map[string]ToolPolicy{
		// Spot order placement and fund movement.
		NameCreateOrder:   {BlockedOrigins: cronBlocked},
		NameTransferFunds: {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},

		// propose_order moves no money itself: it stores an order and sends the
		// owner a card, and the order is placed only by the approval.order workflow
		// after the owner approves exactly it. It is a money-moving path all the
		// same (an approved proposal becomes a real order), so it is listed: cron
		// is blocked, and paper mode is handled by the proposal itself (spot is
		// simulated, a futures order is refused unless it only reduces a position),
		// so RefuseUnderPaperMode is deliberately not set.
		NameProposeOrder: {BlockedOrigins: cronBlocked},

		// Options are not simulated: the tool refuses in paper mode itself, and the flag is the registry backstop.
		NameOptionCreateOrder: {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},

		// Futures. None of these check paper mode, and all place or modify live
		// orders or leverage.
		NameFuturesSetLeverage:      {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},
		NameFuturesOpenPosition:     {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},
		NameFuturesModifyProtection: {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},

		// Delta-neutral execution legs.
		NameOpenDeltaNeutralPosition:   {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},
		NameResizeDeltaNeutralPosition: {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},

		// Earn moves real funds via Subscribe/Redeem. It checks trade
		// permission itself but has no paper mode.
		NameManageEarnPosition: {BlockedOrigins: cronBlocked, RefuseUnderPaperMode: true},

		// Plan writes set the amounts a later execution spends. They place no
		// orders themselves, so paper mode is irrelevant; the concern is a
		// scheduled job rewriting its own budget.
		NameCreateDCAPlan:          {BlockedOrigins: cronBlocked},
		NameUpdateDCAPlan:          {BlockedOrigins: cronBlocked},
		NameUpdateDeltaNeutralPlan: {BlockedOrigins: cronBlocked},

		// Skill installation fetches and runs third-party code.
		NameInstallSkill: {BlockedOrigins: cronBlocked},
	}
}

// Deliberately absent from the table, and why:
//
//   - execute_dca_order runs on a schedule by design - blocking cron would
//     disable DCA entirely. It already honours paper mode and the risk gates.
//   - futures_close_position, futures_reduce_position, futures_cancel_orders,
//     futures_emergency_flatten, cancel_order, option_cancel_order,
//     emergency_stop and unwind_delta_neutral_position all reduce exposure. Refusing them is the
//     dangerous direction: an operator cutting losses must never be blocked.
//   - every get_*/list_* tool is read-only.

// riskReducingTools reduce exposure. They are absent from the table and must
// never be gated - an operator cutting losses cannot be told to wait - so an
// operator's require_approval on one of them is ignored rather than honoured.
var riskReducingTools = map[string]bool{
	NameFuturesClosePosition:       true,
	NameFuturesReducePosition:      true,
	NameFuturesCancelOrders:        true,
	NameFuturesEmergencyFlatten:    true,
	NameCancelOrder:                true,
	NameOptionCancelOrder:          true,
	NameEmergencyStop:              true,
	NameUnwindDeltaNeutralPosition: true,
}

var (
	policyMu      sync.RWMutex
	policyTable   = defaultToolPolicies()
	policyPaper   bool
	policyEnabled bool

	// policyApproval is the operator's opt-in set from
	// tools.policies.<tool>.require_approval, kept apart from the table so the
	// code defaults and the operator's additions cannot overwrite each other.
	policyApproval = map[string]bool{}
)

// SetPolicyConfig wires the policy check to the running configuration.
//
// ExecuteWithContext is a method on ToolRegistry and has no *config.Config, so
// the paper-mode flag has to be handed in. This mirrors how
// broker.GlobalLossTracker.SetStorePath is wired from NewAgentLoop, which every
// mode that builds trading tools calls - the same completeness argument applies
// here.
//
// Until this is called the policy check is inert. That is deliberate: a package
// default that silently enforced would change behaviour in tests and in
// embedders that never opted in, and enforcement that appears without wiring is
// as surprising as enforcement that vanishes.
func SetPolicyConfig(paperTradingMode bool) {
	policyMu.Lock()
	defer policyMu.Unlock()
	policyPaper = paperTradingMode
	policyEnabled = true
}

// SetRequiredApprovals sets the tools whose calls need operator approval,
// replacing any previous set. It returns the names it ignored because they
// reduce exposure, sorted, so the caller can warn about them.
func SetRequiredApprovals(names []string) (ignored []string) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if riskReducingTools[n] {
			ignored = append(ignored, n)
			continue
		}
		set[n] = true
	}
	sort.Strings(ignored)

	policyMu.Lock()
	defer policyMu.Unlock()
	policyApproval = set
	return ignored
}

// SetToolPolicies replaces the table. Intended for operator overrides and for
// tests, which must restore the previous value.
func SetToolPolicies(table map[string]ToolPolicy) map[string]ToolPolicy {
	policyMu.Lock()
	defer policyMu.Unlock()
	prev := policyTable
	policyTable = table
	return prev
}

// PolicyFor returns the policy for a tool and whether one is declared.
func PolicyFor(name string) (ToolPolicy, bool) {
	policyMu.RLock()
	defer policyMu.RUnlock()
	p, ok := policyTable[name]
	return p, ok
}

// checkToolPolicy returns a refusal result, or nil to allow the call.
//
// The refusal sets ForUser as well as ForLLM: a block the operator never sees
// is indistinguishable from the tool quietly doing nothing.
func checkToolPolicy(ctx context.Context, name string, args map[string]any) *ToolResult {
	refusal := enforceToolPolicy(ctx, name, args)
	// Shadow: the policy engine is asked about the call and its verdict recorded
	// beside what the checks above decided. It cannot change the outcome. In live,
	// a money call the checks allowed is decided and recorded by the hook instead
	// (policyHook), so that it is neither asked twice nor counted twice.
	if refusal != nil || livePolicy.Load() == nil || !applies(name) {
		observeShadow(ctx, name, args, refusal == nil)
	}
	return refusal
}

// enforceToolPolicy is the enforcement: the class ceiling, the per-tool rows and
// the approval hold.
func enforceToolPolicy(ctx context.Context, name string, args map[string]any) *ToolResult {
	// The class ceiling comes first and answers one question for autonomous
	// turns: may this turn call this tool at all? The per-tool rows below then
	// apply to whatever the ceiling lets through, as they always did.
	if !turnMayCall(ctx, name) {
		if name == NameProposeOrder {
			return policyRefusal(name, "propose_order is not available in this turn. It is open only to the owner's own "+
				"conversation turn and to a proactive turn whose inputs were all exchange data (a proactive turn that "+
				"has read the web, a file or a skill cannot propose); approvals must also be switched on.")
		}
		if name == NameRemember {
			return policyRefusal(name, fmt.Sprintf("tool %q is not permitted in an autonomous (%s) turn: memories cannot be "+
				"suggested without an owner (proactive.owner) to confirm them in the daily digest, and the digest and "+
				"approvals must be running.", name, describeOrigin(ToolOrigin(ctx))))
		}
		if ToolOrigin(ctx) == OriginProactive {
			return policyRefusal(name, fmt.Sprintf(
				"tool %q is not permitted in an autonomous (%s) turn. A proactive turn can read data and "+
					"declare its decision with %s; it cannot message, write, trade or change anything.",
				name, OriginProactive, NameRespondProactive))
		}
		return policyRefusal(name, fmt.Sprintf(
			"tool %q is not permitted in an autonomous (%s) turn. Turns the assistant starts by itself can "+
				"read, notify and write workspace files only; trading, funds, scheduling, system and "+
				"MCP tools need a person to start the turn.",
			name, describeOrigin(ToolOrigin(ctx))))
	}

	switch kind, msg := rowGate(ctx, name); kind {
	case rowBlocked:
		return policyRefusal(name, msg)
	case rowHold:
		// Approval runs last: a call that would be refused anyway must not cost the
		// operator an approval they then cannot use.
		return checkApproval(ctx, name, args)
	}
	return nil
}

type rowKind int

const (
	rowPass rowKind = iota
	rowBlocked
	rowHold
)

// rowGate is the tool's own policy row, without side effects: whether a blocked
// origin, paper mode or a required approval stands in the call's way. It is the
// whole of the row logic - enforceToolPolicy and the policy engine both ask it - so
// the two cannot disagree. For rowBlocked msg is the refusal text.
func rowGate(ctx context.Context, name string) (kind rowKind, msg string) {
	policyMu.RLock()
	enabled, paper := policyEnabled, policyPaper
	policy, declared := policyTable[name]
	needsApproval := policy.RequiresApproval || policyApproval[name]
	policyMu.RUnlock()

	if !enabled || (!declared && !needsApproval) {
		return rowPass, ""
	}

	origin := ToolOrigin(ctx)
	for _, blocked := range policy.BlockedOrigins {
		// An untagged origin fails closed against a blocked list. A turn that
		// lost its tag is a turn we cannot vouch for, and "unknown" must not be
		// the one value that slips past a financial gate.
		if origin == blocked || origin == "" {
			return rowBlocked, fmt.Sprintf(
				"%s is not available to %s turns. A scheduled job runs with nobody present to "+
					"answer for it, so financial actions must be started by a person.",
				name, describeOrigin(origin))
		}
	}

	if policy.RefuseUnderPaperMode && paper {
		return rowBlocked, fmt.Sprintf(
			"%s is blocked while paper_trading_mode is on. This tool has no simulated mode, so "+
				"running it would place real orders despite paper mode. Disable paper_trading_mode "+
				"to use it.", name)
	}

	if needsApproval {
		// An approval request is a message to the operator's chat. A proactive
		// turn delivers nothing, and nobody is waiting on it to answer one.
		if origin == OriginProactive {
			return rowBlocked, fmt.Sprintf(
				"%s requires operator approval, which a proactive turn cannot ask for.", name)
		}
		return rowHold, ""
	}
	return rowPass, ""
}

// turnMayCall is the one predicate for "may the turn carried by ctx call this
// tool". checkToolPolicy refuses on it and ToolRegistry.ToProviderDefsFor hides
// definitions with it, so what the model is shown and what it can run cannot
// drift apart.
//
// A turn that is not autonomous may call anything here; its limits are the
// per-tool rows, paper mode and approvals. An autonomous turn may call the
// read, notify and local-state classes, plus two exceptions that exist because
// their jobs need them and that are bound to the job:
//
//   - a heartbeat may spawn (the child inherits the origin, so the same ceiling);
//   - a "dca:" scheduled job may call execute_dca_order (the tool itself checks
//     the plan ID against the job's).
//
// An untagged turn is not treated as autonomous: every production turn is tagged
// where it starts, and the per-tool rows already fail closed on an empty origin.
// Like the rows, the ceiling does nothing until SetPolicyConfig has armed it.
func turnMayCall(ctx context.Context, name string) bool {
	// propose_order is granted by name, to the owner's own root turn and to a
	// trusted candidate-driven proactive turn, and to nothing else (see
	// proposeOrderOpen). It is decided before the origin shortcuts below, which
	// would give a proactive turn the read class only and a person's turn everything.
	if name == NameProposeOrder {
		return proposeOrderOpen(ctx)
	}
	if name == NameRemember {
		return rememberOpen(ctx)
	}
	origin := ToolOrigin(ctx)

	// A proactive turn is decided before the shortcuts below, which would give
	// it notify and local-state tools, and before the armed check: the origin
	// exists only inside agent.RunProactiveTurn, so no embedder that never armed
	// the policy can be surprised, and a ceiling that depends on a setup call
	// someone may have skipped is not one to trust for a turn nobody watches.
	if origin == OriginProactive {
		return proactiveTurnMayCall(ctx, name)
	}

	if !IsAutonomousOrigin(origin) {
		return true
	}

	policyMu.RLock()
	enabled := policyEnabled
	policyMu.RUnlock()
	if !enabled {
		return true
	}

	switch ClassOf(name) {
	case ClassRead, ClassNotify, ClassLocalState:
		return true
	}

	switch origin {
	case OriginHeartbeat:
		return name == NameSpawn
	case OriginCron:
		return name == NameExecuteDCAOrder && isDCAJobScope(ctx)
	}
	return false
}

// TurnMayCall reports whether the turn ctx describes may call the named tool:
// the predicate ExecuteWithContext enforces and ToProviderDefsFor shows
// definitions by. It lets a caller that narrows a turn's registry ask what the
// ceiling would have allowed.
func TurnMayCall(ctx context.Context, name string) bool {
	return turnMayCall(ctx, name)
}

// proactiveTurnMayCall is the ceiling of a proactive turn: every read-class tool
// (bar web_fetch and the two discovery tools, below) and respond_proactive,
// nothing else. No notify tool (the runtime delivers, the
// turn only decides), no local-state tool (it writes no file and no memory), no
// spawn, no trade, funds, automation or system tool, and no MCP or otherwise
// unclassified tool.
//
// The legacy heartbeat kind additionally keeps take_snapshot, because the
// heartbeat checklist records a snapshot, and web_fetch, which the legacy
// heartbeat can call today and whose prompt is the owner's own checklist. Any
// other kind, or none, gets the stricter set.
//
// web_fetch takes a model-chosen URL, and a proactive turn's candidate text can
// come from outside the process: with it, whatever the read tools returned
// could be sent to an address the text chose. web_search stays. A new read-class
// tool that sends model-chosen data to a third party belongs in the exclusions
// here.
func proactiveTurnMayCall(ctx context.Context, name string) bool {
	if name == NameRespondProactive {
		return true
	}
	// The discovery tools are built over the agent's shared registry, not the
	// turn's clone, and promote tools on it. They would also only promote MCP
	// tools, which this ceiling denies. A read-only turn must not change shared
	// state, so they are out although they are read-class.
	if name == NameToolSearchRegex || name == NameToolSearchBM25 {
		return false
	}
	if name == NameWebFetch {
		return ProactiveTurnKind(ctx) == proactive.TurnHeartbeatLegacy
	}
	if name == NameTakeSnapshot && ProactiveTurnKind(ctx) == proactive.TurnHeartbeatLegacy {
		return true
	}
	return ClassOf(name) == ClassRead
}

func describeOrigin(origin string) string {
	if origin == "" {
		return "untagged"
	}
	return origin
}

func policyRefusal(name, message string) *ToolResult {
	return &ToolResult{
		ForLLM:  message,
		ForUser: message,
		IsError: true,
	}
}
