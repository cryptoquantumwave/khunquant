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
//
// declaresConfirm is whether the registered tool's schema declares a boolean
// "confirm" parameter. The approval gate waves through confirm=false calls only
// for such tools; see isPreviewCall.
func checkToolPolicy(ctx context.Context, name string, args map[string]any, declaresConfirm bool) *ToolResult {
	// The class ceiling comes first and answers one question for autonomous
	// turns: may this turn call this tool at all? The per-tool rows below then
	// apply to whatever the ceiling lets through, as they always did.
	if !turnMayCall(ctx, name) {
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

	policyMu.RLock()
	enabled, paper := policyEnabled, policyPaper
	policy, declared := policyTable[name]
	needsApproval := policy.RequiresApproval || policyApproval[name]
	policyMu.RUnlock()

	if !enabled || (!declared && !needsApproval) {
		return nil
	}

	origin := ToolOrigin(ctx)
	for _, blocked := range policy.BlockedOrigins {
		// An untagged origin fails closed against a blocked list. A turn that
		// lost its tag is a turn we cannot vouch for, and "unknown" must not be
		// the one value that slips past a financial gate.
		if origin == blocked || origin == "" {
			return policyRefusal(name, fmt.Sprintf(
				"%s is not available to %s turns. A scheduled job runs with nobody present to "+
					"answer for it, so financial actions must be started by a person.",
				name, describeOrigin(origin)))
		}
	}

	if policy.RefuseUnderPaperMode && paper {
		return policyRefusal(name, fmt.Sprintf(
			"%s is blocked while paper_trading_mode is on. This tool has no simulated mode, so "+
				"running it would place real orders despite paper mode. Disable paper_trading_mode "+
				"to use it.", name))
	}

	// Approval runs last: a call that would be refused anyway must not cost the
	// operator an approval they then cannot use.
	if needsApproval {
		// An approval request is a message to the operator's chat. A proactive
		// turn delivers nothing, and nobody is waiting on it to answer one.
		if origin == OriginProactive {
			return policyRefusal(name, fmt.Sprintf(
				"%s requires operator approval, which a proactive turn cannot ask for.", name))
		}
		return checkApproval(ctx, name, args, declaresConfirm)
	}

	return nil
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
