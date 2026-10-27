package llmops

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func newEngine(t *testing.T) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineOptions{TurnInfo: func(context.Context) sim.CallInfo {
		return sim.CallInfo{Turn: "user", TurnID: "t1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func runOp(t *testing.T, e *sim.Engine, op, args string) (any, error) {
	t.Helper()
	for _, o := range Group(e).Ops {
		if o.Name == op {
			return o.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("no op %q", op)
	return nil, nil
}

func codeOf(err error) string {
	var le *lab.Error
	if errors.As(err, &le) {
		return le.Code
	}
	return ""
}

// ask is an agent turn whose last user message is text.
func ask(t *testing.T, e *sim.Engine, text string) (*protocoltypes.LLMResponse, error) {
	t.Helper()
	msgs := []protocoltypes.Message{{Role: "system", Content: "S"}, {Role: "user", Content: text}}
	return e.Chat(context.Background(), msgs, nil, "m", map[string]any{"prompt_cache_key": "main"})
}

func TestGroupIsValid(t *testing.T) {
	l := &lab.Lab{}
	if err := l.AddGroup(Group(newEngine(t))); err != nil {
		t.Fatalf("AddGroup: %v", err)
	}
}

func TestScriptOps(t *testing.T) {
	e := newEngine(t)

	if res, err := runOp(t, e, "mode", `{"op":"mode"}`); err != nil || res.(map[string]string)["mode"] != "scripted" {
		t.Fatalf("mode = %v, %v", res, err)
	}
	if _, err := runOp(t, e, "mode", `{"set":"replay"}`); codeOf(err) != lab.CodeUnavailable {
		t.Errorf("setting replay = %v, want unavailable", err)
	}

	put := `{"op":"script_put","rules":[
	  {"id":"hello","match":{"turn":"user","prompt_contains":["hi"]},"respond":{"text":"hello"}},
	  {"id":"parse","match":{"complete":"news"},"respond":{"text":"{}"},"times":1}],"fallback":"error"}`
	if _, err := runOp(t, e, "script_put", put); err != nil {
		t.Fatal(err)
	}
	if r, err := ask(t, e, "hi"); err != nil || r.Content != "hello" {
		t.Fatalf("answer = %v, %v", r, err)
	}
	res, err := runOp(t, e, "script_list", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	s := res.(sim.Script)
	if len(s.Rules) != 2 || s.Rules[0].ID != "hello" || s.Rules[0].Hits != 1 || s.Rules[1].Hits != 0 || s.Fallback != "error" {
		t.Errorf("script_list = %+v", s)
	}
	if _, err := ask(t, e, "xyz"); err == nil {
		t.Error("fallback error was not applied")
	}
	if s := e.Script(); s.Unmatched != 1 {
		t.Errorf("unmatched = %d, want 1", s.Unmatched)
	}

	// replace:false appends; a fresh put resets the hit counts.
	if _, err := runOp(t, e, "script_put", `{"replace":false,"rules":[{"id":"extra","respond":{"text":"x"}}]}`); err != nil {
		t.Fatal(err)
	}
	if s := e.Script(); len(s.Rules) != 3 || s.Rules[0].Hits != 0 || s.Unmatched != 0 {
		t.Errorf("after append: %+v", s)
	}
}

func TestScriptPutRejectsBadScripts(t *testing.T) {
	e := newEngine(t)
	if err := e.Put([]sim.Rule{{ID: "keep", Respond: sim.Respond{Text: "kept"}}}, true, ""); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]string{
		"misspelt match field": `{"rules":[{"id":"a","match":{"prompt_contain":["x"]},"respond":{"text":"y"}}]}`,
		"unknown rule field":   `{"rules":[{"id":"a","mach":{},"respond":{"text":"y"}}]}`,
		"duplicate ids":        `{"rules":[{"id":"a","respond":{"text":"y"}},{"id":"a","respond":{"text":"z"}}]}`,
		"unknown turn":         `{"rules":[{"id":"a","match":{"turn":"chat"},"respond":{"text":"y"}}]}`,
		"no rules":             `{}`,
		"rules not a list":     `{"rules":"x"}`,
	} {
		if _, err := runOp(t, e, "script_put", args); codeOf(err) != lab.CodeBadArgs {
			t.Errorf("%s: err = %v, want bad_args", name, err)
		}
	}
	// A refused script leaves the installed one alone.
	if r, err := ask(t, e, "x"); err != nil || r.Content != "kept" {
		t.Errorf("a refused put changed the script: %v, %v", r, err)
	}
}
