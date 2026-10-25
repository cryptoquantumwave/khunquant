package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
)

const statsPath = "/api/proactive/stats"

// statsRoute registers the stats route the way registerGatewayRoutes does, on a
// capturing mux, so the guards can be exercised without a server.
func statsRoute(t *testing.T, bus *events.EventBus, token string) http.Handler {
	t.Helper()
	var registered http.Handler
	var pattern string
	registerProactiveStatsAPI(patternSink{fn: func(p string, h http.Handler) {
		pattern, registered = p, h
	}}, bus, token)
	if pattern != "GET "+statsPath {
		t.Fatalf("registered %q, want GET %s", pattern, statsPath)
	}
	return registered
}

type patternSink struct{ fn func(string, http.Handler) }

func (s patternSink) Handle(p string, h http.Handler) { s.fn(p, h) }

func statsRequest(h http.Handler, remote, token string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, statsPath, nil)
	req.RemoteAddr = remote
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStatsRoute(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	h := statsRoute(t, bus, "secret")

	// A loopback caller with the token gets the counts.
	bus.PublishNonBlocking(events.Event{Kind: events.KindAgentSubTurnSpawn})
	rec := statsRequest(h, "127.0.0.1:4000", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback caller with the token: status %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got busStatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if got.Published != 1 || got.Closed || got.Subscribers != 0 || got.SubscriberStats == nil {
		t.Errorf("stats = %+v, want 1 published on an open bus with no subscribers and an empty (non-null) list", got)
	}

	// Every management route also refuses a loopback caller without the token,
	// and one that comes with a browser's Origin.
	if rec := statsRequest(h, "127.0.0.1:4000", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", rec.Code)
	}
	if rec := statsRequest(h, "127.0.0.1:4000", "wrong", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", rec.Code)
	}
	if rec := statsRequest(h, "127.0.0.1:4000", "secret", func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	}); rec.Code != http.StatusForbidden {
		t.Errorf("browser origin: status %d, want 403", rec.Code)
	}
}

