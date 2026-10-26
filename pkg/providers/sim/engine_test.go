package sim

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

type journalEntry struct {
	Kind, Subject string
	Payload       json.RawMessage
}

type memJournal struct {
	mu sync.Mutex
	es []journalEntry
}

func (j *memJournal) Record(kind, subject string, payload json.RawMessage) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.es = append(j.es, journalEntry{kind, subject, payload})
}

func (j *memJournal) entries() []journalEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]journalEntry(nil), j.es...)
}

func (j *memJournal) payload(t *testing.T, i int) map[string]any {
	t.Helper()
	es := j.entries()
	if i >= len(es) {
		t.Fatalf("journal has %d entries, want index %d", len(es), i)
	}
	var m map[string]any
	if err := json.Unmarshal(es[i].Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// newTestEngine builds an engine whose every turn is a "user" turn with turn id "t1".
func newTestEngine(t *testing.T, rules ...Rule) (*Engine, *memJournal) {
	t.Helper()
	j := &memJournal{}
	e, err := NewEngine(EngineOptions{Journal: j, TurnInfo: func(context.Context) CallInfo {
		return CallInfo{Turn: "user", Origin: "remote", TurnID: "t1", Sender: "simchat:owner"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Put(rules, true, ""); err != nil {
		t.Fatal(err)
	}
	return e, j
}

func user(s string) []protocoltypes.Message {
	return []protocoltypes.Message{{Role: "system", Content: "SYS"}, {Role: "user", Content: s}}
}

var agentOpts = map[string]any{"prompt_cache_key": "main"}

func chat(t *testing.T, e *Engine, msgs []protocoltypes.Message) *protocoltypes.LLMResponse {
	t.Helper()
	r, err := e.Chat(context.Background(), msgs, nil, "m", agentOpts)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return r
}

func TestRuleOrderAndTimes(t *testing.T) {
	e, _ := newTestEngine(t,
		Rule{ID: "once", Times: 1, Match: Match{PromptContains: []string{"hi"}}, Respond: Respond{Text: "first"}},
		Rule{ID: "any", Match: Match{Turn: "user"}, Respond: Respond{Text: "second"}},
	)
	for i, want := range []string{"first", "second", "second"} {
		if got := chat(t, e, user("hi there")).Content; got != want {
			t.Errorf("call %d = %q, want %q", i, got, want)
		}
	}
	// A rule that sets several fields needs all of them.
	e2, _ := newTestEngine(t, Rule{ID: "r", Match: Match{Turn: "user", Sender: "simchat:bob", PromptContains: []string{"x"}}, Respond: Respond{Text: "no"}})
	if got := chat(t, e2, user("x")).Content; got != noMatchText {
		t.Errorf("a rule for another sender answered: %q", got)
	}
}

func TestMatchFields(t *testing.T) {
	msgs := []protocoltypes.Message{
		{Role: "system", Content: "SYS", SystemParts: []protocoltypes.ContentBlock{{Type: "text", Text: "part-two"}}},
		{Role: "user", Content: "buy btc"},
	}
	e, _ := newTestEngine(t,
		Rule{ID: "sys", Match: Match{SystemContains: []string{"part-two"}, PromptContains: []string{"buy", "btc"}}, Respond: Respond{Text: "sys"}},
	)
	if got := chat(t, e, msgs).Content; got != "sys" {
		t.Errorf("system parts / prompt match = %q", got)
	}
	e, _ = newTestEngine(t, Rule{ID: "tools", Match: Match{ToolsInclude: []string{"message"}}, Respond: Respond{Text: "has"}})
	offered := []protocoltypes.ToolDefinition{{Function: protocoltypes.ToolFunctionDefinition{Name: "message"}}}
	if r, _ := e.Chat(context.Background(), user("x"), offered, "m", agentOpts); r.Content != "has" {
		t.Errorf("tools_include with the tool offered = %q", r.Content)
	}
	if r, _ := e.Chat(context.Background(), user("x"), nil, "m", agentOpts); r.Content != noMatchText {
		t.Errorf("tools_include without the tool = %q", r.Content)
	}
}

func TestTwoToolCallsInOneResponse(t *testing.T) {
	e, j := newTestEngine(t, Rule{ID: "dn", Match: Match{Turn: "user"}, Respond: Respond{ToolCalls: []ToolCallSpec{
		{Name: "open_delta_neutral_position", Args: map[string]any{"plan_id": "p1", "confirm": true}},
		{Name: "open_delta_neutral_position", Args: map[string]any{"plan_id": "p1", "confirm": true}},
	}}})
	r := chat(t, e, user("go"))
	if r.FinishReason != "tool_calls" || len(r.ToolCalls) != 2 {
		t.Fatalf("finish %q, %d calls", r.FinishReason, len(r.ToolCalls))
	}
	a, b := r.ToolCalls[0], r.ToolCalls[1]
	if a.ID == b.ID || !strings.HasPrefix(a.ID, "sim_dn_") || a.Type != "function" || a.Name != "open_delta_neutral_position" ||
		a.Function == nil || a.Function.Name != a.Name || a.Arguments["plan_id"] != "p1" {
		t.Errorf("tool calls = %+v / %+v", a, b)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(a.Function.Arguments), &args); err != nil || args["confirm"] != true {
		t.Errorf("function arguments %q: %v", a.Function.Arguments, err)
	}
	// The journal names the calls.
	if calls, _ := j.payload(t, 0)["tool_calls"].([]any); len(calls) != 2 {
		t.Errorf("journal tool_calls = %v", j.payload(t, 0)["tool_calls"])
	}
	// Mutating the delivered arguments does not change the script.
	a.Arguments["plan_id"] = "changed"
	if got := chat(t, e, user("go")).ToolCalls[0].Arguments["plan_id"]; got != "p1" {
		t.Errorf("the script was edited through a response: %v", got)
	}
}

// A rule may emit any arguments, including ones the tool's schema does not declare.
func TestRuleEmitsArgsOutsideTheSchema(t *testing.T) {
	e, _ := newTestEngine(t, Rule{ID: "h6", Match: Match{Turn: "user"}, Respond: Respond{
		Tool: "install_skill", Args: map[string]any{"slug": "evil", "confirm": false, "nested": map[string]any{"a": []any{1, "b"}}}}})
	tc := chat(t, e, user("x")).ToolCalls[0]
	if v, ok := tc.Arguments["confirm"]; !ok || v != false || tc.Arguments["slug"] != "evil" {
		t.Errorf("arguments = %v", tc.Arguments)
	}
}

func toolResult(resp *protocoltypes.LLMResponse, content string) []protocoltypes.Message {
	msgs := user("go")
	msgs = append(msgs, protocoltypes.Message{Role: "assistant", ToolCalls: resp.ToolCalls})
	for _, tc := range resp.ToolCalls {
		msgs = append(msgs, protocoltypes.Message{Role: "tool", ToolCallID: tc.ID, Content: content})
	}
	return msgs
}

func TestThenTextStopsLoop(t *testing.T) {
	then := "all done"
	e, _ := newTestEngine(t,
		Rule{ID: "a_b", Match: Match{Turn: "user"}, Respond: Respond{Tool: "web_fetch", Args: map[string]any{"url": "u"}, ThenText: &then}},
	)
	first := chat(t, e, user("go"))
	if len(first.ToolCalls) != 1 {
		t.Fatal("no tool call")
	}
	// The rule id holds an underscore: the id is recovered from the last one.
	second := chat(t, e, toolResult(first, "page text"))
	if second.Content != then || second.FinishReason != "stop" || len(second.ToolCalls) != 0 {
		t.Errorf("follow-up = %+v, want the then_text and a stop", second)
	}
	// The follow-up did not use up the rule.
	e2, _ := newTestEngine(t, Rule{ID: "r", Times: 1, Match: Match{Turn: "user"}, Respond: Respond{Tool: "t"}})
	f := chat(t, e2, user("go"))
	if got := chat(t, e2, toolResult(f, "ok")).Content; got != defaultThen {
		t.Errorf("default then_text = %q", got)
	}
}

// Only a rule that reads a tool result may answer a result of our own tool call.
func TestFollowUpRulesReadToolResults(t *testing.T) {
	e, _ := newTestEngine(t,
		Rule{ID: "call", Match: Match{Turn: "user", PromptContains: []string{"go"}}, Respond: Respond{Tool: "web_fetch"}},
		Rule{ID: "react", Match: Match{LastTool: "web_fetch", LastToolResult: []string{"secret"}}, Respond: Respond{Text: "saw it"}},
	)
	first := chat(t, e, user("go"))
	if got := chat(t, e, toolResult(first, "the secret is 42")).Content; got != "saw it" {
		t.Errorf("a rule on the tool result = %q", got)
	}
	if got := chat(t, e, toolResult(first, "nothing")).Content; got != defaultThen {
		t.Errorf("a result the reading rule does not match = %q, want the default follow-up", got)
	}
}

func TestToolLoopGuard(t *testing.T) {
	// Each answer is a tool call that a "reads the result" rule calls again, for ever.
	e, _ := newTestEngine(t,
		Rule{ID: "loop", Match: Match{LastTool: "t"}, Respond: Respond{Tool: "t"}},
		Rule{ID: "start", Match: Match{Turn: "user"}, Respond: Respond{Tool: "t"}},
	)
	resp := chat(t, e, user("go"))
	var err error
	rounds := 1
	for ; err == nil && rounds < 50; rounds++ {
		resp, err = e.Chat(context.Background(), toolResult(resp, "r"), nil, "m", agentOpts)
	}
	if err == nil || !strings.Contains(err.Error(), "tool loop") || rounds > maxToolRounds+2 {
		t.Fatalf("after %d rounds err = %v, want sim: tool loop", rounds, err)
	}
	if len(e.loops) != 0 {
		t.Errorf("loop counters kept after the guard fired: %v", e.loops)
	}
}

func TestFallbackAndErrorRule(t *testing.T) {
	e, j := newTestEngine(t, Rule{ID: "boom", Match: Match{PromptContains: []string{"boom"}}, Respond: Respond{Error: "provider exploded"}})
	if r := chat(t, e, user("quiet")); r.Content != noMatchText {
		t.Errorf("unmatched text = %q", r.Content)
	}
	if got := j.payload(t, 0); got["unmatched"] != true || got["rule"] != "" {
		t.Errorf("unmatched call journalled as %v", got)
	}
	if _, err := e.Chat(context.Background(), user("boom"), nil, "m", agentOpts); err == nil || err.Error() != "provider exploded" {
		t.Errorf("error rule = %v", err)
	}
	if got := j.payload(t, 1); got["error"] != "provider exploded" || got["rule"] != "boom" {
		t.Errorf("error call journalled as %v", got)
	}
	if err := e.Put(nil, false, "error"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Chat(context.Background(), user("quiet"), nil, "m", agentOpts); err == nil {
		t.Error("fallback error: an unmatched call must fail")
	}
}

func TestPutValidates(t *testing.T) {
	e, _ := newTestEngine(t)
	ok := Rule{ID: "a", Respond: Respond{Text: "x"}}
	for name, rules := range map[string][]Rule{
		"no id":          {{Respond: Respond{Text: "x"}}},
		"duplicate":      {ok, ok},
		"bad turn":       {{ID: "b", Match: Match{Turn: "nonsense"}, Respond: Respond{Text: "x"}}},
		"bad caller":     {{ID: "b", Match: Match{Complete: "nonsense"}, Respond: Respond{Text: "x"}}},
		"empty respond":  {{ID: "b"}},
		"tool and calls": {{ID: "b", Respond: Respond{Tool: "t", ToolCalls: []ToolCallSpec{{Name: "u"}}}}},
		"error and text": {{ID: "b", Respond: Respond{Error: "e", Text: "x"}}},
		"unnamed call":   {{ID: "b", Respond: Respond{ToolCalls: []ToolCallSpec{{}}}}},
		"negative times": {{ID: "b", Times: -1, Respond: Respond{Text: "x"}}},
	} {
		if err := e.Put(rules, true, ""); err == nil {
			t.Errorf("%s: Put accepted the script", name)
		}
	}
	if err := e.Put([]Rule{ok}, true, "nonsense"); err == nil {
		t.Error("an unknown fallback was accepted")
	}
	if err := e.Put([]Rule{ok}, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.Put([]Rule{ok}, false, ""); err == nil {
		t.Error("appending a rule whose id exists was accepted")
	}
	if err := e.Put([]Rule{{ID: "b", Respond: Respond{Text: "y"}}}, false, ""); err != nil || len(e.rules) != 2 {
		t.Errorf("append: %v, %d rules", err, len(e.rules))
	}
}

type fakeFaults struct {
	delay time.Duration
	err   error
	seen  CallInfo
}

func (f *fakeFaults) BeforeLLM(_ context.Context, c CallInfo) (time.Duration, error) {
	f.seen = c
	return f.delay, f.err
}

func TestFaultHookDelayAndError(t *testing.T) {
	e, j := newTestEngine(t, Rule{ID: "r", Respond: Respond{Text: "ok"}})
	f := &fakeFaults{}
	e.o.Faults = f

	f.delay = 40 * time.Millisecond
	start := time.Now()
	if r := chat(t, e, user("x")); r.Content != "ok" || time.Since(start) < f.delay {
		t.Errorf("the delay was not honoured: %v %q", time.Since(start), r.Content)
	}
	if f.seen.Turn != "user" || f.seen.Model != "m" {
		t.Errorf("the hook saw %+v", f.seen)
	}

	// A cancelled context ends the wait, long before the delay.
	f.delay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := e.Chat(ctx, user("x"), nil, "m", agentOpts); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("cancelled wait = %v after %v", err, time.Since(start))
	}

	f.delay, f.err = 0, errors.New("llm down")
	before := len(j.entries())
	if _, err := e.Chat(context.Background(), user("x"), nil, "m", agentOpts); err == nil || err.Error() != "llm down" {
		t.Errorf("hook error = %v", err)
	}
	if got := j.payload(t, before); got["error"] != "llm down" {
		t.Errorf("faulted call journalled as %v", got)
	}
}

func TestRecordTruncatesAndRedactsNothingElse(t *testing.T) {
	long := strings.Repeat("é", 3000)
	e, j := newTestEngine(t, Rule{ID: "r", Respond: Respond{Text: long}})
	chat(t, e, user("x"))
	text, _ := j.payload(t, 0)["text"].(string)
	if len(text) > maxJournalText || len(text) < maxJournalText-1 || strings.ContainsRune(text, '�') {
		t.Errorf("journalled text is %d bytes, want a rune-safe cut at %d", len(text), maxJournalText)
	}
	if es := j.entries(); es[0].Kind != "llm.call" || es[0].Subject != "user" {
		t.Errorf("entry = %+v", es[0])
	}
}
