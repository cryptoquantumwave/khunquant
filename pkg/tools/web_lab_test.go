package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/fault"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simweb"
)

type webJournal struct {
	mu sync.Mutex
	es []labjournal.Entry
}

func (j *webJournal) Append(e labjournal.Entry) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.es = append(j.es, e)
}

// kind returns the journaled entries of one kind, decoded.
func (j *webJournal) kind(k string) []map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []map[string]any
	for _, e := range j.es {
		if e.Kind != k {
			continue
		}
		m := map[string]any{}
		_ = json.Unmarshal(e.Payload, &m)
		out = append(out, m)
	}
	return out
}

type webRig struct {
	srv   *sandbox.Server
	web   *simweb.Store
	j     *webJournal
	chain *sandbox.Chain
}

// lockedWebRig locks this test process (undone at cleanup) and serves SimWeb on a real
// sandbox server, behind the same chain the lab uses.
func lockedWebRig(t *testing.T) *webRig {
	t.Helper()
	if err := sandbox.Lock(sandbox.LockOptions{InstanceName: "t", Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sandbox.ResetLockForTest)
	j := &webJournal{}
	web, err := simweb.Open(simweb.Options{Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	chain := sandbox.NewChain(sandbox.NewStore())
	chain.Set(sandbox.StageWeb, web.Middleware())
	srv := sandbox.NewServer(sandbox.NewStore())
	srv.SetResponder(chain)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return &webRig{srv: srv, web: web, j: j, chain: chain}
}

// dialLog records every address the web tools' transport dials, and fails any DNS lookup.
type dialLog struct {
	addrs   []string
	lookups atomic.Int32
}

// trackDials wraps the dialer under the RoundTripper of client and makes the process's
// resolver count (and fail) every lookup.
func trackDials(t *testing.T, c *http.Client) *dialLog {
	t.Helper()
	rt, ok := c.Transport.(*sandbox.RoundTripper)
	if !ok {
		t.Fatalf("locked client transport is %T, want *sandbox.RoundTripper", c.Transport)
	}
	tr, ok := rt.Underlying.(*http.Transport)
	if !ok {
		t.Fatalf("underlying transport is %T, want *http.Transport", rt.Underlying)
	}
	d := &dialLog{}
	inner := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d.addrs = append(d.addrs, addr)
		return inner(ctx, network, addr)
	}
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		d.lookups.Add(1)
		return nil, errors.New("test: no DNS lookup is allowed")
	}}
	t.Cleanup(func() { net.DefaultResolver = old })
	return d
}