// A caller that is not on the loopback interface is refused by loopbackOnly,
// whatever it presents - the same 403 as the cron and sandbox routes.
func TestStatsRoute_RefusesNonLoopbackCaller(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	h := statsRoute(t, bus, "secret")

	for _, remote := range []string{"10.0.0.5:1234", "192.168.1.20:9", "[2001:db8::1]:80", "garbage"} {
		rec := statsRequest(h, remote, "secret", nil) // even with the right token
		if rec.Code != http.StatusForbidden {
			t.Errorf("caller %s: status %d, want 403", remote, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "published") {
			t.Errorf("caller %s was refused but the body has stats: %s", remote, rec.Body)
		}
	}
	for _, remote := range []string{"127.0.0.1:1", "[::1]:1"} {
		if rec := statsRequest(h, remote, "secret", nil); rec.Code != http.StatusOK {
			t.Errorf("loopback caller %s: status %d, want 200", remote, rec.Code)
		}
	}
}

// The response is counts only: the exact key set is pinned, and a published
// event's payload, scope and attrs are nowhere in it.
func TestStatsRoute_ReturnsCountsOnly(t *testing.T) {
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	h := statsRoute(t, bus, "secret")

	const secret = "BALANCE_9_999_999"
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	if _, err := bus.Channel().Subscribe(context.Background(),
		events.SubscribeOptions{Name: "watcher", Buffer: 1},
		func(context.Context, events.Event) error { <-release; return nil },
	); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // 1 handled/held, 1 queued, the rest dropped
		bus.PublishNonBlocking(events.Event{
			Kind:    events.KindAgentSubTurnSpawn,
			Payload: map[string]string{"prompt": secret},
			Attrs:   map[string]any{"token": secret},
			Scope:   events.Scope{SessionKey: secret},
		})
	}

	rec := statsRequest(h, "127.0.0.1:4000", "secret", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("the stats body carries event content: %s", rec.Body)
	}

	keys := func(raw json.RawMessage) []string {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}
	top := keys(rec.Body.Bytes())
	wantTop := []string{"blocked", "closed", "delivered", "dropped", "matched", "published", "subscriber_stats", "subscribers"}
	if strings.Join(top, ",") != strings.Join(wantTop, ",") {
		t.Errorf("top-level keys = %v, want %v", top, wantTop)
	}

	var body struct {
		Published       uint64            `json:"published"`
		Dropped         uint64            `json:"dropped"`
		SubscriberStats []json.RawMessage `json:"subscriber_stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Published != 5 || body.Dropped == 0 || len(body.SubscriberStats) != 1 {
		t.Fatalf("stats = %s, want 5 published, some dropped, one subscriber", rec.Body)
	}
	sub := keys(body.SubscriberStats[0])
	wantSub := []string{"dropped", "failed", "handled", "id", "name", "panicked", "received", "timed_out"}
	if strings.Join(sub, ",") != strings.Join(wantSub, ",") {
		t.Errorf("subscriber keys = %v, want %v", sub, wantSub)
	}
}

// ---- through the real assembly ----

// The route is registered by registerGatewayRoutes, which every reload runs, and
// the counts come from the one shared bus, so they do not reset on a reload.
func TestReload_StatsRouteSurvivesAndKeepsCounting(t *testing.T) {
	f := newAssemblyFixture(t, nil) // proactive off: the bus is there anyway
	bus := f.shared.Events
	if bus == nil {
		t.Fatal("the shared resources have no event bus")
	}

	published := func() uint64 {
		t.Helper()
		body := f.expectStatus(statsPath, testMgmtToken, http.StatusOK)
		var got busStatsResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if got.Closed {
			t.Fatalf("the bus reports closed while the gateway runs: %s", body)
		}
		return got.Published
	}

	if got := published(); got != 0 {
		t.Fatalf("a fresh gateway has %d published event(s), want 0", got)
	}
	bus.PublishNonBlocking(events.Event{Kind: events.KindAgentSubTurnSpawn})
	bus.PublishNonBlocking(events.Event{Kind: events.KindAgentSubTurnEnd})
	if got := published(); got != 2 {
		t.Fatalf("published = %d, want 2", got)
	}

	for i := 1; i <= 2; i++ {
		if err := f.reload(nil); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if f.shared.Events != bus {
			t.Fatalf("reload %d replaced the event bus", i)
		}
		if got := published(); got != uint64(1+i) {
			t.Fatalf("after reload %d the route reports %d published, want %d (the same bus, not reset)", i, got, 1+i)
		}
		bus.PublishNonBlocking(events.Event{Kind: events.KindAgentSubTurnSpawn})
		if got := published(); got != uint64(2+i) {
			t.Fatalf("after reload %d: published = %d, want %d", i, got, 2+i)
		}
	}

	// The route answers only to what the other management routes answer to.
	f.expectStatus(statsPath, "", http.StatusUnauthorized)
	f.expectStatus(statsPath, "not-the-token", http.StatusUnauthorized)

	// And only to GET.
	req, err := http.NewRequest(http.MethodPost, f.url(statsPath), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testMgmtToken)
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST %s: status %d, want 405", statsPath, resp.StatusCode)
	}
}

func eventBusClosers(r *sharedResources) int { return countShared(r, eventBusResource) }

// One bus for the life of the process: not replaced by a reload, closed exactly
// once at shutdown, and a publish after that is harmless.
func TestEventBusLifecycle(t *testing.T) {
	f := newAssemblyFixture(t, withOwner("1001")) // proactive on this time
	bus := f.shared.Events

	for i := 0; i < 2; i++ {
		if err := f.reload(withOwner("1001")); err != nil {
			t.Fatalf("reload %d: %v", i+1, err)
		}
		if f.shared.Events != bus {
			t.Fatalf("reload %d replaced the event bus", i+1)
		}
		if bus.Stats().Closed {
			t.Fatalf("reload %d closed the event bus", i+1)
		}
		if got := eventBusClosers(f.shared); got != 1 {
			t.Fatalf("after reload %d the bus is registered for close %d times, want 1", i+1, got)
		}
	}

	// A subscriber that outlives the reloads still sees events.
	sub, ch, err := bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t", Buffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	_ = sub
	bus.PublishNonBlocking(events.Event{Kind: events.KindGatewayReady})
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("a subscriber did not receive an event after the reloads")
	}

	// Shutdown: services stop, then the shared resources close.
	stopServices(f.services, 5*time.Second)
	f.services = nil
	if err := f.shared.Close(); err != nil {
		t.Fatal(err)
	}
	if !bus.Stats().Closed {
		t.Fatal("shutdown left the event bus open")
	}
	if err := f.shared.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	if res := bus.PublishNonBlocking(events.Event{Kind: events.KindGatewayReady}); !res.Closed {
		t.Errorf("publish after close = %+v, want Closed", res)
	}
	if res := bus.Publish(context.Background(), events.Event{Kind: events.KindGatewayReady}); !res.Closed {
		t.Errorf("blocking publish after close = %+v, want Closed", res)
	}
}

// Without shared resources (a caller that has none) wiring and routing do not
// panic: the route reports a closed, empty bus.
func TestEventBus_NilSharedResources(t *testing.T) {
	var r *sharedResources
	if r.eventBus() != nil {
		t.Fatal("a nil sharedResources has a bus")
	}
	wireEventBus(assemblyDeps{}) // no loop, no shared: nothing to do

	h := statsRoute(t, r.eventBus(), "secret")
	rec := statsRequest(h, "127.0.0.1:1", "secret", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"closed":true`) {
		t.Fatalf("status %d body %s, want 200 and a closed bus", rec.Code, rec.Body)
	}
}
