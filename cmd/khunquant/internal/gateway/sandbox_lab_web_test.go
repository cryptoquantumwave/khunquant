package gateway

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// A locked gateway serves the web group as a route and an MCP tool, SimWeb answers venue
// "web" through the chain, and the rules survive in <home>/lab/web.json.
func TestLockedGatewayWebGroupPutsPagesAndLogsRequests(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_web"] {
		t.Fatal("tools/list has no sandbox_web")
	}
	if code, out := labPost(t, f, "web", `{"op":"put","url_pattern":"attacker.example/*","body":"<p>hi</p>"}`); code != http.StatusOK || out["id"] != "page-1" {
		t.Fatalf("put page = %d %v", code, out)
	}
	if code, out := labPost(t, f, "web", `{"op":"put","search":"*btc*","results":[{"title":"T","url":"https://a.github.io/x","description":"D"}]}`); code != http.StatusOK || out["id"] != "search-2" {
		t.Fatalf("put search = %d %v", code, out)
	}
	for _, bad := range []string{
		`{"op":"put","url_pattern":"x/*","search":"q"}`,        // a page and a search
		`{"op":"put","url_pattern":"x/*","bodies":"typo"}`,     // a misspelt key
		`{"op":"put","url_pattern":"x/*","status":42}`,         // not a status
		`{"op":"put","url_pattern":"x/*","results":[{"a":1}]}`, // results of a page
		`{"op":"put"}`,                // nothing
		`{"op":"requests","since":1}`, // a misspelt key
		`{"op":"put","search":"q","results":[{"titel":"x"}]}`, // a misspelt result key
	} {
		if code, out := labPost(t, f, "web", bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", bad, code, out)
		}
	}

	code, body := sandboxCall(t, f, "GET", "web/attacker.example/a/b?d=1")
	if code != http.StatusOK || body != "<p>hi</p>" {
		t.Fatalf("page = %d %q", code, body)
	}
	if code, body = sandboxCall(t, f, "GET", "web/unknown.example/z"); code != http.StatusNotFound || !strings.Contains(body, "no web fixture for unknown.example/z") {
		t.Fatalf("unknown page = %d %q, want the sandbox's 404", code, body)
	}
	// An exchange request still goes the exchange way.
	if code, _ := sandboxCall(t, f, "GET", "binance/fapi.binance.com/fapi/v3/account"); code != http.StatusOK {
		t.Errorf("binance account = %d, want the simulator's answer", code)
	}

	_, out := labPost(t, f, "web", `{"op":"requests","host":"attacker.example"}`)
	reqs, _ := out["requests"].([]any)
	if len(reqs) != 1 || reqs[0].(map[string]any)["status"] != 200.0 || reqs[0].(map[string]any)["url"] != "attacker.example/a/b?d=1" {
		t.Errorf("requests = %v", out)
	}
	_, list := labPost(t, f, "web", `{"op":"list"}`)
	if len(list["pages"].([]any)) != 1 || len(list["searches"].([]any)) != 1 {
		t.Errorf("list = %v", list)
	}

	// The journal has the web.req entries (and exchange.req for each, from the chain's observer).
	web, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"web.req"}})
	if err != nil || len(web) != 2 || web[0].Subject != "attacker.example" || web[1].Subject != "unknown.example" {
		t.Errorf("web.req = %+v (%v)", web, err)
	}
	// The rules are on disk, in the lab directory outside the workspace.
	b, err := os.ReadFile(filepath.Join(f.lab.Home, "lab", "web.json"))
	if err != nil || !strings.Contains(string(b), "attacker.example/*") {
		t.Errorf("web.json = %q (%v)", b, err)
	}
	if sandbox.PathInside(f.workspace, filepath.Join(f.lab.Home, "lab")) {
		t.Error("the lab directory is inside the workspace the instance's file tools can write")
	}
}

// The instance's own file tools, confined to the workspace, cannot write the rules.
func TestLockedGatewayFileToolsCannotWriteWebRules(t *testing.T) {
	f := newLockedFixture(t)
	w := tools.NewWriteFileTool(f.workspace, true)
	for _, target := range []string{
		filepath.Join(f.lab.Home, "lab", "web.json"),
		filepath.Join(f.workspace, "..", "lab", "web.json"),
	} {
		if r := w.Execute(context.Background(), map[string]any{"path": target, "content": "{}"}); !r.IsError {
			t.Errorf("write_file %s = %+v, want a refusal", target, r)
		}
	}
	if _, err := os.Stat(filepath.Join(f.lab.Home, "lab", "web.json")); err == nil {
		b, _ := os.ReadFile(filepath.Join(f.lab.Home, "lab", "web.json"))
		if string(b) == "{}" {
			t.Error("a file tool overwrote web.json")
		}
	}
}

