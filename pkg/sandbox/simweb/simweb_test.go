package simweb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

type memJournal struct {
	mu sync.Mutex
	es []labjournal.Entry
}

func (m *memJournal) Append(e labjournal.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.es = append(m.es, e)
}

// serve runs one request for venue through the stage, the way the chain would, and returns
// the answer and whether it was the stage's own (not next's).
func serve(t *testing.T, s *Store, venue, method, target string) (*sandbox.Response, bool) {
	t.Helper()
	nextCalled := false
	next := sandbox.ResponderFunc(func(string, string, string, *http.Request) (*sandbox.Response, bool) {
		nextCalled = true
		return nil, false
	})
	r := httptest.NewRequest(method, "/__sbx__/"+venue+"/"+target, nil)
	p, err := sandbox.ParseRequest(r.URL.EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	resp, ok := s.Middleware()(next).Respond(venue, method, p.Path, r)
	if nextCalled && ok {
		t.Fatal("answered and also called next")
	}
	return resp, ok
}

func TestSimWeb_PageRulesGlobNewestWinsAnd404(t *testing.T) {
	s, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPage(PageRule{URLPattern: "attacker.example/*", Body: "old"}); err != nil {
		t.Fatal(err)
	}
	id, err := s.PutPage(PageRule{URLPattern: "*.github.io/news/*", Body: "<p>hi</p>", Status: 203, Headers: map[string]string{"X-A": "b"}})
	if err != nil {
		t.Fatal(err)
	}
	newest, _ := s.PutPage(PageRule{URLPattern: "ATTACKER.example/deep/*", Body: "new"})

	cases := []struct {
		target, body string
		status       int
		rule         string
	}{
		{"attacker.example/", "old", 200, "page-1"},
		{"attacker.example/a/b/c?d=1", "old", 200, "page-1"}, // * crosses "/", the query is ignored
		{"attacker.example/deep/x/y", "new", 200, newest},    // the newest matching rule wins, case-insensitively
		{"a.b.github.io/news/2026/x", "<p>hi</p>", 203, id},
		{"attacker.example:8080/z", "old", 200, "page-1"}, // matching is on the host without its port
		{"other.example/", "sandbox: no web fixture for other.example/", 404, ""},
	}
	for _, c := range cases {
		resp, ok := serve(t, s, "web", "GET", c.target)
		if !ok {
			t.Fatalf("%s: the web stage declined", c.target)
		}
		if resp.Status != c.status || resp.BodyText != c.body {
			t.Errorf("%s = %d %q, want %d %q", c.target, resp.Status, resp.BodyText, c.status, c.body)
		}
	}
	resp, _ := serve(t, s, "web", "GET", "a.github.io/news/1")
	if resp.Headers["X-A"] != "b" || resp.Headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Errorf("headers = %v, want X-A and the default content type", resp.Headers)
	}
	reqs := s.Requests(0, "")
	if len(reqs) != 7 || reqs[0].RuleID != "page-1" || reqs[5].Status != 404 || reqs[5].RuleID != "" {
		t.Errorf("request log = %+v", reqs)
	}
}

func TestSimWeb_BraveSearchRendersRuleAndEmptyShape(t *testing.T) {
	s, _ := Open(Options{})
	if _, err := s.PutSearch(SearchRule{Query: "*bitcoin*", Results: []Result{{Title: "T", URL: "https://a.github.io/n", Description: "D"}}}); err != nil {
		t.Fatal(err)
	}
	get := func(q string) string {
		resp, ok := serve(t, s, "web", "GET", "api.search.brave.com/res/v1/web/search?q="+q+"&count=5")
		if !ok || resp.Status != 200 || resp.Headers["Content-Type"] != "application/json" {
			t.Fatalf("search %q = %+v ok=%v", q, resp, ok)
		}
		return string(resp.Body)
	}
	if got := get("Bitcoin+ETF+news"); got != `{"web":{"results":[{"title":"T","url":"https://a.github.io/n","description":"D"}]}}` {
		t.Errorf("matching search = %s", got)
	}
	if got := get("weather"); got != `{"web":{"results":[]}}` {
		t.Errorf("no rule = %s, want Brave's empty shape", got)
	}
	// Another provider's host is not rendered: a 404 of the sandbox's own.
	for _, h := range []string{"api.tavily.com/search", "api.perplexity.ai/chat/completions", "html.duckduckgo.com/html/?q=x", "searx.example/search?q=x"} {
		if resp, _ := serve(t, s, "web", "GET", h); resp.Status != 404 {
			t.Errorf("%s = %d, want 404", h, resp.Status)
		}
	}
}