func fetchTool(t *testing.T) *WebFetchTool {
	t.Helper()
	f, err := NewWebFetchToolWithProxy(0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// page decodes a web_fetch answer.
func page(t *testing.T, r *ToolResult) (status int, text string) {
	t.Helper()
	var p struct {
		Status int    `json:"status"`
		Text   string `json:"text"`
	}
	if r.IsError {
		t.Fatalf("web_fetch failed: %s", r.ForLLM)
	}
	if err := json.Unmarshal([]byte(r.ForLLM), &p); err != nil {
		t.Fatalf("web_fetch answer %q: %v", r.ForLLM, err)
	}
	return p.Status, p.Text
}

func TestWebFetch_LockedServesFixture(t *testing.T) {
	rig := lockedWebRig(t)
	if _, err := rig.web.PutPage(simweb.PageRule{URLPattern: "docs.example/guide/*", Body: "<html><body><h1>Guide</h1>\n<script>x()</script><p>Fixture text.</p></body></html>"}); err != nil {
		t.Fatal(err)
	}
	status, text := page(t, fetchTool(t).Execute(context.Background(), map[string]any{"url": "https://docs.example/guide/a?x=1"}))
	if status != 200 || text != "Guide\nFixture text." {
		t.Fatalf("status %d text %q, want the fixture through the HTML extraction", status, text)
	}
	if got := rig.j.kind("web.req"); len(got) != 1 || got[0]["host"] != "docs.example" || got[0]["status"] != 200.0 || got[0]["rule_id"] != "page-1" {
		t.Errorf("web.req = %v", got)
	}
}

// An address no rule matches is a 404 of the sandbox's own. web_fetch (as unlocked) reports
// a 404 in the status field rather than as an error. Nothing is dialled but the sandbox
// server, by IP literal, and nothing is resolved.
func TestWebFetch_LockedUnknownURL404(t *testing.T) {
	rig := lockedWebRig(t)
	tool := fetchTool(t)
	dials := trackDials(t, tool.client)

	status, text := page(t, tool.Execute(context.Background(), map[string]any{"url": "https://never-resolved.invalid/x/y?d=1"}))
	if status != 404 || !strings.Contains(text, "sandbox: no web fixture for never-resolved.invalid/x/y") {
		t.Fatalf("status %d text %q, want the sandbox's 404", status, text)
	}
	if got := rig.j.kind("web.req"); len(got) != 1 || got[0]["host"] != "never-resolved.invalid" || got[0]["status"] != 404.0 {
		t.Errorf("web.req = %v, want one 404", got)
	}
	if len(dials.addrs) == 0 {
		t.Fatal("the recorder saw no dial: the proof below would be vacuous")
	}
	for _, a := range dials.addrs {
		if a != rig.srv.Addr() {
			t.Errorf("dialled %q, want only the sandbox server %q", a, rig.srv.Addr())
		}
	}
	if n := dials.lookups.Load(); n != 0 {
		t.Errorf("%d DNS lookups, want 0", n)
	}
}

// Under the lock the dialer is the loopback-only egress one, not the SSRF-safe one, and
// the transport has no proxy: a target the rewrite did not reach would be refused.
func TestWebFetch_LockedDialerIsLoopbackOnly(t *testing.T) {
	lockedWebRig(t)
	tool := fetchTool(t)
	tr := tool.client.Transport.(*sandbox.RoundTripper).Underlying.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("the locked transport has a proxy")
	}
	if name := funcName(tr.DialContext); !strings.Contains(name, "egress.") || strings.Contains(name, "newSafeDialContext") {
		t.Errorf("locked dialer is %s, want the egress guard", name)
	}
	for _, addr := range []string{"203.0.113.9:443", "example.com:443", "10.0.0.1:80"} {
		if _, err := tr.DialContext(context.Background(), "tcp", addr); !errors.Is(err, egress.ErrBlocked) {
			t.Errorf("dial %s = %v, want egress.ErrBlocked", addr, err)
		}
	}
}

func TestWebFetch_LockedRedirectStaysInSandbox(t *testing.T) {
	rig := lockedWebRig(t)
	rig.web.PutPage(simweb.PageRule{URLPattern: "short.example/go", Status: 302, Headers: map[string]string{"Location": "https://other.example/x"}})
	rig.web.PutPage(simweb.PageRule{URLPattern: "other.example/x", Body: "<p>landed</p>", ContentType: "text/html"})
	tool := fetchTool(t)
	dials := trackDials(t, tool.client)

	if status, text := page(t, tool.Execute(context.Background(), map[string]any{"url": "https://short.example/go"})); status != 200 || text != "landed" {
		t.Fatalf("status %d text %q, want the redirect target's page", status, text)
	}
	got := rig.j.kind("web.req")
	if len(got) != 2 || got[0]["host"] != "short.example" || got[0]["status"] != 302.0 || got[1]["host"] != "other.example" {
		t.Errorf("web.req = %v, want the redirect and its target, both in the sandbox", got)
	}
	for _, a := range dials.addrs {
		if a != rig.srv.Addr() {
			t.Errorf("dialled %q", a)
		}
	}

	// A redirect to a private host is refused by the redirect check, kept under the lock.
	rig.web.PutPage(simweb.PageRule{URLPattern: "evil.example/r", Status: 302, Headers: map[string]string{"Location": "http://127.0.0.1:1/api"}})
	before := len(rig.j.kind("web.req"))
	if r := tool.Execute(context.Background(), map[string]any{"url": "https://evil.example/r"}); !r.IsError || !strings.Contains(r.ForLLM, "private or local") {
		t.Errorf("redirect to loopback = %+v, want the private-host refusal", r)
	}
	if n := len(rig.j.kind("web.req")) - before; n != 1 {
		t.Errorf("%d requests for the refused redirect, want only the first hop", n)
	}
}

// The gateway's own loopback API is not reachable through the lab: the pre-flight refuses a
// private host before any request, and nothing reaches the sandbox server.
func TestWebFetch_LockedPrivatePreflightKept(t *testing.T) {
	rig := lockedWebRig(t)
	tool := fetchTool(t)
	dials := trackDials(t, tool.client)
	for _, u := range []string{"http://127.0.0.1:18790/api/status", "http://localhost/", "http://[::1]:8080/", "http://10.0.0.5/", rig.srv.BaseURL() + "/__sbx__/binance/api.binance.com/x"} {
		r := tool.Execute(context.Background(), map[string]any{"url": u})
		if !r.IsError || !strings.Contains(r.ForLLM, "private or local network") {
			t.Errorf("%s = %+v, want the private-host refusal", u, r)
		}
	}
	if len(dials.addrs) != 0 || len(rig.j.kind("web.req")) != 0 {
		t.Errorf("dials %v, web.req %v: the pre-flight must stop it before any request", dials.addrs, rig.j.kind("web.req"))
	}
}

func TestWebSearch_LockedBraveRendersRule(t *testing.T) {
	rig := lockedWebRig(t)
	rig.web.PutSearch(simweb.SearchRule{Query: "*btc*", Results: []simweb.Result{
		{Title: "BTC rallies", URL: "https://a.github.io/btc", Description: "It rallied."},
		{Title: "BTC again", URL: "https://b.github.io/btc"},
	}})
	s, err := NewWebSearchTool(WebSearchToolOptions{BraveEnabled: true, BraveAPIKeys: []string{"k-not-real"}})
	if err != nil || s == nil {
		t.Fatalf("NewWebSearchTool = %v, %v", s, err)
	}
	r := s.Execute(context.Background(), map[string]any{"query": "BTC news today"})
	want := "Results for: BTC news today\n1. BTC rallies\n   https://a.github.io/btc\n   It rallied.\n2. BTC again\n   https://b.github.io/btc"
	if r.IsError || r.ForLLM != want {
		t.Fatalf("search = %+v, want\n%s", r, want)
	}
	got := rig.j.kind("web.req")
	if len(got) != 1 || got[0]["host"] != "api.search.brave.com" || !strings.Contains(got[0]["query"].(string), "q=BTC+news+today") {
		t.Errorf("web.req = %v, want the logged Brave query", got)
	}
	if r := s.Execute(context.Background(), map[string]any{"query": "weather"}); r.IsError || !strings.Contains(r.ForLLM, "No results for") {
		t.Errorf("no rule = %+v, want Brave's empty answer", r)
	}
}

// Every search provider's client goes through the sandbox, SearXNG's nil-client fallback
// included, and a provider other than Brave is a 404: it fails closed.
func TestWebSearch_LockedEveryProviderFailsClosed(t *testing.T) {
	rig := lockedWebRig(t)
	for name, opts := range map[string]WebSearchToolOptions{
		"perplexity": {PerplexityEnabled: true, PerplexityAPIKeys: []string{"k"}},
		"tavily":     {TavilyEnabled: true, TavilyAPIKeys: []string{"k"}},
		"duckduckgo": {DuckDuckGoEnabled: true},
		"searxng":    {SearXNGEnabled: true, SearXNGBaseURL: "https://searx.example"},
		"glm":        {GLMSearchEnabled: true, GLMSearchAPIKey: "k", GLMSearchBaseURL: "https://glm.example/api"},
	} {
		s, err := NewWebSearchTool(opts)
		if err != nil || s == nil {
			t.Fatalf("%s: %v, %v", name, s, err)
		}
		c := providerClient(s.provider)
		if _, ok := c.Transport.(*sandbox.RoundTripper); !ok {
			t.Errorf("%s: client transport is %T, want the sandbox RoundTripper", name, c.Transport)
		}
		before := len(rig.j.kind("web.req"))
		// Some providers report a failed search as an error, some as "no results".
		if r := s.Execute(context.Background(), map[string]any{"query": "btc"}); strings.Contains(r.ForLLM, "Results for") {
			t.Errorf("%s: search = %q, want no results", name, r.ForLLM)
		}
		got := rig.j.kind("web.req")
		if len(got) <= before || got[len(got)-1]["status"] != 404.0 {
			t.Errorf("%s: web.req = %v, want a 404 logged", name, got)
		}
	}
	// The SearXNG provider built without a client builds one on the first search.
	p := &SearXNGSearchProvider{baseURL: "https://searx-fallback.example"}
	if _, err := p.Search(context.Background(), "q", 3, ""); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("fallback search error = %v, want the sandbox's 404", err)
	}
	if got := rig.j.kind("web.req"); got[len(got)-1]["host"] != "searx-fallback.example" {
		t.Errorf("fallback request not routed to the sandbox: %v", got[len(got)-1])
	}
}

