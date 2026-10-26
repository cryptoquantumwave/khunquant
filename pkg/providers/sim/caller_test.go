package sim

import (
	"context"
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

func tool(name string) protocoltypes.ToolDefinition {
	return protocoltypes.ToolDefinition{Type: "function", Function: protocoltypes.ToolFunctionDefinition{Name: name}}
}

func TestClassifyCallers(t *testing.T) {
	e, _ := newTestEngine(t)
	var asked int
	e.o.TurnInfo = func(context.Context) CallInfo {
		asked++
		return CallInfo{Turn: "proactive", Origin: "proactive", TurnID: "t9", Sender: "s"}
	}
	for _, tc := range []struct {
		name    string
		tools   []protocoltypes.ToolDefinition
		options map[string]any
		want    CallInfo
	}{
		{"seahorse", nil, map[string]any{"prompt_cache_key": "seahorse", "max_tokens": 10},
			CallInfo{Turn: "complete", Caller: "seahorse", Tools: []string{}}},
		{"news", nil, map[string]any{"no_native_tools": true},
			CallInfo{Turn: "complete", Caller: "news", Tools: []string{}}},
		{"plain complete", nil, map[string]any{"max_tokens": 10},
			CallInfo{Turn: "complete", Caller: "complete", Tools: []string{}}},
		{"nil options", nil, nil, CallInfo{Turn: "complete", Caller: "complete", Tools: []string{}}},
		{"agent turn", []protocoltypes.ToolDefinition{tool("a"), tool("b")}, map[string]any{"prompt_cache_key": "main"},
			CallInfo{Turn: "proactive", Origin: "proactive", TurnID: "t9", Sender: "s", Tools: []string{"a", "b"}}},
		{"agent turn without tools", nil, map[string]any{"prompt_cache_key": "main"},
			CallInfo{Turn: "proactive", Origin: "proactive", TurnID: "t9", Sender: "s", Tools: []string{}}},
	} {
		asked = 0
		got := e.classify(context.Background(), tc.tools, tc.options)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: classify = %+v, want %+v", tc.name, got, tc.want)
		}
		if wantAsk := tc.want.Turn != "complete"; (asked == 1) != wantAsk {
			t.Errorf("%s: TurnInfo asked %d times", tc.name, asked)
		}
	}

	// No TurnInfo: a turn is "unknown", never a guess.
	e.o.TurnInfo = nil
	if got := e.classify(context.Background(), []protocoltypes.ToolDefinition{tool("a")}, agentOpts); got.Turn != "unknown" {
		t.Errorf("turn without TurnInfo = %q", got.Turn)
	}
}

// A Complete call is journalled as llm.complete and answered by a rule on its caller.
func TestCompleteCallsAreRoutedAndJournalled(t *testing.T) {
	e, j := newTestEngine(t,
		Rule{ID: "parse", Match: Match{Complete: "news"}, Respond: Respond{Text: `{"relevant": false}`}},
		Rule{ID: "sum", Match: Match{Turn: "complete", Complete: "seahorse"}, Respond: Respond{Text: "summary"}},
		Rule{ID: "turns", Match: Match{Turn: "user"}, Respond: Respond{Text: "turn"}},
	)
	msgs := []protocoltypes.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "the article"}}
	r, err := e.Chat(context.Background(), msgs, nil, "m", map[string]any{"no_native_tools": true})
	if err != nil || r.Content != `{"relevant": false}` {
		t.Fatalf("news = %v %v", r, err)
	}
	if r, _ := e.Chat(context.Background(), msgs, nil, "m", map[string]any{"prompt_cache_key": "seahorse"}); r.Content != "summary" {
		t.Errorf("seahorse = %q", r.Content)
	}
	if r, _ := e.Chat(context.Background(), msgs, nil, "m", nil); r.Content != noMatchText {
		t.Errorf("a plain complete matched a rule meant for turns: %q", r.Content)
	}

	es := j.entries()
	if len(es) != 3 || es[0].Kind != "llm.complete" || es[0].Subject != "news" || es[1].Subject != "seahorse" {
		t.Fatalf("journal = %+v", es)
	}
	if p := j.payload(t, 0); p["caller"] != "news" || p["prompt"] != "the article" || p["rule"] != "parse" || p["text"] != `{"relevant": false}` {
		t.Errorf("llm.complete payload = %v", p)
	}
}
