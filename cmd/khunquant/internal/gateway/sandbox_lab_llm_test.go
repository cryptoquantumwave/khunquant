package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// simTurnInfo reads the tags the agent loop really puts on a turn's context (the
// helpers below are the ones loop.go, proactive_turn.go, reflection_turn.go and
// subturn.go call), so a rename or a change of tagging fails here.
func TestClassifyTurnKinds(t *testing.T) {
	bg := context.Background()
	userCtx := tools.WithToolSender(tools.WithToolOrigin(tools.WithTurnID(bg, "turn-1"), tools.OriginForChannel("simchat")), "simchat:owner")

	for _, tc := range []struct {
		name       string
		ctx        context.Context
		turn       string
		origin, id string
		sender     string
	}{
		{"user from a chat", userCtx, "user", tools.OriginRemote, "turn-1", "simchat:owner"},
		{"user from the console", tools.WithToolOrigin(bg, tools.OriginForChannel("cli")), "user", tools.OriginInternal, "", ""},
		{"proactive", tools.WithProactiveTurn(bg, proactive.TurnProactive), "proactive", tools.OriginProactive, "", ""},
		{"reflection", tools.WithProactiveTurn(bg, proactive.TurnReflection), "reflection", tools.OriginProactive, "", ""},
		{"approval follow-up", tools.WithProactiveTurn(bg, proactive.TurnApprovalFollowUp), "approval_followup", tools.OriginProactive, "", ""},
		// A proactive turn started inside a person's context still classifies as proactive.
		{"proactive over a user ctx", tools.WithProactiveTurn(userCtx, proactive.TurnProactive), "proactive", tools.OriginProactive, "turn-1", "simchat:owner"},
		{"legacy heartbeat", tools.WithToolOrigin(bg, tools.OriginHeartbeat), "heartbeat_legacy", tools.OriginHeartbeat, "", ""},
		{"legacy cron", tools.WithToolOrigin(bg, tools.OriginCron), "cron_legacy", tools.OriginCron, "", ""},
		{"sub-turn of a cron job", tools.WithSubTurn(tools.WithToolOrigin(bg, tools.OriginCron)), "subturn", tools.OriginCron, "", ""},
		{"sub-turn of a user turn", tools.WithSubTurn(userCtx), "subturn", tools.OriginRemote, "turn-1", "simchat:owner"},
		{"untagged", bg, "", "", "", ""},
	} {
		got := simTurnInfo(tc.ctx)
		if got.Turn != tc.turn || got.Origin != tc.origin || got.TurnID != tc.id || got.Sender != tc.sender {
			t.Errorf("%s: simTurnInfo = %+v, want turn %q origin %q id %q sender %q", tc.name, got, tc.turn, tc.origin, tc.id, tc.sender)
		}
	}
}

// An ordinary gateway has no llm group, builds no sim model and cannot attach an engine.
func TestLabLLMNotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/llm"), testSandboxToken, `{"op":"script_list"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/llm = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_llm"] {
		t.Fatal("an unlocked gateway lists sandbox_llm")
	}
	e, err := sim.NewEngine(sim.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Attach(e); err == nil {
		t.Fatal("sim.Attach succeeded on an unlocked process")
	}
	if _, _, err := providers.CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/scripted"}); err == nil {
		t.Fatal("sim/scripted built on an unlocked process")
	}
}

