package llmops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

func replayEngine(t *testing.T) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineOptions{Mode: sim.ModeReplay, TurnInfo: func(context.Context) sim.CallInfo {
		return sim.CallInfo{Turn: "user", TurnID: "t1"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func askMsgs(text string) []protocoltypes.Message {
	return []protocoltypes.Message{{Role: "system", Content: "S"}, {Role: "user", Content: text}}
}

var askOpts = map[string]any{"prompt_cache_key": "main"}

// entryLine is the cassette line that answers askMsgs(text) with reply.
func entryLine(t *testing.T, text, reply string) string {
	t.Helper()
	req := sim.Canonicalize("", askMsgs(text), nil, askOpts)
	b, err := json.Marshal(sim.Entry{Key: sim.Key(req), Request: req, Response: sim.Reply{Content: reply, FinishReason: "stop"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runCassette(t *testing.T, e *sim.Engine, home, args string) (any, error) {
	t.Helper()
	for _, o := range Group(e, home).Ops {
		if o.Name == "cassette_load" {
			return o.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatal("no cassette_load op")
	return nil, nil
}

func TestCassetteLoadFromFileAndInline(t *testing.T) {
	home := t.TempDir()
	e := replayEngine(t)
	if err := os.WriteFile(filepath.Join(home, "c.jsonl"), []byte(entryLine(t, "hi", "from file")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := runCassette(t, e, home, `{"path":"c.jsonl"}`)
	if err != nil || res.(map[string]any)["entries"] != 1 || res.(map[string]any)["miss"] != "error" {
		t.Fatalf("path load = %v, %v", res, err)
	}
	if r, err := e.Chat(context.Background(), askMsgs("hi"), nil, "m", askOpts); err != nil || r.Content != "from file" {
		t.Fatalf("replayed = %v, %v", r, err)
	}
	inline := `{"inline":[` + entryLine(t, "yo", "inline") + `],"miss":"scripted"}`
	if res, err = runCassette(t, e, home, inline); err != nil || res.(map[string]any)["miss"] != "scripted" {
		t.Fatalf("inline load = %v, %v", res, err)
	}
	if r, err := e.Chat(context.Background(), askMsgs("yo"), nil, "m", askOpts); err != nil || r.Content != "inline" {
		t.Fatalf("replayed inline = %v, %v", r, err)
	}
}

func TestCassetteLoadRefusals(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	e := replayEngine(t)
	if err := os.WriteFile(filepath.Join(outside, "c.jsonl"), []byte(entryLine(t, "hi", "x")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "c.jsonl"), filepath.Join(home, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]string{
		"an absolute path outside the home": `{"path":"` + filepath.Join(outside, "c.jsonl") + `"}`,
		"a dot-dot path":                    `{"path":"../` + filepath.Base(outside) + `/c.jsonl"}`,
		"a symlink out of the home":         `{"path":"link.jsonl"}`,
		"a missing file":                    `{"path":"nope.jsonl"}`,
		"neither path nor inline":           `{}`,
		"both path and inline":              `{"path":"a","inline":[{}]}`,
		"an unknown miss policy":            `{"inline":[` + entryLine(t, "a", "b") + `],"miss":"guess"}`,
		"an entry with a wrong key":         `{"inline":[{"key":"k","request":{"messages":[{"role":"user","content":"x"}]},"response":{}}]}`,
	} {
		if _, err := runCassette(t, e, home, args); codeOf(err) != lab.CodeBadArgs {
			t.Errorf("%s: err = %v, want bad_args", name, err)
		}
	}
	if n, _ := e.CassetteStat(); n != 0 {
		t.Errorf("a refused load left %d entries", n)
	}
	if _, err := runCassette(t, e, "", `{"path":"/etc/hosts"}`); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Errorf("no home: %v", err)
	}
}

func TestModeOpIsFixedAtStart(t *testing.T) {
	e := replayEngine(t)
	for _, o := range Group(e, "").Ops {
		if o.Name != "mode" {
			continue
		}
		if res, err := o.Run(context.Background(), json.RawMessage(`{"set":"replay"}`)); err != nil || res.(map[string]string)["mode"] != "replay" {
			t.Errorf("restating the mode = %v, %v", res, err)
		}
		if _, err := o.Run(context.Background(), json.RawMessage(`{"set":"scripted"}`)); codeOf(err) != lab.CodeUnavailable {
			t.Errorf("switching the mode = %v, want unavailable", err)
		}
	}
}
