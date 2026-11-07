package sim

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// entryFor builds the cassette entry that answers msgs.
func entryFor(msgs []protocoltypes.Message, resp *protocoltypes.LLMResponse) Entry {
	req := Canonicalize("", msgs, nil, agentOpts)
	return Entry{Key: Key(req), Request: req, Response: ReplyOf(resp), Usage: resp.Usage}
}

func newReplayEngine(t *testing.T, entries []Entry, miss string) (*Engine, *memJournal) {
	t.Helper()
	j := &memJournal{}
	e, err := NewEngine(EngineOptions{Journal: j, Mode: ModeReplay, TurnInfo: func(context.Context) CallInfo {
		return CallInfo{Turn: "user", Origin: "remote", TurnID: "t1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		if err := e.LoadCassette(entries, miss); err != nil {
			t.Fatal(err)
		}
	}
	return e, j
}

func TestReplayMissFailsClosed(t *testing.T) {
	known := user("known")
	e, j := newReplayEngine(t, []Entry{entryFor(known, &protocoltypes.LLMResponse{Content: "hi", FinishReason: "stop"})}, "")
	if r := chat(t, e, known); r.Content != "hi" {
		t.Fatalf("hit answered %q", r.Content)
	}
	_, err := e.Chat(context.Background(), user("unknown"), nil, "m", agentOpts)
	want := Key(Canonicalize("", user("unknown"), nil, agentOpts))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a miss must fail and name its key %s: %v", want, err)
	}
	p := j.payload(t, 1)
	if p["cassette_miss"] != true || p["key"] != want || p["error"] == nil {
		t.Errorf("journal payload of the miss = %v", p)
	}
	if p0 := j.payload(t, 0); p0["cassette_miss"] != nil || p0["key"] == "" {
		t.Errorf("a hit must carry its key and no miss mark: %v", p0)
	}
}

func TestReplayNoCassetteFailsClosed(t *testing.T) {
	e, _ := newReplayEngine(t, nil, "")
	if _, err := e.Chat(context.Background(), user("x"), nil, "m", agentOpts); err == nil || !strings.Contains(err.Error(), "no cassette") {
		t.Fatalf("replay with no cassette = %v", err)
	}
}

func TestReplayMissScriptedFallsBackAndMarks(t *testing.T) {
	e, j := newReplayEngine(t, []Entry{entryFor(user("known"), &protocoltypes.LLMResponse{Content: "hi", FinishReason: "stop"})}, MissScripted)
	if err := e.Put([]Rule{{ID: "r", Match: Match{PromptContains: []string{"other"}}, Respond: Respond{Text: "scripted"}}}, true, ""); err != nil {
		t.Fatal(err)
	}
	if r := chat(t, e, user("other")); r.Content != "scripted" {
		t.Fatalf("miss: scripted answered %q", r.Content)
	}
	if p := j.payload(t, 0); p["cassette_miss"] != true {
		t.Errorf("a scripted fallback is still a miss in the journal: %v", p)
	}
}

func TestReplayRemintsCallIDsAndRepeatsInOrder(t *testing.T) {
	msgs := user("go")
	call := &protocoltypes.LLMResponse{FinishReason: "tool_calls", ToolCalls: []protocoltypes.ToolCall{
		{ID: "toolu_REAL", Name: "read_file", Arguments: map[string]any{"path": "a"}},
	}}
	e, _ := newReplayEngine(t, []Entry{entryFor(msgs, call), entryFor(msgs, &protocoltypes.LLMResponse{Content: "second", FinishReason: "stop"})}, "")
	r1 := chat(t, e, msgs)
	if len(r1.ToolCalls) != 1 || r1.ToolCalls[0].Name != "read_file" || r1.ToolCalls[0].Arguments["path"] != "a" ||
		!strings.HasPrefix(r1.ToolCalls[0].ID, toolIDPrefix) || r1.ToolCalls[0].ID == "toolu_REAL" ||
		r1.ToolCalls[0].Function == nil || !strings.Contains(r1.ToolCalls[0].Function.Arguments, `"path"`) {
		t.Fatalf("first answer = %+v", r1)
	}
	for i := range 2 { // the second entry answers, then repeats
		if r := chat(t, e, msgs); r.Content != "second" {
			t.Fatalf("call %d answered %q", i+2, r.Content)
		}
	}
	// A new cassette starts from its first entry again.
	if err := e.LoadCassette([]Entry{entryFor(msgs, &protocoltypes.LLMResponse{Content: "fresh", FinishReason: "stop"})}, ""); err != nil {
		t.Fatal(err)
	}
	if r := chat(t, e, msgs); r.Content != "fresh" {
		t.Errorf("after reload answered %q", r.Content)
	}
}

func TestCassetteRefusesEditedEntry(t *testing.T) {
	e := entryFor(user("x"), &protocoltypes.LLMResponse{Content: "y"})
	e.Request.Messages[1].Content = "edited"
	if _, err := NewCassette([]Entry{e}, ""); err == nil || !strings.Contains(err.Error(), "not the key") {
		t.Errorf("an entry whose request was edited = %v", err)
	}
	if _, err := ParseEntries(strings.NewReader(`{"key":"k"}` + "\n")); err == nil {
		t.Error("an entry with no request must be refused")
	}
	if _, err := ParseEntries(strings.NewReader(`{"key":"k","bogus":1}` + "\n")); err == nil {
		t.Error("an unknown field must be refused")
	}
	if _, err := NewCassette(nil, "sometimes"); err == nil {
		t.Error("an unknown miss policy must be refused")
	}
}

func TestProviderModeMustMatchInstance(t *testing.T) {
	lockForTest(t)
	attached.Store(nil)
	e, _ := newReplayEngine(t, nil, "")
	if err := Attach(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Detach(e) })
	p, err := NewProvider(ModeScripted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), user("x"), nil, "m", agentOpts); err == nil || !strings.Contains(err.Error(), "sim/replay") {
		t.Errorf("a sim/scripted provider on a replay instance = %v", err)
	}
	pr, err := NewProvider(ModeReplay)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pr.Chat(context.Background(), user("x"), nil, "m", agentOpts); err == nil || !strings.Contains(err.Error(), "cassette") {
		t.Errorf("a replay provider reaches the engine, which has no cassette: %v", err)
	}
}

func TestScriptedCallsJournalTheirKey(t *testing.T) {
	e, j := newTestEngine(t, Rule{ID: "r", Respond: Respond{Text: "ok"}})
	chat(t, e, user("hello"))
	if p := j.payload(t, 0); p["key"] != Key(Canonicalize("", user("hello"), nil, agentOpts)) {
		t.Errorf("llm.call key = %v", p["key"])
	}
}

func TestParseRef(t *testing.T) {
	for in, want := range map[string][2]string{"replay": {"replay", ""}, "live:anthropic-messages/m": {"live", "anthropic-messages/m"}, "": {"", ""}} {
		if m, i := ParseRef(in); m != want[0] || i != want[1] {
			t.Errorf("ParseRef(%q) = %q, %q", in, m, i)
		}
	}
	if Ref("replay", "") != "sim/replay" || Ref("live", "p/m") != "sim/live:p/m" {
		t.Error("Ref")
	}
}