// Unlocked, the web tools are what they were: the SSRF-safe dialer, no wrapper.
func TestWebTools_UnlockedUnchanged(t *testing.T) {
	if sandbox.Locked() {
		t.Fatal("this process must not be locked")
	}
	tool := fetchTool(t)
	tr, ok := tool.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unlocked transport is %T, want a plain *http.Transport", tool.client.Transport)
	}
	if tr.DialContext == nil || !strings.Contains(funcName(tr.DialContext), "newSafeDialContext") {
		t.Errorf("unlocked dialer is %s, want newSafeDialContext's", funcName(tr.DialContext))
	}
	// Behaviour, not just identity: a loopback listener is refused and sees no connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			c.Close()
		}
	}()
	if c, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Error("the unlocked dialer connected to loopback")
	}
	select {
	case <-accepted:
		t.Error("the listener saw a connection from the unlocked dialer")
	default:
	}

	// A search client and the helper are untouched too.
	c, err := searchClient("", searchTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Transport.(*http.Transport); !ok {
		t.Errorf("unlocked search client transport is %T, want *http.Transport", c.Transport)
	}
	plain := &http.Client{}
	if labWebClient(plain) != plain || plain.Transport != nil {
		t.Error("labWebClient changed a client in an unlocked process")
	}
	if labWebClient(nil) != nil {
		t.Error("labWebClient(nil) built a client in an unlocked process")
	}
}

