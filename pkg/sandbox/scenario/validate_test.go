package scenario

import (
	"strings"
	"testing"
)

// withExpect gives a scenario text the one expectation Validate requires.
func withExpect(src string) string {
	if strings.Contains(src, "expect:") {
		return src
	}
	return src + "expect:\n  - logs: {}\n"
}

// issueAt returns the message of the issue at path, or "".
func issueAt(issues []Issue, path string) string {
	for _, i := range issues {
		if i.Path == path {
			return i.Msg
		}
	}
	return ""
}

func TestStep_OneKindKeyPlusAt(t *testing.T) {
	cases := []struct {
		name, steps, path, want string
	}{
		{"at plus kind", "timeline:\n  - {at: +5m, chat.channel: {state: down}}\n", "", ""},
		{"two kind keys", "timeline:\n  - {chat.channel: {state: down}, advance: 5m}\n", "/timeline/0", "one kind key"},
		{"no kind key", "timeline:\n  - {at: +5m}\n", "/timeline/0", "one kind key"},
		{"unknown kind", "timeline:\n  - {chat.chanel: {state: down}}\n", "/timeline/0/chat.chanel", "unknown step kind"},
		{"offsets decrease", "timeline:\n  - {at: +30m, advance: 1m}\n  - {at: +10m, advance: 1m}\n", "/timeline/1/at", "must not decrease"},
		{"bad offset", "timeline:\n  - {at: soon, advance: 1m}\n", "/timeline/0/at", "offset"},
		{"at in setup", "setup:\n  - {at: +1m, advance: 1m}\n", "/setup/0/at", "no at"},
		{"missing arg", "timeline:\n  - {market.path: {symbol: BTC/USDT, to: 1}}\n", "/timeline/0/market.path/over", "required"},
		{"not a mapping", "timeline:\n  - advance\n", "/timeline/0", "mapping"},
		{"workspace dotdot", "instance: {workspace: {\"../x\": a}}\n", "/instance/workspace/..~1x", ".."},
		{"workspace absolute", "instance: {workspace: {\"/etc/x\": a}}\n", "/instance/workspace/~1etc~1x", "relative"},
		{"workspace ok", "instance: {workspace: {\"memory/a.md\": a}}\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := Validate(parseString(t, withExpect(base+c.steps)), nil)
			if c.path == "" {
				if len(issues) != 0 {
					t.Fatalf("want no issues, got %v", issues)
				}
				return
			}
			if got := issueAt(issues, c.path); !strings.Contains(got, c.want) {
				t.Fatalf("issue at %s = %q, want it to contain %q (all: %v)", c.path, got, c.want, issues)
			}
		})
	}
}

func TestValidate_UnknownKeysHaveJSONPointers(t *testing.T) {
	f := parseString(t, "version: 1\nscenario: t\nscenerio: typo\nclock: {start: \"2026-10-20T09:00:00+07:00\", mod: manual}\n")
	issues := Validate(f, nil)
	if issueAt(issues, "/scenerio") == "" {
		t.Errorf("a top-level typo needs a pointer, got %v", issues)
	}
	if issueAt(issues, "/clock/mod") == "" {
		t.Errorf("a nested typo needs a pointer, got %v", issues)
	}
}

func TestValidate_KnownBugNeedsID(t *testing.T) {
	if got := issueAt(Validate(parseString(t, base+"status: known_bug\n"), nil), "/known_bug"); got == "" {
		t.Error("status: known_bug without known_bug must be invalid")
	}
	if got := issueAt(Validate(parseString(t, base+"known_bug: H8\n"), nil), "/known_bug"); got == "" {
		t.Error("known_bug set while active must be invalid")
	}
	if issues := Validate(parseString(t, withExpect(base+"status: known_bug\nknown_bug: H8\n")), nil); len(issues) != 0 {
		t.Errorf("a documented known bug is valid, got %v", issues)
	}
}

