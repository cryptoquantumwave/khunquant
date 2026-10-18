package tools

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/policy"
)

// livePolicy is the policy engine's enforcement, armed by the gateway on every
// build and reload and only while proactive is effectively on and
// proactive.policy.mode is live (with a valid owner, and brakes that validate). Nil
// (the default, shadow and off) means the hook below is not in any registry's chain.
//
// It is package state, not a hook added to each registry, so that every registry
// that can run a tool - the agent's own, a subagent's clone, one rebuilt on a reload
// - is covered, and a reload swaps it in one place.
var livePolicy atomic.Pointer[policyHook]

// SetPolicyLive arms the engine's enforcement; nil disarms it. It does not arm the
// recorder (SetPolicyShadow does), which the hook uses for its rows.
func SetPolicyLive(e *policy.Engine) {
	if e == nil {
		livePolicy.Store(nil)
		return
	}
	livePolicy.Store(&policyHook{engine: e})
}

// policyHook is the live enforcement as a Hook. It runs after today's checks, so it
// can only add refusals: ask, notify_only and deny all refuse. It never creates a
// proposal, a card or an order - an "ask" is an error result telling the model to use
// propose_order, which is the only way a proposal is made.
type policyHook struct{ engine *policy.Engine }

// applies is whether the engine is asked about a call: the classes that move money.
// Every other class is already decided by gate L and the rows, which today's checks
// have just applied, and the kill switch and the money gates are about money; the
// dial would only mis-handle a tool it cannot classify (every MCP tool).
func applies(name string) bool {
	if name == NameRemember {
		return false // a suggestion for the owner's digest: not a money action, and a tainted turn may make it
	}
	switch ClassOf(name) {
	case ClassTrade, ClassFunds, ClassPropose:
		return true
	}
	return false
}

func (h *policyHook) verdictFor(ctx context.Context, name string, args map[string]any) (policy.ActionRequest, policy.Verdict) {
	req := buildActionRequest(ctx, name, args)
	return req, h.engine.Enforce(ctx, req)
}

func (h *policyHook) BeforeTool(ctx context.Context, name string, args map[string]any) (bool, *ToolResult) {
	if !applies(name) {
		return true, nil
	}
	req, v := h.verdictFor(ctx, name, args)
	shadowRecorder.Load().Record(ctx, req, v)
	if v.Mode == policy.ModeAct {
		return true, nil
	}
	return false, verdictRefusal(name, v)
}

// AfterTool does nothing: the registry's NoteToolCalls is the one place a turn's
// taint is lowered, and the decision was recorded before the tool ran.
func (h *policyHook) AfterTool(context.Context, string, map[string]any, *ToolResult) {}

// verdictRefusal is the result the model sees for a verdict that is not act. The
// reasons name the gate and the limit, never an argument.
func verdictRefusal(name string, v policy.Verdict) *ToolResult {
	switch v.Mode {
	case policy.ModeAsk:
		return policyRefusal(name, "Policy: this action needs the owner's approval and cannot be done directly from this turn. "+
			"Nothing was done. To put it to the owner, call "+NameProposeOrder+"; the owner, and only the owner, approves exactly that order.")
	case policy.ModeNotifyOnly:
		return policyRefusal(name, "Policy: this turn may only inform the owner about this; it may not act on it. Nothing was done.")
	}
	why := strings.Join(v.Reasons, "; ")
	if why == "" {
		why = "no reason recorded"
	}
	return policyRefusal(name, "Policy refused this call: "+why+". Nothing was done.")
}

// PolicyProbeLive is PolicyProbe for the live mode: it runs what ExecuteWithContext
// runs for the policy - today's checks, then the live hook - without running the tool
// or recording anything, and reports each step. liveAllowed is true only when both
// let the call through. It COUNTS an allowed order in e, as a real call does, so a
// test that asks twice with one engine sees the second counted; build an engine per
// probe for an independent answer.
func PolicyProbeLive(ctx context.Context, e *policy.Engine, name string, args map[string]any) (legacyAllowed, liveAllowed bool, v policy.Verdict) {
	if enforceToolPolicy(ctx, name, args) != nil {
		return false, false, policy.Verdict{Mode: policy.ModeDeny, Reasons: []string{"today's checks refuse"}}
	}
	if !applies(name) {
		return true, true, policy.Verdict{Mode: policy.ModeAct, Reasons: []string{"not a money action"}}
	}
	_, v = (&policyHook{engine: e}).verdictFor(ctx, name, args)
	return true, v.Mode == policy.ModeAct, v
}
