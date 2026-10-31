package scenario

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/channels/simchat"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

var (
	ict       = time.FixedZone("ICT", 7*3600)
	evalStart = time.Date(2026, 10, 20, 9, 0, 0, 0, ict)
)

// evalFixture is a fake instance with a journal the test fills with entries made the
// way the real producers make them, and an env that reads it through the lab client.
type evalFixture struct {
	t  *testing.T
	in *fakeInstance
	v  *env
}

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	sup := newFakeSup(t)
	info, err := sup.Up(context.Background(), supervisor.UpOptions{ClockStart: evalStart.Format(time.RFC3339), ClockMode: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := sup.Client(info.Name)
	if err != nil {
		t.Fatal(err)
	}
	return &evalFixture{t: t, in: sup.order[0], v: newEnv(c, evalStart, ict)}
}

// chatOut journals what simchat journals for a send: the OutMsg and sandbox:true.
func (f *evalFixture) chatOut(to, text, severity string, delivered bool, at time.Duration, buttons ...string) {
	f.t.Helper()
	m := simchat.OutMsg{To: to, Text: text, Severity: severity, Delivered: delivered, Silent: false}
	for _, b := range buttons {
		m.Buttons = append(m.Buttons, simchat.Button{Action: b, Label: b})
	}
	payload, err := json.Marshal(struct {
		simchat.OutMsg
		Sandbox bool `json:"sandbox"`
	}{m, true})
	if err != nil {
		f.t.Fatal(err)
	}
	f.in.j.Append(labjournal.Entry{Kind: "chat.out", Source: "simchat", Subject: to, SimTime: evalStart.Add(at), Payload: payload})
}

func (f *evalFixture) check(yaml string, wantOK bool) ExpectResult {
	f.t.Helper()
	sc := parseString(f.t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\nexpect:\n  - "+yaml+"\n")
	if issues := Validate(sc, nil); len(issues) != 0 {
		f.t.Fatalf("%s is not valid: %v", yaml, issues)
	}
	r, _ := f.v.eval(context.Background(), 0, &sc.Expect[0])
	if r.OK != wantOK {
		f.t.Errorf("%s: OK = %v, want %v\n want: %s\n  got: %s", yaml, r.OK, wantOK, r.Want, r.Got)
	}
	return r
}

func TestEval_MessageWithinDuring(t *testing.T) {
	f := newEvalFixture(t)
	f.chatOut("dm:owner", "BTC crossed 100000", "warning", true, 6*time.Minute, "mute", "snooze_1h")
	f.chatOut("dm:owner", "BTC crossed 100000", "warning", false, 7*time.Minute) // the outage: a refused send
	f.chatOut("dm:owner", "BTC above 100500", "warning", true, 31*time.Minute)
	f.chatOut("group:team", "BTC", "info", true, 40*time.Minute)
	f.chatOut("dm:stranger", "hi", "info", true, 50*time.Minute)
	f.chatOut("dm:owner", "night note", "info", true, 14*time.Hour+30*time.Minute) // 23:30 in Bangkok, 16:30 UTC

	// within is sim time from clock.start; a refused send is no message.
	r := f.check(`{message: {to: owner, contains: ["BTC", "100"], count: 1}, within: {from: "+30m", to: "+35m"}}`, true)
	if len(r.MatchedSeqs) != 1 {
		t.Errorf("MatchedSeqs = %v, want exactly the one entry counted", r.MatchedSeqs)
	}
	f.check(`{message: {to: owner, contains: [BTC], count: 2}, within: {from: "+0m", to: "+1h"}}`, true)
	f.check(`{message: {to: owner, contains: [BTC], count: 1}, within: {from: "+5m", to: "+10m"}}`, true)
	f.check(`{message: {to: owner, contains: [BTC], count: 0}, within: {from: "+35m", to: "+2h"}}`, true)

	// to: a persona list and a group glob.
	f.check(`{message: {to: [stranger, "group:*"], count: 0}}`, false)
	f.check(`{message: {to: "group:*", count: 1}}`, true)
	f.check(`{message: {to: "group:other", count: 0}}`, true)

	// severity, buttons, contains.
	f.check(`{message: {to: owner, severity: warning, buttons: [mute], count: 1}}`, true)
	f.check(`{message: {to: owner, buttons: [approve], count: 0}}`, true)
	f.check(`{message: {to: owner, contains: [ETH], count: 0}}`, true)

	// during: 22:00-07:00 wraps midnight and is the profile's zone, not UTC.
	f.check(`{message: {to: owner, during: "22:00-07:00", count: 1}}`, true)
	f.check(`{message: {to: owner, during: "09:00-10:00", count: 2}}`, true)
	f.check(`{message: {to: owner, during: "00:00-07:00", count: 0}}`, true)

	// A failing one says what it wanted and what it saw.
	r = f.check(`{message: {to: owner, contains: [BTC], count: 5}}`, false)
	if !strings.Contains(r.Want, "count 5") || !strings.Contains(r.Got, "of 6 chat.out") || len(r.Excerpt) == 0 {
		t.Errorf("want/got/excerpt = %q / %q / %d entries", r.Want, r.Got, len(r.Excerpt))
	}
}

// publish journals a bus event through the real observer, and waits for it.
func (f *evalFixture) publish(evs ...events.Event) {
	f.t.Helper()
	bus := events.NewBus()
	obs, err := labjournal.StartObserver(context.Background(), bus, f.in.j)
	if err != nil {
		f.t.Fatal(err)
	}
	defer obs.Close()
	for _, e := range evs {
		if e.Time.IsZero() {
			e.Time = evalStart.Add(time.Minute)
		}
		bus.PublishNonBlocking(e)
	}
	res, err := f.in.j.WaitFor(context.Background(), labjournal.Match{Count: ">= " + strconv.Itoa(len(evs))}, 0, 5*time.Second)
	if err != nil || !res.Matched {
		f.t.Fatalf("the observer journaled nothing: %v", err)
	}
	for !obs.Idle() {
		time.Sleep(time.Millisecond)
	}
}

func decisionEvent(reason string, sev proactive.Severity) events.Event {
	return events.Event{Kind: events.KindAttentionDecision, Source: events.Source{Component: "engine"}, Payload: attention.Decision{
		Disposition: proactive.DispDrop, Reason: reason,
		Candidate: proactive.Candidate{TriggerID: "move_vs_vol:BTC/USDT", Severity: sev, Subject: "BTC/USDT"},
	}}
}

func TestEval_DecisionReasonNegation(t *testing.T) {
	f := newEvalFixture(t)
	f.publish(decisionEvent(attention.ReasonMuted, proactive.SeverityCritical))

	// The only critical decision was dropped as muted: "!muted" fails, "muted" holds.
	f.check(`decision: {trigger: move_vs_vol, severity: critical, reason: "!muted"}`, false)
	f.check(`decision: {trigger: move_vs_vol, severity: critical, reason: muted}`, true)
	f.check(`decision: {trigger: move_vs_vol, severity: warning}`, false)

	f2 := newEvalFixture(t)
	f2.publish(decisionEvent(attention.ReasonMuted, proactive.SeverityWarning), decisionEvent(attention.ReasonProtective, proactive.SeverityCritical))
	f2.check(`decision: {trigger: move_vs_vol, severity: critical, reason: "!muted"}`, true)
	f2.check(`decision: {severity: critical, disposition: drop, count: 1}`, true)
}

// The design's H8 expectation: an event by its bus kind, with the candidate's fields as
// shorthand, and fields: as payload paths.
func TestEval_EventShorthandAndFields(t *testing.T) {
	f := newEvalFixture(t)
	f.publish(events.Event{Kind: events.KindTriggerCandidate, Source: events.Source{Component: "engine"},
		Payload: proactive.Candidate{TriggerID: "price_cross:7", Subject: "BTC/USDT", Severity: proactive.SeverityWarning}})
	f.check(`{event: {kind: trigger.candidate, trigger: price_cross, subject: BTC/USDT}, within: {from: "+0m", to: "+10m"}}`, true)
	f.check(`event: {kind: trigger.candidate, trigger: price_cross, subject: ETH/USDT}`, false)
	f.check(`event: {kind: trigger.candidate, fields: {"event.payload.Severity": warning, "event.payload.TriggerID": "price_cross:7"}}`, true)
	f.check(`event: {kind: trigger.candidate, fields: {"event.payload.Severity": critical}}`, false)
	f.check(`event: {kind: trigger.candidate, count: 0}`, false)
}

func TestEval_ProposalPolicyLLM(t *testing.T) {
	f := newEvalFixture(t)
	f.publish(
		events.Event{Kind: events.KindApprovalCreated, Source: events.Source{Component: "approval"},
			Payload: map[string]any{"id": "1", "code": "A1", "kind": "order", "status": "pending", "origin": "proactive"}},
		events.Event{Kind: events.KindPolicyDecision, Source: events.Source{Component: "policy", Name: "create_order"},
			Attrs: map[string]any{"tool": "create_order", "mode": "ask", "tier": "T3"}},
	)
	f.in.j.Append(labjournal.Entry{Kind: "llm.call", Subject: "proactive", Payload: json.RawMessage(`{"turn":"proactive"}`), SimTime: evalStart})
	f.in.j.Append(labjournal.Entry{Kind: "llm.complete", Subject: "news", Payload: json.RawMessage(`{"caller":"news"}`), SimTime: evalStart})

	f.check(`proposal: {kind: order, status: pending}`, true)
	f.check(`proposal: {kind: mandate}`, false)
	f.check(`policy: {verdict: ask, gate: create_order}`, true)
	f.check(`policy: {verdict: act}`, false)
	f.check(`llm: {turn: proactive, count: 1}`, true)
	f.check(`llm: {turn: heartbeat_legacy, count: 0}`, true)
	f.check(`llm: {turn: complete, count: 1}`, true)
	f.check(`llm: {count: 2}`, true)
}

// Before SBX-14a an order is a successful create request in the request log; the
// payload is sandbox.ReqInfo, which has no symbol or side.
func TestEval_OrderFallbackBefore14a(t *testing.T) {
	f := newEvalFixture(t)
	req := func(venue, method, path string, status int) {
		b, _ := json.Marshal(sandbox.ReqInfo{Venue: venue, Host: venue + ".sandbox", Method: method, Path: path, Stage: "orders", Status: status})
		f.in.j.Append(labjournal.Entry{Kind: "exchange.req", Source: "sandbox", Subject: venue, SimTime: evalStart, Payload: b})
	}
	req("okx", "POST", "/api/v5/trade/order", 200)
	req("okx", "GET", "/api/v5/trade/order", 200)
	req("okx", "POST", "/api/v5/trade/order", 500)
	req("okx", "GET", "/api/v5/market/ticker", 200)
	req("binance", "POST", "/fapi/v1/order", 200)
	req("binance", "POST", "/fapi/v1/leverage", 200)

	f.check(`order: {venue: okx, count: 1}`, true)
	f.check(`order: {venue: binance, count: 1}`, true)
	f.check(`order: {count: 2}`, true)
	f.check(`order: {venue: bitkub, count: 0}`, true)

	// What the request log cannot answer is refused up front, not guessed.
	sc := parseString(t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\nexpect:\n  - order: {venue: okx, symbol: BTC/USDT, count: 1}\n")
	issues := Validate(sc, &Caps{Kinds: []string{"exchange.req"}})
	if got := issueAt(issues, "/expect/0/order/symbol"); !strings.Contains(got, "SBX-14a") {
		t.Errorf("order.symbol without exchange.order: %q (%v)", got, issues)
	}
}

func TestEval_Logs(t *testing.T) {
	f := newEvalFixture(t)
	f.v.logs = func() ([]string, error) {
		return []string{
			"\x1b[90m10:00:00\x1b[0m \x1b[32mINF\x1b[0m gateway started",
			"\x1b[90m10:00:01\x1b[0m \x1b[31mERR\x1b[0m sim: tool failed: expected failure",
		}, nil
	}
	r := f.check(`logs: {}`, false)
	if !strings.Contains(r.Got, "expected failure") {
		t.Errorf("got %q, want the offending line", r.Got)
	}
	f.check(`logs: {allow: ["expected failure"]}`, true)
}

func TestEval_UntilExpectation(t *testing.T) {
	f := newEvalFixture(t)
	o, err := f.v.untilOptions(map[string]any{"until": map[string]any{"message": map[string]any{"to": "owner", "contains": []any{"BTC"}}}, "step": "10m", "max_sim": "2h"})
	if err != nil || o.Step != 10*time.Minute || o.MaxSim != 2*time.Hour {
		t.Fatalf("untilOptions = %+v, %v", o, err)
	}
	if ok, _, err := o.Check(context.Background()); ok || err != nil {
		t.Fatalf("nothing sent yet: %v, %v", ok, err)
	}
	f.chatOut("dm:owner", "BTC jumped", "warning", true, 25*time.Minute)
	ok, entry, err := o.Check(context.Background())
	if !ok || err != nil || entry == nil || !entry.SimTime.Equal(evalStart.Add(25*time.Minute)) {
		t.Fatalf("after the message: %v, %+v, %v; want a match at its sim time", ok, entry, err)
	}
	for _, bad := range []map[string]any{{}, {"until": map[string]any{"nothing": map[string]any{}}}, {"until": map[string]any{"message": map[string]any{}}, "step": "soon"}} {
		if _, err := f.v.untilOptions(bad); err == nil {
			t.Errorf("untilOptions(%v) must fail", bad)
		}
	}
}

// A passing expectation lists the chat.out seqs it counted; replay_match is parsed and
// carried and never evaluated.
func TestRunner_RecordsMatchedSeqs(t *testing.T) {
	sup := newFakeSup(t)
	var want []int64
	sup.onUp = func(_ int, in *fakeInstance) {
		fx := &evalFixture{t: t, in: in}
		fx.chatOut("dm:owner", "BTC hit", "warning", true, 2*time.Minute)
		fx.chatOut("dm:owner", "nothing", "info", true, 3*time.Minute)
		fx.chatOut("dm:owner", "BTC again", "warning", true, 4*time.Minute)
	}
	f := scenarioOf(t, `
timeline:
  - advance: 10m
expect:
  - id: two-btc
    message: {to: owner, contains: [BTC], count: 2, replay_match: "BTC hit"}
`)
	if f.Expect[0].ReplayMatch != "BTC hit" {
		t.Fatalf("replay_match was not carried: %+v", f.Expect[0])
	}
	res := runOne(t, sup, f).Runs[0]
	if !res.Pass {
		t.Fatalf("result = %+v", res)
	}
	for _, e := range res.Expect {
		if e.ID == "two-btc" {
			want = e.MatchedSeqs
		}
	}
	got := journalSeqs(t, sup.order[0], "BTC")
	if len(want) != 2 || len(got) != 2 || want[0] != got[0] || want[1] != got[1] {
		t.Errorf("MatchedSeqs = %v, want the seqs of the two BTC messages %v", want, got)
	}
}

func journalSeqs(t *testing.T, in *fakeInstance, contains string) []int64 {
	t.Helper()
	es, err := in.j.Query(context.Background(), labjournal.Query{Kinds: []string{"chat.out"}})
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range es {
		if strings.Contains(string(e.Payload), contains) {
			out = append(out, e.Seq)
		}
	}
	return out
}

// An expectation that fails shows its want and got, and the run carries the model's calls.
func TestRunner_FailingExpectationExplainsItself(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		in.j.Append(labjournal.Entry{Kind: "llm.call", Subject: "user", Payload: json.RawMessage(`{"turn":"user","text":"hello"}`)})
	}
	rep := runOne(t, sup, scenarioOf(t, "expect:\n  - id: no-owner\n    message: {to: owner, count: 1}\n"))
	res := rep.Runs[0]
	var e *ExpectResult
	for i := range res.Expect {
		if res.Expect[i].ID == "no-owner" {
			e = &res.Expect[i]
		}
	}
	if res.Pass || e == nil || e.OK || !strings.Contains(e.Want, "count 1") || !strings.Contains(e.Got, "0 matched") || rep.ExitCode() != ExitFailed {
		t.Fatalf("result = %+v (exit %d)", res, rep.ExitCode())
	}
	if len(res.LLMCalls) != 1 || !strings.Contains(string(res.LLMCalls[0]), "hello") {
		t.Errorf("LLMCalls = %s", res.LLMCalls)
	}
}

// A failing expectation shows the story around its window even when the window itself is
// empty: the entries before and after it, not the clock steps and settles.
func TestEval_ExcerptAroundAnEmptyWindow(t *testing.T) {
	f := newEvalFixture(t)
	for i := 0; i < 15; i++ {
		f.chatOut("dm:owner", "early", "info", true, time.Duration(i)*time.Minute)
	}
	for i := 0; i < 15; i++ {
		f.chatOut("dm:owner", "late", "info", true, 60*time.Minute+time.Duration(i)*time.Minute)
	}
	f.in.j.Append(labjournal.Entry{Kind: "settle", Payload: json.RawMessage(`{"ok":true}`), SimTime: evalStart.Add(31 * time.Minute)})
	r := f.check(`{message: {to: owner, count: 1}, within: {from: "+30m", to: "+40m"}}`, false)
	if len(r.Excerpt) != 20 {
		t.Fatalf("excerpt = %d entries, want 20", len(r.Excerpt))
	}
	early, late := 0, 0
	for _, e := range r.Excerpt {
		if e.Kind != "chat.out" {
			t.Errorf("the excerpt has a %s entry", e.Kind)
		}
		if strings.Contains(string(e.Payload), "early") {
			early++
		} else {
			late++
		}
	}
	if early != 10 || late != 10 {
		t.Errorf("%d early and %d late entries, want the 10 nearest on each side", early, late)
	}
}

// The until form takes an event kind too, and a misspelt one is refused, not waited for.
func TestEval_UntilRefusesAMisspeltEventKind(t *testing.T) {
	f := newEvalFixture(t)
	until := func(kind string) error {
		_, err := f.v.untilOptions(map[string]any{"until": map[string]any{"event": map[string]any{"kind": kind}}})
		return err
	}
	if err := until("trigger.candiate"); err == nil || !strings.Contains(err.Error(), "not an event kind") {
		t.Errorf("a typo: %v", err)
	}
	for _, ok := range []string{"trigger.candidate", "trigger.*", "tool.call"} {
		if err := until(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}
