//go:build !windows

package sandboxlab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// ownerPatch is the config a scenario's instance.config gives an owner.
func ownerPatch() map[string]any {
	return map[string]any{"proactive": map[string]any{
		"enabled": true,
		"owner":   map[string]any{"channel": "simchat", "chat_id": "dm:owner", "sender_id": "simchat:owner"},
	}}
}

func (h *mcpHarness) logs() string {
	h.t.Helper()
	return h.must("sandbox_instance", map[string]any{"op": "logs", "tail": 100})
}

// op=up carries config_patch, workspace and accounts into the instance's home exactly as a
// scenario's instance block does (RDY-2).
func TestMCPUp_ConfigWorkspaceAccountsReachTheInstance(t *testing.T) {
	h := newHarness(t)
	var info supervisor.Info
	out := h.must("sandbox_instance", map[string]any{
		"op": "up", "fixtures_from": repoFixtures(t),
		"config_patch": ownerPatch(),
		"workspace":    map[string]string{"notes/a.md": "hello"},
		"accounts":     []string{"binance"},
	})
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatal(err, out)
	}

	raw, err := os.ReadFile(filepath.Join(info.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Proactive struct {
			Enabled bool `json:"enabled"`
			Owner   struct {
				ChatID string `json:"chat_id"`
			} `json:"owner"`
		} `json:"proactive"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Proactive.Enabled || cfg.Proactive.Owner.ChatID != "dm:owner" {
		t.Errorf("config_patch did not reach config.json: %s", raw)
	}
	if b, err := os.ReadFile(filepath.Join(info.Home, "workspace", "notes", "a.md")); err != nil || string(b) != "hello" {
		t.Errorf("workspace file = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(info.Home, sandbox.AccountsFile)); err != nil || !strings.Contains(string(b), "binance") {
		t.Errorf("accounts record = %q, %v", b, err)
	}
}

// Every refusal is the supervisor's own, the error the scenario runner gets for the same
// options (it calls Supervisor.Up with them): nothing starts, and the message is the same.
func TestMCPUp_RefusalsAreTheRunnersErrors(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		opts supervisor.UpOptions
	}{
		{"a credential", map[string]any{"config_patch": map[string]any{"exchanges": map[string]any{"binance": map[string]any{"accounts": []any{map[string]any{"name": "x", "api_key": "AKIAREAL"}}}}}},
			supervisor.UpOptions{ConfigPatch: map[string]any{"exchanges": map[string]any{"binance": map[string]any{"accounts": []any{map[string]any{"name": "x", "api_key": "AKIAREAL"}}}}}}},
		{"a lock-owned key", map[string]any{"config_patch": map[string]any{"debug": map[string]any{"sandbox": map[string]any{"enabled": false}}}},
			supervisor.UpOptions{ConfigPatch: map[string]any{"debug": map[string]any{"sandbox": map[string]any{"enabled": false}}}}},
		{"a base URL off this machine", map[string]any{"config_patch": map[string]any{"model_list": []any{map[string]any{"api_base": "https://api.example.com/v1"}}}},
			supervisor.UpOptions{ConfigPatch: map[string]any{"model_list": []any{map[string]any{"api_base": "https://api.example.com/v1"}}}}},
		{"a workspace path outside the workspace", map[string]any{"workspace": map[string]string{"../escape": "x"}},
			supervisor.UpOptions{Workspace: map[string]string{"../escape": "x"}}},
		{"an absolute workspace path", map[string]any{"workspace": map[string]string{"/etc/passwd": "x"}},
			supervisor.UpOptions{Workspace: map[string]string{"/etc/passwd": "x"}}},
		{"a venue with no simulator", map[string]any{"accounts": []string{"kraken"}},
			supervisor.UpOptions{Accounts: []string{"kraken"}}},
		{"a patch over a placeholder account", map[string]any{"accounts": []string{"binance"}, "config_patch": map[string]any{"exchanges": map[string]any{"binance": map[string]any{"accounts": []any{}}}}},
			supervisor.UpOptions{Accounts: []string{"binance"}, ConfigPatch: map[string]any{"exchanges": map[string]any{"binance": map[string]any{"accounts": []any{}}}}}},
		{"live without a budget", map[string]any{"llm": "live", "llm_model": "openai/gpt-x"},
			supervisor.UpOptions{LLM: "live", LLMModel: "openai/gpt-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			args := map[string]any{"op": "up", "fixtures_from": repoFixtures(t)}
			for k, v := range tc.args {
				args[k] = v
			}
			got, isErr := h.call("sandbox_instance", args)
			if !isErr {
				t.Fatalf("up was accepted: %s", got)
			}
			tc.opts.FixturesFrom, tc.opts.Name, tc.opts.ClockMode = repoFixtures(t), "default", "manual"
			_, want := h.sup.Up(context.Background(), tc.opts)
			if want == nil || got != want.Error() {
				t.Errorf("MCP error = %q\nrunner error = %v", got, want)
			}
			if list, _ := h.call("sandbox_instance", map[string]any{"op": "list"}); strings.Contains(list, `"name":"default"`) {
				t.Errorf("a refused up left an instance: %s", list)
			}
		})
	}
}

// op=up defaults to a manual clock (RDY-3), and clock_mode realtime is still available.
func TestMCPUp_ClockDefaultsToManual(t *testing.T) {
	h := newHarness(t)
	h.up()
	if logs := h.logs(); !strings.Contains(logs, "KQ_SANDBOX_CLOCK_MODE=manual") {
		t.Errorf("the default instance did not get a manual clock:\n%s", logs)
	}
	h.must("sandbox_instance", map[string]any{"op": "down"})
	h.must("sandbox_instance", map[string]any{"op": "up", "fixtures_from": repoFixtures(t), "clock_mode": "realtime"})
	if logs := h.logs(); !strings.Contains(logs, "KQ_SANDBOX_CLOCK_MODE=realtime") {
		t.Errorf("clock_mode realtime was not honoured:\n%s", logs)
	}
}

// The tool schema and description tell the caller about the new arguments.
func TestMCPUp_SchemaDescribesTheInstanceBlock(t *testing.T) {
	tool := newHarness(t).tools()["sandbox_instance"]
	b, _ := json.Marshal(tool.InputSchema)
	for _, want := range []string{`"config_patch"`, `"workspace"`, `"accounts"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("sandbox_instance schema lacks %s", want)
		}
	}
	if !strings.Contains(tool.Description, "config_patch") || !strings.Contains(tool.Description, "default manual") {
		t.Errorf("sandbox_instance description does not say what up accepts: %s", tool.Description)
	}
}
