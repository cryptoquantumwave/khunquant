package scenario

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/crash"
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

// shape is what a journal payload holds below one path: the keys of an object, each with
// a shape of its own. A nil shape declares nothing below it (a scalar, a tool's
// arguments, a bus event's own payload), so a path may go on from there unchecked.
type shape map[string]shape

// has says whether the dotted path can exist in a payload of this shape.
func (s shape) has(path string) bool {
	for _, seg := range strings.Split(path, ".") {
		if s == nil {
			return true
		}
		next, ok := s[seg]
		if !ok {
			return false
		}
		s = next
	}
	return true
}

func (s shape) keys() string { return strings.Join(slices.Sorted(maps.Keys(s)), ", ") }

// jsonShape is the top level of a struct type's JSON form.
func jsonShape(t reflect.Type) shape {
	s := shape{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.IsExported() && name != "-" {
			s[cmp.Or(name, f.Name)] = nil
		}
	}
	return s
}

// observerShape is the payload of every entry the bus observer writes (event.<kind>,
// workflow.step, proposal, policy.decision): {"recordable", "event": {...}}, or the
// envelope alone with payload_dropped.
var observerShape = shape{"recordable": nil, "payload_dropped": nil, "event": jsonShape(reflect.TypeFor[events.Event]())}

// payloadShapes are the payloads of the other journal kinds whose producer writes a fixed
// shape. A kind that is in neither declares no paths.
var payloadShapes = map[string]shape{
	// the gateway's tool tap (cmd/khunquant/internal/gateway/sandbox_lab_tap.go)
	"tool.call": {"name": nil, "origin": nil, "sender": nil, "turn_kind": nil, "is_error": nil, "args": nil, "args_truncated": nil, "args_error": nil},
	// the gateway's journalConfigReload
	"config.reload": {"ok": nil, "error": nil},
	// the fault injector's record (fault.KindFired)
	"fault.fired":    {"rule_id": nil, "effect": nil, "venue": nil, "method": nil, "path": nil, "order_op": nil, "symbol": nil, "turn": nil, "caller": nil},
	"exchange.req":   jsonShape(reflect.TypeFor[sandbox.ReqInfo]()),
	"exchange.order": jsonShape(reflect.TypeFor[sandbox.OrderEntry]()),
	// a funding payment of an open perpetual position (SBX-14b)
	"exchange.funding": jsonShape(reflect.TypeFor[sandbox.FundingEntry]()),
	// one request SimWeb served (simweb Store.record, a map literal)
	"web.req": {"method": nil, "url": nil, "host": nil, "path": nil, "query": nil, "status": nil, "rule_id": nil},
	// the live budget's event (providers/sim budgetEventLocked, a map literal)
	"llm.budget": {"event": nil, "limit_usd": nil, "limit_tok": nil, "spent_usd": nil, "spent_tok": nil, "calls": nil},
	// the crash a previous process died of: its marker, reported by the next process
	"crash": jsonShape(reflect.TypeFor[crash.Marker]()),
	// llm.call and llm.complete are left undeclared: their payload also carries the keys of
	// the rule's extra (providers/sim engine.go), so no closed path set exists for them.
}

// payloadShape is the payload a journal kind's entries have, when it is declared.
func payloadShape(kind string) (shape, bool) {
	switch {
	case strings.HasPrefix(kind, "event."), kind == "workflow.step", kind == "proposal", kind == "policy.decision":
		return observerShape, true
	}
	s, ok := payloadShapes[kind]
	return s, ok
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