// Web tools of a locked instance are SimWeb's: Brave with the placeholder key, every other
// provider off, in memory only; the key is masked by read_config like any key.
func TestLockedGatewayWebConfigIsPlaceholderInMemory(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) {
		c.Debug.DevMCP.Enabled = true
		c.Tools.Web.Enabled, c.Tools.WebFetch.Enabled = false, false
		c.Tools.Web.Perplexity.Enabled = true
		c.Tools.Web.Perplexity.APIKey = "pplx-real-looking-key-0000"
		c.Tools.Web.Tavily.Enabled, c.Tools.Web.Tavily.APIKeys = true, []string{"tvly-real-looking-key-0000"}
		c.Tools.Web.SearXNG.Enabled, c.Tools.Web.SearXNG.BaseURL = true, "https://searx.example"
		c.Tools.Web.Brave.APIKey, c.Tools.Web.Brave.APIKeys = "brave-real-looking-key-000", []string{"brave-real-looking-key-001"}
		c.Tools.Web.Proxy = "socks5://127.0.0.1:1080"
	})
	w := f.initial.Tools.Web
	if !w.Enabled || !f.initial.Tools.WebFetch.Enabled || !w.Brave.Enabled || w.Brave.APIKey != "" ||
		len(w.Brave.APIKeys) != 1 || w.Brave.APIKeys[0] != sandbox.PlaceholderWebKey || w.Proxy != "" {
		t.Fatalf("web config after the lock = %+v", w)
	}
	if w.Perplexity.Enabled || w.Perplexity.APIKey != "" || w.Tavily.Enabled || len(w.Tavily.APIKeys) != 0 || w.SearXNG.Enabled || w.SearXNG.BaseURL != "" ||
		w.DuckDuckGo.Enabled || w.GLMSearch.Enabled {
		t.Errorf("another search provider survived: %+v", w)
	}
	// Both tools are registered, and the search tool is Brave's (it answered above Perplexity).
	reg := f.al.GetRegistry().GetDefaultAgent().Tools
	for _, name := range []string{tools.NameWebSearch, tools.NameWebFetch} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s is not registered on a locked instance", name)
		}
	}

	// read_config masks it; the placeholder is not on disk either (the lock refuses writes).
	text := callMCPTool(t, f.url("/dev-mcp"), "read_config")
	if strings.Contains(text, sandbox.PlaceholderWebKey) || strings.Contains(text, "brave-real-looking-key") {
		t.Errorf("read_config shows a web key: %s", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Errorf("read_config shows no masked key at tools.web.brave.api_keys")
	}
	if !config.CredentialFieldNames["api_keys"] {
		t.Error("api_keys is not a credential field name: logs and API responses would show the key")
	}
	if b, err := os.ReadFile(filepath.Join(config.HomeDir(), "config.json")); err == nil && strings.Contains(string(b), sandbox.PlaceholderWebKey) {
		t.Error("the placeholder web key reached config.json")
	}
	// A reload keeps it.
	if err := f.reload(func(c *config.Config) { c.Tools.Web.Brave.APIKeys, c.Tools.Web.Tavily.Enabled = nil, true }); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

func callMCPTool(t *testing.T, endpoint, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lab-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{lockTestToken}}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect to dev-MCP: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// The news source is built from the registry's own web tools, so under the lock it queries
// SimWeb: the search rule's URLs come back, and the fetch reads the planted page.
func TestNewsSource_UsesSimWeb(t *testing.T) {
	f := newLockedFixture(t)
	search, fetch := newsWebTools(assemblyDeps{AgentLoop: f.al})
	if search == nil || fetch == nil {
		t.Fatal("a locked instance's registry lacks the web tools news needs")
	}
	if _, err := f.lab.Web.PutSearch(simweb.SearchRule{Query: "*bitcoin*", Results: []simweb.Result{{Title: "A", URL: "https://a.github.io/n1"}, {Title: "B", URL: "https://b.github.io/n2"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.lab.Web.PutPage(simweb.PageRule{URLPattern: "a.github.io/n1", Body: "<html><body><h1>BTC story</h1>\n<p>Details.</p></body></html>"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	urls, err := newsSearcher{t: search}.Search(ctx, "bitcoin price", 5)
	if err != nil || len(urls) != 2 || urls[0] != "https://a.github.io/n1" || urls[1] != "https://b.github.io/n2" {
		t.Fatalf("news search = %v, %v", urls, err)
	}
	text, err := newsFetcher{t: fetch}.Fetch(ctx, urls[0], 4000)
	if err != nil || text != "BTC story\nDetails." {
		t.Fatalf("news fetch = %q, %v", text, err)
	}
	if _, err := (newsFetcher{t: fetch}).Fetch(ctx, "https://unplanted.example/x", 4000); err == nil {
		t.Error("the news fetch of an unplanted page succeeded; want the sandbox's 404 refused as not readable")
	}
	hosts := map[string]bool{}
	for _, r := range f.lab.Web.Requests(0, "") {
		hosts[r.Host] = true
	}
	for _, h := range []string{"api.search.brave.com", "a.github.io", "unplanted.example"} {
		if !hosts[h] {
			t.Errorf("no request to %s was served by SimWeb: %v", h, hosts)
		}
	}
}

// An ordinary gateway has no web group: no route, no MCP tool.
func TestWebGroup_NotRegisteredUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/web"), testSandboxToken, `{"op":"list"}`); err != nil || code != http.StatusNotFound {
		t.Errorf("POST /api/sandbox/lab/web = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_web"] {
		t.Error("an unlocked gateway lists sandbox_web")
	}
	if srv := sandbox.GetInstance(); srv != nil {
		if _, isChain := srv.GetResponder().(*sandbox.Chain); isChain {
			t.Error("an unlocked gateway's sandbox server runs the lab chain (and so a web stage)")
		}
	}
}