func journalEntries(t *testing.T, url, kind string) []map[string]any {
	t.Helper()
	code, body, err := postJSON(url, lockTestToken, `{"op":"journal","kinds":["`+kind+`"]}`)
	if err != nil || code != http.StatusOK {
		t.Fatalf("inspect journal %s = %d (%v) %s", kind, code, err, body)
	}
	var out struct {
		Entries []struct {
			Seq     int64          `json:"seq"`
			Subject string         `json:"subject"`
			Payload map[string]any `json:"payload"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode journal: %v (%s)", err, body)
	}
	var res []map[string]any
	for _, e := range out.Entries {
		e.Payload["subject"], e.Payload["seq"] = e.Subject, e.Seq
		delete(e.Payload, "tools")
		res = append(res, e.Payload)
	}
	return res
}

// The locked gateway attaches the engine with the lab: the llm group scripts it, the
// model factory's sim provider answers from the script (and the journal says which rule
// and which kind of turn), and a reload keeps both the script and the attachment.
func TestLabLLMScriptsTheFactoryModel(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	llmURL, inspectURL := f.url("/api/sandbox/lab/llm"), f.url("/api/sandbox/lab/inspect")
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_llm"] {
		t.Fatal("tools/list of the first generation has no sandbox_llm")
	}

	put := `{"op":"script_put","rules":[
	  {"id":"hello","match":{"turn":"user","prompt_contains":["hi"]},"respond":{"text":"hello"}},
	  {"id":"parse","match":{"complete":"news"},"respond":{"text":"{\"relevant\":false}"}}]}`
	if code, body, err := postJSON(llmURL, lockTestToken, put); err != nil || code != http.StatusOK {
		t.Fatalf("script_put = %d (%v) %s", code, err, body)
	}
	if code, body, _ := postJSON(llmURL, lockTestToken, `{"op":"script_put","rules":[{"id":"a","match":{"nope":1},"respond":{"text":"x"}}]}`); code != http.StatusBadRequest {
		t.Errorf("a script with an unknown match field = %d %s, want 400", code, body)
	}

	// Build the model the way the agent does, from the profile's model entry.
	p, _, err := providers.CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/scripted"})
	if err != nil {
		t.Fatal(err)
	}
	turn := tools.WithToolOrigin(tools.WithTurnID(context.Background(), "t-1"), tools.OriginRemote)
	msgs := []providers.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "hi there"}}
	resp, err := p.Chat(turn, msgs, nil, "scripted", map[string]any{"prompt_cache_key": "main"})
	if err != nil || resp.Content != "hello" {
		t.Fatalf("agent turn = %v, %v", resp, err)
	}
	text, _, err := providers.Complete(context.Background(), p, "scripted", "an article", providers.CompleteOptions{MaxTokens: 50, NoNativeTools: true})
	if err != nil || text != `{"relevant":false}` {
		t.Fatalf("Complete = %q, %v", text, err)
	}

	calls := journalEntries(t, inspectURL, "llm.call")
	if len(calls) != 1 || calls[0]["turn"] != "user" || calls[0]["rule"] != "hello" || calls[0]["origin"] != "remote" || calls[0]["turn_id"] != "t-1" {
		t.Fatalf("llm.call entries = %v", calls)
	}
	if done := journalEntries(t, inspectURL, "llm.complete"); len(done) != 1 || done[0]["caller"] != "news" || done[0]["rule"] != "parse" || done[0]["subject"] != "news" {
		t.Fatalf("llm.complete entries = %v", done)
	}

	// A reload builds a new generation; the engine, the group and the script stay.
	if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_llm"] {
		t.Fatal("tools/list after the reload has no sandbox_llm")
	}
	if code, body, err := postJSON(llmURL, lockTestToken, `{"op":"script_list"}`); err != nil || code != http.StatusOK ||
		!strings.Contains(body, `"id":"hello"`) || !strings.Contains(body, `"hits":1`) {
		t.Fatalf("script_list after the reload = %d (%v) %s", code, err, body)
	}
	if resp, err := p.Chat(turn, msgs, nil, "scripted", map[string]any{"prompt_cache_key": "main"}); err != nil || resp.Content != "hello" {
		t.Fatalf("after the reload: %v, %v", resp, err)
	}
}

// A new lab (the next test, or the next process) can attach again: closing the lab
// detaches the engine.
func TestLabClosingDetachesTheEngine(t *testing.T) {
	f := newLockedFixture(t)
	p, err := sim.NewProvider(sim.ModeScripted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), nil, nil, "m", nil); err != nil && strings.Contains(err.Error(), "no engine") {
		t.Fatalf("the engine is not attached while the lab runs: %v", err)
	}
	if err := f.lab.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), nil, nil, "m", nil); err == nil || !strings.Contains(err.Error(), "no engine") {
		t.Fatalf("after the lab closed the provider = %v, want it to fail closed", err)
	}
}

// The acceptance check of SBX-04, on the real path: the profile `sandbox up` writes
// (model sim/scripted), a re-executed locked gateway, a rule scripted over HTTP, and an
// owner DM answered by that rule. Nothing is stubbed.
func TestLockedGatewayAnswersFromScript(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "")
	port := freePort(t)
	if _, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: port}); err != nil {
		t.Fatal(err)
	}

	childHome, childTmp := t.TempDir(), t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TMPDIR=" + childTmp, "HOME=" + childHome,
		"KHUNQUANT_HOME=" + home, "KHUNQUANT_CONFIG=" + home + "/config.json",
		"KQ_SANDBOX_LOCKED=1", "KQ_SANDBOX_TOKEN=" + lockTestToken, "KQ_SANDBOX_INSTANCE=subprocess",
	}
	p := launchLockedProcess(t, home, port, env, []string{home, childHome, childTmp})
	p.waitReady()
	p.waitOutput("Channels enabled: [simchat]", 20*time.Second)

	base := fmt.Sprintf("http://127.0.0.1:%d/api/sandbox/lab/", port)
	call := func(group, body string) string {
		t.Helper()
		code, out, err := postJSON(base+group, lockTestToken, body)
		if err != nil || code != http.StatusOK {
			t.Fatalf("POST %s %s = %d (%v) %s\n%s", group, body, code, err, out, p.out.String())
		}
		return out
	}
	call("llm", `{"op":"script_put","rules":[{"id":"greet","match":{"turn":"user"},"respond":{"text":"hello"}}]}`)
	call("chat", `{"op":"send","as":"owner","text":"hi"}`)

	msgs := waitChatOutbox(t, base+"chat", "dm:owner", 1)
	if txt, _ := msgs[0]["text"].(string); txt != "hello" {
		t.Fatalf("the owner's reply = %v, want \"hello\"\n%s", msgs[0], p.out.String())
	}
	calls := journalEntries(t, base+"inspect", "llm.call")
	if len(calls) != 1 || calls[0]["turn"] != "user" || calls[0]["rule"] != "greet" || calls[0]["sender"] != "simchat:owner" {
		b, _ := json.MarshalIndent(calls, "", " ")
		t.Fatalf("llm.call entries = %s, want exactly one user turn answered by greet\n%s", b, p.out.String())
	}
	if list := call("llm", `{"op":"script_list"}`); !strings.Contains(list, `"hits":1`) {
		t.Errorf("script_list = %s, want the rule hit once", list)
	}
	// The script is on disk in the instance home, where a restart finds it.
	if b, err := os.ReadFile(filepath.Join(home, "lab", "llm_script.json")); err != nil || !strings.Contains(string(b), `"greet"`) {
		t.Errorf("saved script = %q, %v", b, err)
	}
}
