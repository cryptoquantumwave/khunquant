package scenario

import "testing"

func TestKindDeclared(t *testing.T) {
	d := []string{"chat.out", "event.*"}
	for k, want := range map[string]bool{
		"chat.out": true, "chat.in": false, "event.trigger.candidate": true, "event.*": true, "chat.*": true, "tool.call": false, "tool.*": false,
	} {
		if got := KindDeclared(d, k); got != want {
			t.Errorf("KindDeclared(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestEventKindIssue(t *testing.T) {
	for kind, wantIssue := range map[string]bool{
		"event.trigger.candidate": false, "event.attention.decision": false, "event.trigger.*": false, "event.*": false,
		"tool.call": false, "chat.state": false, // not under event.: not its business
		"event.trigger.candiate": true, "event.nothing.*": true, "event.approval.created": true, "event.approval.*": true,
		"event.journal.step": true, "event.": true,
	} {
		if got := EventKindIssue(kind) != ""; got != wantIssue {
			t.Errorf("EventKindIssue(%q) = %q, issue wanted: %v", kind, EventKindIssue(kind), wantIssue)
		}
	}
}
