package scenario

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

// Helpers on journal kinds, shared by Validate and the runner.

// KindDeclared says whether the journal kind k is covered by the declared list. A
// declared "prefix.*" covers every "prefix.<x>"; a k that is itself "prefix.*" is
// covered when anything under the prefix is declared.
func KindDeclared(declared []string, k string) bool {
	pfx, isPat := strings.CutSuffix(k, ".*")
	for _, d := range declared {
		dp, dPat := strings.CutSuffix(d, ".*")
		switch {
		case d == k, dPat && strings.HasPrefix(k, dp+"."):
			return true
		case isPat && (strings.HasPrefix(d, pfx+".") || (dPat && strings.HasPrefix(pfx, dp+"."))):
			return true
		}
	}
	return false
}

// nonEventRoots are the first segments of journal kinds that are not event.<kind>.
var nonEventRoots = map[string]bool{
	"chat": true, "tool": true, "workflow": true, "proposal": true, "policy": true, "manage": true, "llm": true,
	"exchange": true, "clock": true, "settle": true, "config": true, "web": true, "fault": true, "crash": true, "snapshot": true,
}

// EventJournalKind maps the kind an `event` expectation names to a journal kind:
// "trigger.candidate" is event.trigger.candidate; "tool.call" and "chat.state" are kinds
// of their own.
func EventJournalKind(k string) string {
	root, _, _ := strings.Cut(k, ".")
	if strings.HasPrefix(k, "event.") || nonEventRoots[root] {
		return k
	}
	return "event." + k
}

// PayloadPaths maps the shorthand fields of an event or decision expectation to dotted
// paths into the journal payload of the two pipeline kinds whose payloads are structs
// without JSON tags (an observer entry is {"recordable", "event": {..., "payload": ...}}).
// "subject" names the candidate's subject there, not the journal subject.
var PayloadPaths = map[string]map[string]string{
	"event.trigger.candidate": {"trigger": "event.payload.TriggerID", "severity": "event.payload.Severity", "subject": "event.payload.Subject"},
	"event.attention.decision": {
		"trigger": "event.payload.Candidate.TriggerID", "severity": "event.payload.Candidate.Severity",
		"subject": "event.payload.Candidate.Subject", "disposition": "event.payload.Disposition", "reason": "event.payload.Reason",
	},
}

// OrderJournalKind is the kind an order expectation reads: exchange.order when it asks
// for anything beyond the venue (the request log has no symbol, side or order ids),
// else the request log.
func OrderJournalKind(fields map[string]any) string {
	for k := range fields {
		if k != "venue" {
			return "exchange.order"
		}
	}
	return "exchange.req"
}

// filedElsewhere are bus event kinds the observer journals under a kind of their own, so
// there is no event.<kind> entry for them.
var filedElsewhere = map[string]string{
	"approval.created": "proposal", "approval.decided": "proposal", "approval.expired": "proposal",
	"journal.step": "workflow.step", "journal.workflow": "workflow.step", "policy.decision": "policy.decision",
}

// EventKindIssue says what is wrong with an event.<kind> journal kind, or "" when it can
// match something: event.* covers every name, so a misspelt kind would otherwise match
// nothing forever and make an absence expectation pass. A journal kind that is not under
// event. (tool.call, chat.state) is not its business.
func EventKindIssue(journal string) string {
	name, ok := strings.CutPrefix(journal, "event.")
	if !ok || name == "*" {
		return ""
	}
	known := events.KnownKinds()
	journaled := func(match func(string) bool) bool {
		return slices.ContainsFunc(known, func(k events.Kind) bool { return filedElsewhere[string(k)] == "" && match(string(k)) })
	}
	if pfx, isPrefix := strings.CutSuffix(name, ".*"); isPrefix {
		if !journaled(func(k string) bool { return strings.HasPrefix(k, pfx+".") }) {
			return fmt.Sprintf("no event kind starts with %q", pfx+".")
		}
		return ""
	}
	if to := filedElsewhere[name]; to != "" {
		return fmt.Sprintf("%s is journaled as %s, not as event.%s", name, to, name)
	}
	if !journaled(func(k string) bool { return k == name }) {
		return fmt.Sprintf("%q is not an event kind (events.KnownKinds)", name)
	}
	return ""
}