func providerClient(p SearchProvider) *http.Client {
	switch p := p.(type) {
	case *PerplexitySearchProvider:
		return p.client
	case *BraveSearchProvider:
		return p.client
	case *SearXNGSearchProvider:
		return p.client
	case *TavilySearchProvider:
		return p.client
	case *DuckDuckGoSearchProvider:
		return p.client
	case *GLMSearchProvider:
		return p.client
	}
	return nil
}

func funcName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// A fault rule for venue web reaches web_fetch: a latency rule holds the request (the lab's
// server wrap), a status rule answers it in the fault stage. In both SimWeb is behind it.
func TestWebFetch_FaultOnVenueWebReachesTheTool(t *testing.T) {
	rig := lockedWebRig(t)
	rig.web.PutPage(simweb.PageRule{URLPattern: "slow.example/*", Body: "<p>ok</p>"})
	fj := &webJournal{}
	inj := fault.New(fault.Options{Journal: fj, MaxHold: 5 * time.Second})
	t.Cleanup(func() { _ = inj.Close() })
	rig.chain.Set(sandbox.StageFault, inj.Middleware())
	rig.srv.SetHandlerWrap(inj.HandlerWrap())

	const delay = 200 * time.Millisecond
	if _, err := inj.Add(fault.Rule{Match: fault.Match{Venue: "web"}, Effect: fault.Effect{Latency: fault.Duration(delay)}, When: fault.When{Times: 1}}); err != nil {
		t.Fatal(err)
	}
	tool := fetchTool(t)
	start := time.Now()
	status, text := page(t, tool.Execute(context.Background(), map[string]any{"url": "https://slow.example/a"}))
	if elapsed := time.Since(start); elapsed < delay || status != 200 || text != "ok" {
		t.Errorf("held fetch: %v %d %q, want it held at least %v and then served", elapsed, status, text, delay)
	}
	if got := fj.kind("fault.fired"); len(got) != 1 || got[0]["venue"] != "web" || got[0]["effect"] != "latency" {
		t.Errorf("fault.fired = %v", got)
	}

	if _, err := inj.Add(fault.Rule{Match: fault.Match{Venue: "web"}, Effect: fault.Effect{Status: 503, Body: json.RawMessage(`{"down":true}`)}}); err != nil {
		t.Fatal(err)
	}
	before := len(rig.j.kind("web.req"))
	status, text = page(t, tool.Execute(context.Background(), map[string]any{"url": "https://slow.example/b"}))
	if status != 503 || !strings.Contains(text, "down") {
		t.Errorf("faulted fetch = %d %q, want the injected 503", status, text)
	}
	if len(rig.j.kind("web.req")) != before {
		t.Error("a request the fault stage answered was logged by SimWeb (it never saw it)")
	}
}
