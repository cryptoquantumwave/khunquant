package scenario

import (
	"strings"
	"testing"
)

// The enums of an expectation are checked at load, from the sets the code defines, so a
// misspelling cannot make an absence check pass (TRUST-3). Each bad case has a control that
// is the same expectation spelt right and validates.
func TestValidate_ExpectationValuesAreChecked(t *testing.T) {
	cases := []struct {
		name, expect, path, want string // path "" = must validate
	}{
		{"to persona", "message: {to: owner, count: 0}", "", ""},
		{"to chat id", "message: {to: \"dm:owner\", count: 0}", "", ""},
		{"to list and glob", "message: {to: [stranger, \"group:*\"], count: 0}", "", ""},
		{"to misspelt", "message: {to: ownr, count: 0}", "/expect/0/message/to", "not a chat"},
		{"to bad group", "message: {to: \"group:nobody\", count: 0}", "/expect/0/message/to", "not a chat"},
		{"message severity", "message: {to: owner, severity: critical, count: 0}", "", ""},
		{"message severity misspelt", "message: {to: owner, severity: crit, count: 0}", "/expect/0/message/severity", "one of"},
		{"decision severity", "decision: {trigger: liquidation_distance, severity: critical, count: 0}", "", ""},
		{"decision severity misspelt", "decision: {severity: crit, count: 0}", "/expect/0/decision/severity", "one of"},
		{"decision trigger", "decision: {trigger: move_vs_vol, count: 0}", "", ""},
		{"decision trigger id", "decision: {trigger: \"price_cross:01\", count: 0}", "", ""},
		{"decision trigger misspelt", "decision: {trigger: liquidaton_distance, count: 0}", "/expect/0/decision/trigger", "not a trigger"},
		{"event trigger misspelt", "event: {kind: trigger.candidate, trigger: pricecross, count: 0}", "/expect/0/event/trigger", "not a trigger"},
		{"workflow kind", "workflow: {kind: dn.open, count: 0}", "", ""},
		{"workflow kind misspelt", "workflow: {kind: dn.opne, count: 0}", "/expect/0/workflow/kind", "one of"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := Validate(parseString(t, base+"expect:\n  - "+c.expect+"\n"), nil)
			if c.path == "" {
				if len(issues) != 0 {
					t.Fatalf("want no issues, got %v", issues)
				}
				return
			}
			if got := issueAt(issues, c.path); !strings.Contains(got, c.want) {
				t.Fatalf("issue at %s = %q, want %q (all: %v)", c.path, got, c.want, issues)
			}
		})
	}
}

// A scenario that configures its own simchat personas is not held to the default ones.
func TestValidate_CustomPersonasSkipTheToCheck(t *testing.T) {
	src := base + "instance:\n  config: {channels: {simchat: {personas: [{name: carol}]}}}\nexpect:\n  - message: {to: carol, count: 0}\n"
	if issues := Validate(parseString(t, src), nil); len(issues) != 0 {
		t.Fatalf("issues = %v", issues)
	}
}

// The sets come from the code: the workflow kinds are the packages' own definitions.
func TestValueSets_ComeFromTheCode(t *testing.T) {
	for _, k := range []string{"dn.open", "dn.recover", "dn.unwind", "dn.resize", "dca.execute", "approval.order"} {
		found := false
		for _, w := range workflowKinds {
			found = found || w == k
		}
		if !found {
			t.Errorf("workflowKinds = %v, lacks %s", workflowKinds, k)
		}
	}
	if len(severities) != 4 || severities[3] != "critical" {
		t.Errorf("severities = %v", severities)
	}
}

// An absence count on a path below an undeclared part of a payload (a tool's arguments, a
// bus event's own payload) is refused unless the file uses that path in a positive check
// (TRUST-3): args.symbl with count 0 would pass whatever the tool was called with.
func TestValidate_AbsenceBelowAnUndeclaredPayload(t *testing.T) {
	cases := []struct {
		name, expect string
		refused      bool
	}{
		{"args absence alone", "event: {kind: tool.call, fields: {name: create_order, args.symbl: BTC/USDT}, count: 0}", true},
		{"payload absence alone", "event: {kind: trigger.candidate, fields: {event.payload.DedupKey: margin_}, count: 0}", true},
		{"args absence with a positive use", "event: {kind: tool.call, fields: {name: create_order, args.symbol: BTC/USDT}, count: 0}\n  - event: {kind: tool.call, fields: {args.symbol: BTC/USDT}, count: \">= 1\"}", false},
		{"declared path", "event: {kind: tool.call, fields: {name: create_order}, count: 0}", false},
		{"positive alone", "event: {kind: tool.call, fields: {args.symbl: BTC/USDT}, count: \">= 1\"}", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := Validate(parseString(t, base+"expect:\n  - "+c.expect+"\n"), nil)
			refused := false
			for _, i := range issues {
				refused = refused || strings.Contains(i.Msg, "declares no keys")
			}
			if refused != c.refused {
				t.Fatalf("refused = %v, want %v (issues %v)", refused, c.refused, issues)
			}
		})
	}
}