func TestValidate_RequirementsNamePR(t *testing.T) {
	caps := &Caps{Groups: map[string][]string{"chat": {"send", "set_state"}, "market": {"set", "path"}}, Kinds: []string{"chat.out", "exchange.req"}}

	f := parseString(t, withExpect(base+"timeline:\n  - fault.add: {kind: latency}\n  - market.recording: {symbol: BTC/USDT}\n"))
	issues := Validate(f, caps)
	if got := issueAt(issues, "/timeline/0/fault.add"); !strings.Contains(got, "SBX-13") || !strings.Contains(got, "group `fault`") {
		t.Errorf("fault.add: %q", got)
	}
	if got := issueAt(issues, "/timeline/1/market.recording"); !strings.Contains(got, "SBX-21") {
		t.Errorf("market.recording: %q", got)
	}
	if len(Validate(f, nil)) != 0 {
		t.Error("with no caps only static checks run")
	}

	f = parseString(t, base+"expect:\n  - order: {venue: okx, client_id: abc}\n  - order: {venue: okx, count: 1}\n")
	issues = Validate(f, caps)
	if got := issueAt(issues, "/expect/0/order/client_id"); !strings.Contains(got, "SBX-14a") {
		t.Errorf("order.client_id: %q (%v)", got, issues)
	}
	if got := issueAt(issues, "/expect/1/order"); got != "" {
		t.Errorf("a plain order count reads exchange.req: %q", got)
	}

	f = parseString(t, base+"expect:\n  - event: {kind: trigger.candidate}\n  - web: {host: x}\n")
	issues = Validate(f, &Caps{Kinds: []string{"event.*"}})
	if got := issueAt(issues, "/expect/0/event"); got != "" {
		t.Errorf("event.* covers event.trigger.candidate: %q", got)
	}
	if got := issueAt(issues, "/expect/1/web"); !strings.Contains(got, "SBX-15") {
		t.Errorf("web: %q", got)
	}

	if got := issueAt(Validate(parseString(t, base+"llm: live\n"), nil), "/llm"); !strings.Contains(got, "SBX-16") {
		t.Errorf("llm: live: %q", got)
	}
	if got := issueAt(Validate(parseString(t, base+"timeline:\n  - restart: {kill_at: {workflow: dn.open}}\n"), nil), "/timeline/0/restart/kill_at"); !strings.Contains(got, "SBX-20") {
		t.Errorf("restart kill_at: %q", got)
	}
}

func TestCountGrammar(t *testing.T) {
	for _, ok := range []string{"1", `">= 1"`, `"<= 2"`, "0"} {
		f := parseString(t, base+"expect:\n  - message: {to: owner, count: "+ok+"}\n")
		if issues := Validate(f, nil); len(issues) != 0 {
			t.Errorf("count %s: %v", ok, issues)
		}
	}
	f := parseString(t, base+"expect:\n  - message: {to: owner, count: \"~1\"}\n")
	if got := issueAt(Validate(f, nil), "/expect/0/count"); got == "" {
		t.Error(`count "~1" must be refused`)
	}
}

func TestValidate_ExpectationShapes(t *testing.T) {
	f := parseString(t, base+`expect:
  - id: a
    message: {to: owner, contians: [x]}
  - id: a
    message: {to: owner}
    within: {from: +30m, to: +10m}
    during: "25:00-07:00"
  - id: c
    decision: {trigger: x}
    replay_match: foo
  - message: {to: owner, count: 1}
    event: {kind: x}
  - nothing: {}
`)
	issues := Validate(f, nil)
	for path, want := range map[string]string{
		"/expect/0/message/contians": "unknown field",
		"/expect/1/id":               "duplicate",
		"/expect/1/within":           "after",
		"/expect/1/during":           "HH:MM",
		"/expect/2/replay_match":     "message",
		"/expect/3":                  "one kind key",
		"/expect/4/nothing":          "unknown expectation kind",
	} {
		if got := issueAt(issues, path); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q (all: %v)", path, got, want, issues)
		}
	}
}