// A request for another venue is not SimWeb's: it goes on, untouched and unlogged.
func TestSimWeb_OtherVenuesGoToNext(t *testing.T) {
	s, _ := Open(Options{})
	next := false
	n := sandbox.ResponderFunc(func(string, string, string, *http.Request) (*sandbox.Response, bool) {
		next = true
		return nil, false
	})
	r := httptest.NewRequest("GET", "/__sbx__/binance/api.binance.com/api/v3/ping", nil)
	if _, ok := s.Middleware()(n).Respond("binance", "GET", "/api/v3/ping", r); ok || !next {
		t.Fatalf("a binance request: ok=%v next=%v, want it passed on", ok, next)
	}
	if len(s.Requests(0, "")) != 0 {
		t.Error("an exchange request was logged as a web request")
	}
}

func TestSimWeb_PersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab", "web.json")
	s, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutPage(PageRule{URLPattern: "x.example/*", Body: "b", Status: 418}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSearch(SearchRule{Query: "q", Results: []Result{{Title: "t"}}}); err != nil {
		t.Fatal(err)
	}
	again, err := Open(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	pages, searches := again.List()
	if len(pages) != 1 || pages[0].Status != 418 || pages[0].ID != "page-1" || len(searches) != 1 || searches[0].ID != "search-2" {
		t.Fatalf("restored %+v %+v", pages, searches)
	}
	if id, _ := again.PutPage(PageRule{URLPattern: "y.example/*"}); id != "page-3" {
		t.Errorf("the next ID after a restart = %q, want page-3 (IDs are not reused)", id)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Options{Path: path}); err == nil {
		t.Error("a corrupt web.json opened as an empty store")
	}
}

func TestSimWeb_PutValidatesAndReplacesByID(t *testing.T) {
	s, _ := Open(Options{})
	for i, bad := range []PageRule{
		{URLPattern: ""}, {URLPattern: "x/*", Status: 99}, {URLPattern: "x/*", Status: 1000},
		{URLPattern: "x/*", Body: strings.Repeat("a", maxBody+1)}, {URLPattern: strings.Repeat("a", maxPattern+1)},
	} {
		if _, err := s.PutPage(bad); err == nil {
			t.Errorf("accepted bad rule %d", i)
		}
	}
	if _, err := s.PutSearch(SearchRule{}); err == nil {
		t.Error("accepted a search with no query")
	}
	a, _ := s.PutPage(PageRule{ID: "p", URLPattern: "x/*", Body: "1"})
	s.PutPage(PageRule{ID: "q", URLPattern: "x/*", Body: "2"})
	s.PutPage(PageRule{ID: a, URLPattern: "x/*", Body: "3"}) // same ID: replaced, and now the newest
	if pages, _ := s.List(); len(pages) != 2 || pages[1].ID != "p" || pages[1].Body != "3" {
		t.Errorf("pages = %+v", pages)
	}
	for range maxRules {
		if _, err := s.PutPage(PageRule{URLPattern: "z/*"}); err != nil {
			break
		}
	}
	if _, err := s.PutPage(PageRule{URLPattern: "z/*"}); err == nil {
		t.Error("the rule count is unbounded")
	}
}

func TestSimWeb_RequestLogRingAndJournal(t *testing.T) {
	j := &memJournal{}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	s, _ := Open(Options{Journal: j, Now: func() time.Time { return at }})
	for range ringSize + 5 {
		serve(t, s, "web", "GET", "u.example/p?x=1")
	}
	reqs := s.Requests(0, "")
	if len(reqs) != ringSize || reqs[0].Seq != 6 || reqs[len(reqs)-1].Seq != ringSize+5 {
		t.Fatalf("ring holds %d requests, first seq %d", len(reqs), reqs[0].Seq)
	}
	if got := s.Requests(int64(ringSize+3), "U.example"); len(got) != 2 {
		t.Errorf("since + host = %d requests, want 2", len(got))
	}
	if len(s.Requests(0, "nobody.example")) != 0 {
		t.Error("the host filter matched another host")
	}
	// The journal is unbounded: it holds every request, the ring's dropped ones included.
	if len(j.es) != ringSize+5 {
		t.Fatalf("journal has %d entries, want %d", len(j.es), ringSize+5)
	}
	e := j.es[0]
	var p map[string]any
	if err := json.Unmarshal(e.Payload, &p); err != nil || e.Kind != "web.req" || e.Subject != "u.example" || !e.SimTime.Equal(at) ||
		p["url"] != "u.example/p?x=1" || p["host"] != "u.example" || p["path"] != "/p" || p["query"] != "x=1" || p["status"] != 404.0 || p["method"] != "GET" {
		t.Errorf("entry = %+v payload %v", e, p)
	}
}
