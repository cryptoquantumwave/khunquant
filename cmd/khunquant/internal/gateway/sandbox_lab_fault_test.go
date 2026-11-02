package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// An ordinary gateway has no fault route, no sandbox_fault tool, and no handler wrap on
// any sandbox server it holds.
func TestFaultGroup_NotRegisteredUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/fault"), testSandboxToken, `{"op":"list"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/fault = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_fault"] {
		t.Fatal("an unlocked gateway lists sandbox_fault")
	}
	// The ordinary sandbox server this gateway runs (the one behind /api/sandbox/*): it must
	// exist for the check to mean anything, and it has no handler wrap.
	srv := f.services.SandboxServer
	if srv == nil || !srv.IsRunning() {
		t.Fatal("the unlocked fixture runs no sandbox server, so there is nothing to check")
	}
	if srv.HasHandlerWrap() {
		t.Fatal("an unlocked gateway's sandbox server has a handler wrap")
	}
	if got := sandbox.GetInstance(); got != srv {
		t.Fatalf("GetInstance = %v, want the gateway's own server", got)
	}
}

func exchangeGet(t *testing.T, f *lockedFixture, venue, host, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(f.lab.Server.BaseURL() + "/__sbx__/" + venue + "/" + host + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// On a locked gateway the fault group is a route and a tool, a rule breaks the exchange
// the agent talks to in the venue's own shape, the firing is journaled, list counts it,
// and clear ends it. The injector's holds are a quiescer.
func TestFaultGroup_AddListClearOnALockedGateway(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_fault"] {
		t.Fatal("tools/list has no sandbox_fault")
	}
	if !f.lab.Server.HasHandlerWrap() {
		t.Fatal("a locked lab's server has no handler wrap")
	}
	names := map[string]bool{}
	for _, q := range f.lab.Quiesce.Snapshot() {
		names[q.Name] = true
	}
	if !names["faults"] || !names["exchange_sim"] {
		t.Fatalf("quiescers = %v, want faults and exchange_sim", names)
	}

	const path = "/api/v5/market/tickers?instType=SWAP"
	if code, _ := exchangeGet(t, f, "okx", "www.okx.com", path); code == http.StatusTooManyRequests {
		t.Fatal("429 before any rule")
	}
	code, out := labPost(t, f, "fault", `{"op":"add","match":{"venue":"okx","path_prefix":"/api/v5/market/tickers"},"effect":{"error":"rate_limit"},"when":{"from":"+0m","to":"+2m"}}`)
	if code != http.StatusOK || len(out["ids"].([]any)) != 1 {
		t.Fatalf("add = %d %v", code, out)
	}
	if code, body := exchangeGet(t, f, "okx", "www.okx.com", path); code != http.StatusTooManyRequests || !strings.Contains(body, `"50011"`) {
		t.Fatalf("a matching request = %d %s, want OKX's rate-limit answer", code, body)
	}
	if code, _ := exchangeGet(t, f, "okx", "www.okx.com", "/api/v5/public/time"); code == http.StatusTooManyRequests {
		t.Fatal("a request the rule does not match was broken")
	}

	if code, out = labPost(t, f, "fault", `{"op":"list"}`); code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	rs := out["rules"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["fired"] != float64(1) {
		t.Fatalf("list = %v, want one rule fired once", rs)
	}
	if err := f.lab.Journal.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	fired := journalEntries(t, f.url("/api/sandbox/lab/inspect"), "fault.fired")
	if len(fired) != 1 || fired[0]["venue"] != "okx" || fired[0]["effect"] != "error" || fired[0]["path"] != "/api/v5/market/tickers" || fired[0]["rule_id"] == "" {
		t.Fatalf("fault.fired entries = %v", fired)
	}

	// A rule nobody can honour is refused: an unknown name, an order effect the venue's
	// create answer cannot show (the simulator executes the others), a misspelt key.
	for _, body := range []string{
		`{"op":"add","match":{"venue":"okx"},"effect":{"error":"nope"}}`,
		`{"op":"add","match":{"venue":"okx"},"effect":{"order":"create_reports_fill"}}`,
		`{"op":"add","match":{"venue":"okx","pathprefix":"/x"},"effect":{"status":503}}`,
	} {
		if code, out := labPost(t, f, "fault", body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", body, code, out)
		}
	}

	if code, out = labPost(t, f, "fault", `{"op":"clear"}`); code != http.StatusOK || out["cleared"] != float64(1) {
		t.Fatalf("clear = %d %v", code, out)
	}
	if code, _ := exchangeGet(t, f, "okx", "www.okx.com", path); code == http.StatusTooManyRequests {
		t.Fatal("still 429 after clear")
	}
}

// The model's faults, through the sim provider the agent uses: llm_error fails the matching
// turn only, with an error the providers' classifier reads as the real thing, and llm_latency
// delays the matching call by at least its duration.
func TestBeforeLLM_LatencyAndError(t *testing.T) {
	f := newLockedFixture(t)
	for _, body := range []string{
		`{"op":"add","match":{"venue":"llm","turn":"user"},"effect":{"llm_error":"rate_limit"},"when":{"times":1}}`,
		`{"op":"add","match":{"venue":"llm","turn":"complete","caller":"news"},"effect":{"llm_latency":"60ms"}}`,
	} {
		if code, out := labPost(t, f, "fault", body); code != http.StatusOK {
			t.Fatalf("add %s = %d %v", body, code, out)
		}
	}
	p, _, err := providers.CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/scripted"})
	if err != nil {
		t.Fatal(err)
	}
	turn := tools.WithToolOrigin(tools.WithTurnID(context.Background(), "t-1"), tools.OriginRemote)
	msgs := []providers.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "hi"}}
	opts := map[string]any{"prompt_cache_key": "main"}

	_, err = p.Chat(turn, msgs, nil, "scripted", opts)
	if fe := providers.ClassifyError(err, "sim", "scripted"); err == nil || fe == nil || fe.Reason != providers.FailoverRateLimit {
		t.Fatalf("the matching turn = %v (classified %+v), want a rate-limit failure", err, fe)
	}
	if _, err = p.Chat(turn, msgs, nil, "scripted", opts); err != nil { // times: 1, spent
		t.Fatalf("the second turn = %v, want it to go through", err)
	}
	if _, _, err = providers.Complete(context.Background(), p, "scripted", "an article", providers.CompleteOptions{MaxTokens: 50}); err != nil {
		t.Fatalf("a completion no rule matches = %v", err)
	}
	start := time.Now()
	if _, _, err = providers.Complete(context.Background(), p, "scripted", "an article", providers.CompleteOptions{MaxTokens: 50, NoNativeTools: true}); err != nil {
		t.Fatalf("the delayed completion = %v", err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("the news call took %s, want at least the 60ms latency", d)
	}
	if !f.lab.Faults.Idle() {
		t.Fatal("the injector is busy after the held call returned")
	}
}

// written_then_timeout through the agent's own tool: the simulator creates the Binance
// futures order, the answer never comes, and the tool (which call_tool answers with
// is_error: true) reports a failure while sandbox_account orders shows the one order.
func TestWrittenThenTimeout_ThroughTheFuturesTool(t *testing.T) {
	prev := faultMaxHold
	faultMaxHold = 300 * time.Millisecond
	t.Cleanup(func() { faultMaxHold = prev })
	broker.ResetInstanceCache() // a client cached by an earlier run points at that run's server
	t.Cleanup(broker.ResetInstanceCache)
	f := newLockedFixture(t, ownerConfig, func(c *config.Config) {
		c.Tools.FuturesOpenPosition.Enabled = true
		c.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("sbx-test-api-key-0001"), Secret: *config.NewSecureString("sbx-test-secret-0001")}}
	})
	if code, out := labPost(t, f, "market", `{"op":"set","venue":"binance","symbol":"BTC/USDT","price":98000}`); code != http.StatusOK {
		t.Fatalf("market set = %d %v", code, out)
	}
	// The simulator has no POST /fapi/v1/marginType, which the tool calls first; a fault rule
	// that answers it stands in (a gap of the simulator, not of this PR).
	if code, out := labPost(t, f, "fault", `{"op":"add","match":{"venue":"binance","method":"POST","path_prefix":"/fapi/v1/marginType"},"effect":{"body":{"code":200,"msg":"success"}}}`); code != http.StatusOK {
		t.Fatalf("margin type stand-in = %d %v", code, out)
	}
	if code, out := labPost(t, f, "fault", `{"op":"add","match":{"venue":"binance","order":"create","market":"futures"},"effect":{"order":"written_then_timeout"},"when":{"times":1}}`); code != http.StatusOK {
		t.Fatalf("add = %d %v", code, out)
	}
	code, out := callToolVia(t, f, `{"name":"futures_open_position","as":"owner","args":{"provider":"binance","symbol":"BTC/USDT:USDT","side":"long","leverage":1,"amount":0.001,"confirm":true}}`)
	t.Logf("futures_open_position = %d %v", code, out)
	if code != http.StatusOK || out["is_error"] != true {
		t.Fatalf("the tool answered %d %v, want 200 with is_error: true", code, out)
	}
	code, orders := labPost(t, f, "account", `{"op":"orders","venue":"binance"}`)
	if code != http.StatusOK {
		t.Fatalf("account orders = %d %v", code, orders)
	}
	if list, _ := orders["orders"].([]any); len(list) != 1 {
		t.Fatalf("account orders = %v, want the one order the simulator created", orders)
	}
}
