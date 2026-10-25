package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/web/backend/middleware"
)

// The dashboard routes only proxy: the launcher opens no database, adds the management
// token server-side, forwards only the query it validated, and builds the live stream by
// polling a gateway route.

// fakeGateway serves /api/proactive/changes from a list of items numbered 1..n, as the
// real feed does: the cursor is the number of the last item delivered.
type fakeGateway struct {
	mu      sync.Mutex
	items   int // how many exist
	down    atomic.Bool
	polls   atomic.Int64
	queries []string
	auth    []string
}

func (g *fakeGateway) add(n int) { g.mu.Lock(); g.items += n; g.mu.Unlock() }

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.queries = append(g.queries, r.URL.Path+"?"+r.URL.RawQuery)
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	g.mu.Unlock()
	if r.URL.Path != "/api/proactive/changes" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"available":true}`))
		return
	}
	g.polls.Add(1)
	if g.down.Load() {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := -1
	if c := r.URL.Query().Get("cursor"); c != "" {
		cur, _ = strconv.Atoi(c)
	}
	type item struct {
		Type   string `json:"type"`
		Cursor string `json:"cursor"`
		Data   any    `json:"data"`
	}
	items := []item{}
	if cur >= 0 {
		for i := cur + 1; i <= g.items && len(items) < 200; i++ {
			typ := "attention.decision"
			if i%2 == 0 {
				typ = "approval.pending"
			}
			items = append(items, item{typ, strconv.Itoa(i), map[string]any{"id": "row" + strconv.Itoa(i), "title": "t\nx"}})
		}
	}
	next := cur
	if len(items) > 0 {
		next, _ = strconv.Atoi(items[len(items)-1].Cursor)
	} else if cur < 0 {
		next = g.items
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"available": true, "cursor": strconv.Itoa(next), "items": items})
}

type dashEnv struct {
	h   *Handler
	gw  *fakeGateway
	mux *http.ServeMux
	srv *httptest.Server
}

func newDashEnv(t *testing.T) *dashEnv {
	t.Helper()
	gw := &fakeGateway{}
	gsrv := httptest.NewServer(gw)
	t.Cleanup(gsrv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(gsrv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", port, "s3cret-mgmt"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(path)
	h.proactive.pollEvery, h.proactive.heartbeatEvery = 10*time.Millisecond, 25*time.Millisecond
	mux := http.NewServeMux()
	h.registerAgentProactiveDashboardRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &dashEnv{h: h, gw: gw, mux: mux, srv: srv}
}

// stream opens the event stream and returns its lines on a channel.
type stream struct {
	resp  *http.Response
	lines chan string
	stop  context.CancelFunc
}

func (e *dashEnv) open(t *testing.T, query string, header http.Header) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/api/agent/proactive/events"+query, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &stream{resp: resp, lines: make(chan string, 1000), stop: cancel}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			s.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return s
}

// until reads lines until one satisfies ok, or fails after a deadline.
func (s *stream) until(t *testing.T, what string, ok func(line string) bool) {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case l, open := <-s.lines:
			if !open {
				t.Fatalf("the stream ended before %s", what)
			}
			if ok(l) {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestDashboardProxy_ForwardsOnlyValidatedQueryWithTheToken(t *testing.T) {
	e := newDashEnv(t)
	get := func(p string) int {
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if strings.Contains(rec.Body.String(), "s3cret-mgmt") {
			t.Errorf("GET %s: the token reached the browser", p)
		}
		return rec.Code
	}
	if code := get("/api/agent/proactive/interventions?limit=20&disposition=notify&trigger=price_cross&evil=1&cursor=12.3&from=2026-03-01T00:00:00Z"); code != http.StatusOK {
		t.Fatalf("valid interventions query: %d", code)
	}
	got := e.gw.queries[len(e.gw.queries)-1]
	if strings.Contains(got, "evil") || !strings.HasPrefix(got, "/api/proactive/interventions?") || !strings.Contains(got, "limit=20") || !strings.Contains(got, "trigger=price_cross") {
		t.Errorf("forwarded %q: only the checked parameters, rebuilt", got)
	}
	if a := e.gw.auth[len(e.gw.auth)-1]; a != "Bearer s3cret-mgmt" {
		t.Errorf("authorization = %q", a)
	}
	n := len(e.gw.queries)
	for _, bad := range []string{
		"/api/agent/proactive/interventions?limit=500", "/api/agent/proactive/interventions?limit=0", "/api/agent/proactive/interventions?from=today",
		"/api/agent/proactive/interventions?trigger=a%20b", "/api/agent/proactive/interventions?cursor=x",
		"/api/agent/proactive/metrics?days=0", "/api/agent/proactive/metrics?days=91", "/api/agent/proactive/metrics?days=abc",
	} {
		if code := get(bad); code != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", bad, code)
		}
	}
	if len(e.gw.queries) != n {
		t.Error("a request with a bad parameter reached the gateway")
	}
	if code := get("/api/agent/proactive/metrics?days=90"); code != http.StatusOK {
		t.Errorf("metrics days=90: %d", code)
	}
	if code := get("/api/agent/proactive/state"); code != http.StatusOK {
		t.Errorf("state: %d", code)
	}
}

func TestEvents_CursorAdvancesWithoutDuplicates(t *testing.T) {
	e := newDashEnv(t)
	e.gw.add(3) // already there: a new stream starts after them
	s := e.open(t, "", nil)
	s.until(t, "the live status", func(l string) bool { return strings.Contains(l, `"state":"live"`) })
	e.gw.add(5)
	var ids, types []string
	s.until(t, "five events", func(l string) bool {
		if v, ok := strings.CutPrefix(l, "id: "); ok {
			ids = append(ids, v)
		}
		if v, ok := strings.CutPrefix(l, "event: "); ok && strings.HasPrefix(v, "a") {
			types = append(types, v)
		}
		return len(ids) == 5
	})
	if strings.Join(ids, ",") != "4,5,6,7,8" {
		t.Errorf("ids = %v, want 4..8 once each", ids)
	}
	if types[0] != "approval.pending" || types[1] != "attention.decision" {
		t.Errorf("event names = %v", types)
	}
	// An item's text never breaks the framing: the newline in the title stayed escaped.
	e.gw.add(1)
	s.until(t, "the data line", func(l string) bool { return strings.HasPrefix(l, "data: ") && strings.Contains(l, `"title":"t\nx"`) })
	// Nothing repeats while idle.
	time.Sleep(100 * time.Millisecond)
	e.gw.mu.Lock()
	defer e.gw.mu.Unlock()
	last := e.gw.queries[len(e.gw.queries)-1]
	if !strings.Contains(last, "cursor=9") {
		t.Errorf("the last poll = %q, want cursor=9 (the cursor advanced to the last item)", last)
	}
}

func TestEvents_ResumesFromLastEventID(t *testing.T) {
	e := newDashEnv(t)
	e.gw.add(4)
	s := e.open(t, "", http.Header{"Last-Event-Id": {"2"}})
	var ids []string
	s.until(t, "events 3 and 4", func(l string) bool {
		if v, ok := strings.CutPrefix(l, "id: "); ok {
			ids = append(ids, v)
		}
		return len(ids) == 2
	})
	if strings.Join(ids, ",") != "3,4" {
		t.Errorf("resumed with %v, want 3,4", ids)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agent/proactive/events?cursor=x%0Ay", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed cursor: %d", rec.Code)
	}
}

func TestEvents_HeartbeatAndGatewayOutage(t *testing.T) {
	e := newDashEnv(t)
	s := e.open(t, "", nil)
	s.until(t, "a heartbeat", func(l string) bool { return l == ": heartbeat" })
	e.gw.down.Store(true)
	s.until(t, "the unavailable status", func(l string) bool { return strings.Contains(l, `"state":"unavailable"`) })
	e.gw.down.Store(false)
	s.until(t, "the stream to come back", func(l string) bool { return strings.Contains(l, `"state":"live"`) })
}

func TestEvents_DisconnectStopsPolling(t *testing.T) {
	e := newDashEnv(t)
	s := e.open(t, "", nil)
	s.until(t, "the first poll", func(l string) bool { return strings.Contains(l, `"state":"live"`) })
	s.stop()
	s.resp.Body.Close()
	time.Sleep(100 * time.Millisecond)
	before := e.gw.polls.Load()
	time.Sleep(150 * time.Millisecond) // 15 polls' worth
	if after := e.gw.polls.Load(); after != before {
		t.Fatalf("the launcher kept polling after the client left: %d -> %d", before, after)
	}
	// And its slot is free.
	e.h.proactive.streamMu.Lock()
	open := e.h.proactive.streams
	e.h.proactive.streamMu.Unlock()
	if open != 0 {
		t.Errorf("%d streams still counted", open)
	}
}

func TestEvents_FifthStreamIs429AndAFreedSlotIsReused(t *testing.T) {
	e := newDashEnv(t)
	var open []*stream
	for i := 0; i < eventsMaxStreams; i++ {
		s := e.open(t, "", nil)
		s.until(t, "a stream to start", func(l string) bool { return l == ": connected" })
		open = append(open, s)
	}
	resp, err := http.Get(e.srv.URL + "/api/agent/proactive/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the fifth stream: %d, want 429", resp.StatusCode)
	}
	open[0].stop()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r, err := http.Get(e.srv.URL + "/api/agent/proactive/events")
		if err == nil && r.StatusCode == http.StatusOK {
			r.Body.Close()
			return
		}
		if err == nil {
			r.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("the slot of a closed stream was never freed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvents_ShutdownClosesTheStream(t *testing.T) {
	e := newDashEnv(t)
	s := e.open(t, "", nil)
	s.until(t, "the stream to start", func(l string) bool { return l == ": connected" })
	_ = e.h.Close()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, open := <-s.lines:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the stream outlived the launcher's shutdown")
		}
	}
}

// The stream sits behind the dashboard's authentication like every other /api route.
func TestEvents_RequireTheDashboardAuthentication(t *testing.T) {
	e := newDashEnv(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := middleware.NewSessionStore(0)
	sessions := middleware.SessionAuth("launcher-secret", store, string(hash), middleware.CSRFMutations(e.mux))
	chain, err := middleware.IPAllowlist(middleware.IPAllowlistConfig{}, middleware.PasswordAuth(middleware.PasswordAuthConfig{
		Sessions: store, PasswordHash: string(hash), LauncherToken: "launcher-secret",
	}, sessions))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/agent/proactive/events", "/api/agent/proactive/interventions", "/api/agent/proactive/metrics", "/api/agent/proactive/state"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: %d, want 401", p, rec.Code)
		}
	}
	if n := e.gw.polls.Load(); n != 0 {
		t.Errorf("an unauthenticated stream polled the gateway %d times", n)
	}
}
