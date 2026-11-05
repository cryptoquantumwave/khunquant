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
		// cli passes every owner gate whatever proactive.owner says; no chat turn can carry it (140-1).
		{"call_tool as cli", "timeline:\n  - {call_tool: {name: get_ticker, as: cli}}\n", "/timeline/0/call_tool/as", "local console"},
		{"call_tool as owner", "timeline:\n  - {call_tool: {name: get_ticker, as: owner}}\n", "", ""},
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

	for _, ok := range []string{"llm: live\n", "llm: replay\n", "llm: replay\nmiss: scripted\n", "llm: scripted\n"} {
		if issues := Validate(parseString(t, base+ok), nil); issueAt(issues, "/llm") != "" || issueAt(issues, "/miss") != "" {
			t.Errorf("%q: %v", ok, issues)
		}
	}
	for in, path := range map[string]string{"llm: turbo\n": "/llm", "miss: error\n": "/miss", "llm: live\nmiss: error\n": "/miss", "llm: replay\nmiss: sometimes\n": "/miss"} {
		if got := issueAt(Validate(parseString(t, base+in), nil), path); got == "" {
			t.Errorf("%q must be refused at %s", in, path)
		}
	}
	if got := issueAt(Validate(parseString(t, base+"timeline:\n  - restart: {kill_at: {workflow: dn.open}}\n"), nil), "/timeline/0/restart/kill_at"); !strings.Contains(got, "step") {
		t.Errorf("an incomplete kill_at: %q, want it to name the missing step", got)
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
		if issues := Validate(g, nil); len(issues) != 0 {
			t.Errorf("8.3 = %v, want it valid now that live is supported", issues)
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

// An expectation that cannot fail checks nothing. Each of these would pass whatever the run
// did: a modifier that is dropped or always holds, a path, state or value the journal never
// holds (which matches nothing, and an absence count reads that as success). 161-1, 182-2.
func TestValidate_VacuousExpectationsAreRefused(t *testing.T) {
	check := func(expr string) []Issue {
		t.Helper()
		return Validate(parseString(t, base+"expect:\n  - "+expr+"\n"), nil)
	}
	for _, c := range []struct{ expr, path, want string }{
		// modifiers
		{`{message: {to: owner, count: 1}, within: {from: "+30m", unitl: "+35m"}}`, "/expect/0/within/unitl", "unknown field for within"},
		{`{message: {to: owner, count: 1, within: {from: "+30m", unitl: "+35m"}}}`, "/expect/0/within/unitl", "unknown field for within"},
		{`{message: {to: owner}, count: [1]}`, "/expect/0/count", "one value"},
		{`{message: {to: owner, count: 0}, during: {from: "22:00"}}`, "/expect/0/during", "one value"},
		{`{decision: {trigger: move_vs_vol}, count: ">= 0"}`, "/expect/0/count", "checks nothing"},
		// payload paths
		{`event: {kind: tool.call, fields: {nme: install_skill}, count: 0}`, "/expect/0/event/fields/nme", "no tool.call entry has this path"},
		{`event: {kind: tool.call, fields: {nme: "!install_skill"}}`, "/expect/0/event/fields/nme", "no tool.call entry has this path"},
		{`event: {kind: trigger.candidate, fields: {"evnt.payload.Severity": critical}, count: 0}`, "/expect/0/event/fields/evnt.payload.Severity", "no event.trigger.candidate entry"},
		{`event: {kind: llm.call, fields: {turn: proactive}, count: 0}`, "/expect/0/event/fields/turn", "not declared"},
		{`event: {kind: tool.call, fields: name, count: 0}`, "/expect/0/event/fields", "mapping"},
		// closed sets
		{`{workflow: {kind: dca.execute, state: compelted}, count: 0}`, "/expect/0/workflow/state", "must be one of"},
		{`{decision: {disposition: dorp}, count: 0}`, "/expect/0/decision/disposition", "must be one of"},
		{`order: {venue: binanceth, count: 0}`, "/expect/0/order/venue", "must be one of binance, bitkub, okx"},
		{`order: {venue: "!okx", count: 0}`, "/expect/0/order/venue", "must be one of"},
		{`order: {venue: binance, status: closed, count: 0}`, "/expect/0/order/status", "must be one of"},
		{`order: {venue: binance, status: cancelled, count: 0}`, "/expect/0/order/status", "must be one of"},
		{`order: {venue: okx, side: short, count: 0}`, "/expect/0/order/side", "must be one of buy, sell"},
		{`order: {venue: okx, side: "!long", count: 0}`, "/expect/0/order/side", "must be one of buy, sell"},
		{`order: {venue: binance, symbol: BTCUSDT, count: 0}`, "/expect/0/order/symbol", "unified symbol"},
		{`order: {venue: okx, symbol: BTC-USDT-SWAP, count: 0}`, "/expect/0/order/symbol", "unified symbol"},
		{`order: {venue: binance.spot, side: buy, count: 0}`, "/expect/0/order/venue", "must be one of binance, okx"},
		{`order: {client_id: 12345, count: 0}`, "/expect/0/order/client_id", "string"},
		{`order: {venue: okx, reduce_only: ture, count: 0}`, "/expect/0/order/reduce_only", "true or false"},
	} {
		if got := issueAt(check(c.expr), c.path); !strings.Contains(got, c.want) {
			t.Errorf("%s: issue at %s = %q, want %q", c.expr, c.path, got, c.want)
		}
	}
	// What the producers write is still accepted, in absence counts too.
	for _, ok := range []string{
		`{message: {to: owner, count: 1}, within: {from: "+30m", to: "+35m"}}`,
		`{message: {to: owner, count: "<= 2"}}`,
		`event: {kind: tool.call, fields: {name: install_skill}, count: 0}`,
		`event: {kind: tool.call, fields: {"args.symbol": "BTC/USDT"}, count: 0}`,
		`event: {kind: trigger.candidate, fields: {"event.payload.Severity": critical}, count: 0}`,
		`event: {kind: config.reload, fields: {ok: false}, count: 0}`,
		`event: {kind: fault.fired, fields: {effect: create_reports_fill}}`,
		`event: {kind: exchange.order, fields: {wire: BTCUSDT}, count: 0}`,
		`event: {kind: llm.call, fields: {turn: proactive}, count: 1}`,
		`{workflow: {kind: dn.open, state: succeeded}, count: 1}`,
		`decision: {disposition: "!drop"}`,
		`order: {venue: okx, symbol: "!BTC/USDT:USDT", side: sell, status: partially_filled, client_id: "12345", reduce_only: false, count: 0}`,
		`order: {venue: bitkub, count: 0}`,
	} {
		if issues := check(ok); len(issues) != 0 {
			t.Errorf("%s refused: %v", ok, issues)
		}
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