func TestValidate_DesignExamples(t *testing.T) {
	for _, name := range []string{"design_8_1.yaml", "design_8_2.yaml"} {
		if issues := Validate(parseFixture(t, name), nil); len(issues) != 0 {
			t.Errorf("%s: %v", name, issues)
		}
	}
	files, err := Expand(parseFixture(t, "design_8_3.yaml"))
	if err != nil || len(files) != 2 {
		t.Fatalf("Expand = %d, %v", len(files), err)
	}
	for _, g := range files {
		if issues := Validate(g, nil); len(issues) != 1 || issues[0].Path != "/llm" || !strings.Contains(issues[0].Msg, "SBX-16") {
			t.Errorf("8.3 = %v, want only the llm: live issue", issues)
		}
	}
}

func TestValidate_NoCredentialsOrRealHosts(t *testing.T) {
	f := parseString(t, base+`profile: {api_key: x}
setup:
  - call_tool: {name: t, args: {secret: x}}
web:
  - {url: "https://api.binance.com/api/v3/ticker", body: x}
  - {url: "https://news.example.com/a", body: ok}
instance:
  config: {proactive: {token: x}}
  workspace: {"a.md": "http://api.okx.com/x"}
`)
	issues := Validate(f, nil)
	for path, want := range map[string]string{
		"/profile/api_key":                 "credential",
		"/setup/0/call_tool/args/secret":   "credential",
		"/web/0/url":                       "real exchange host",
		"/instance/config/proactive/token": "credential",
		"/instance/workspace/a.md":         "real exchange host",
	} {
		if got := issueAt(issues, path); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q (all: %v)", path, got, want, issues)
		}
	}
	if got := issueAt(issues, "/web/1/url"); got != "" {
		t.Errorf("a fake host in a SimWeb fixture is fine: %q", got)
	}
}

// A scenario that expects nothing passes on the implicit checks alone: refused.
func TestValidate_ExpectIsRequired(t *testing.T) {
	for name, src := range map[string]string{"missing": base, "empty list": base + "expect: []\n", "null": base + "expect:\n"} {
		if got := issueAt(Validate(parseString(t, src), nil), "/expect"); !strings.Contains(got, "at least one expectation") {
			t.Errorf("%s: /expect = %q", name, got)
		}
	}
	if got := issueAt(Validate(parseString(t, withExpect(base)), nil), "/expect"); got != "" {
		t.Errorf("one expectation is enough: %q", got)
	}
}

// event.* covers every name, so a misspelt kind would match nothing and make a count of
// zero pass for ever: the kind is checked against the bus's own list.
func TestValidate_EventKindMustExist(t *testing.T) {
	check := func(expr string) string {
		f := parseString(t, base+"expect:\n  - "+expr+"\n")
		return issueAt(Validate(f, &Caps{Kinds: []string{"event.*", "tool.call", "chat.state"}}), "/expect/0/event/kind")
	}
	for _, bad := range []string{
		"event: {kind: trigger.candiate, count: 0}",
		"event: {kind: event.trigger.candiate, count: 0}",
		"event: {kind: trigger.*x}",
		"event: {kind: nothing.*, count: 0}",
		"event: {kind: approval.created}", // journaled as proposal, never as event.approval.created
		"event: {kind: approval.*}",
	} {
		if check(bad) == "" {
			t.Errorf("%s must be refused", bad)
		}
	}
	if got := check("event: {kind: trigger.candiate, count: 0}"); !strings.Contains(got, "not an event kind") {
		t.Errorf("message = %q", got)
	}
	for _, ok := range []string{
		"event: {kind: trigger.candidate, count: 0}",
		"event: {kind: event.attention.decision}",
		"event: {kind: trigger.*}",
		"event: {kind: perception.*}",
		"event: {kind: \"*\"}",
		"event: {kind: tool.call}", // a journal-only kind goes through needKind
		"event: {kind: chat.state}",
	} {
		if got := check(ok); got != "" {
			t.Errorf("%s refused: %s", ok, got)
		}
	}
}
