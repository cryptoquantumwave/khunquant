package scenario

import (
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// The sets an expectation's values are checked against at load time. A value the journal
// never holds matches nothing, so under count: 0 a misspelling (to: ownr, severity: crit)
// would pass whatever happened. Each set is read from the code that produces the value.

// severities are attention's severity names, as the journal writes them.
var severities = func() []string {
	var out []string
	for s := proactive.SeverityInfo; s <= proactive.SeverityCritical; s++ {
		out = append(out, s.String())
	}
	return out
}()

// triggerNames are the names a TriggerID starts with: each trigger kind ("price_cross:<id>"),
// the memory and reflection digest items ("memory:<code>"), and the two ids the pipeline
// writes without a registry kind ("dn:<plan>", "heartbeat"). A trigger value is matched as
// a substring of the id, so it is checked as one: it must be part of a known name, or
// contain one.
var triggerNames = []string{
	triggers.KindPriceCross, triggers.KindIndicatorCross, triggers.KindDCADue, triggers.KindDNHealth,
	triggers.KindExternal, triggers.KindGoalDiscrepancy, triggers.KindNewsRelevant, triggers.KindMoveVsVol,
	triggers.KindDrawdown, triggers.KindAllocationDrift, triggers.KindLiquidationDistance, triggers.KindDataStale,
	triggers.KindTimeAt,
	strings.TrimSuffix(proactive.MemoryTriggerPrefix, ":"), strings.TrimSuffix(proactive.ReflectionTriggerPrefix, ":"),
	"dn", "heartbeat",
}

// workflowKinds are the journal workflow kinds the gateway runs (each package's own
// definitions, so a new one needs no edit here).
var workflowKinds = func() []string {
	var out []string
	for _, d := range (&deltaneutral.Deps{}).Definitions() {
		out = append(out, d.Kind)
	}
	for _, d := range (&dca.Deps{}).Definitions() {
		out = append(out, d.Kind)
	}
	for _, d := range (&approval.Service{}).Definitions() {
		out = append(out, d.Kind)
	}
	return out
}()

// triggerKnown says whether a trigger value (a leading "!" negates) can match a TriggerID.
func triggerKnown(v string) bool {
	v = strings.TrimPrefix(v, "!")
	return v != "" && slices.ContainsFunc(triggerNames, func(n string) bool {
		return strings.Contains(n, v) || strings.Contains(v, n)
	})
}

// chatTargets are the chat ids a default simchat world has; a message expectation's to is a
// persona, a chat id (dm:<persona>, group:<name>) or a glob over them.
func chatTargets() []string {
	var out []string
	for _, p := range simchat.DefaultPersonas() {
		out = append(out, "dm:"+p.Name)
	}
	for g := range simchat.DefaultGroups() {
		out = append(out, "group:"+g)
	}
	return out
}

// chatTargetKnown says whether the pattern (as chatMatch reads it) matches a default chat id.
func chatTargetKnown(pattern string) bool {
	return slices.ContainsFunc(chatTargets(), func(c string) bool { return chatMatch(pattern, c) })
}
