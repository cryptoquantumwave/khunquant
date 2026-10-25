package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const triggerTestToken = "trigger-test-token"

// recordingWM is a world model that records the events the pipeline applies.
type recordingWM struct {
	mu  sync.Mutex
	got []events.Event
}

func (w *recordingWM) Apply(_ context.Context, ev events.Event) (engine.Diff, error) {
	w.mu.Lock()
	w.got = append(w.got, ev)
	w.mu.Unlock()
	return nil, nil
}
func (*recordingWM) View() engine.View { return nil }

func (w *recordingWM) events() []events.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]events.Event(nil), w.got...)
}

// triggerHarness is the route over a stub pipeline: a real engine (running or not)
// and a real registry, on a loopback listener, behind the guards a registered
// route gets.
type triggerHarness struct {
	t     *testing.T
	srv   *httptest.Server
	mux   *http.ServeMux
	api   *triggerAPI
	wm    *recordingWM
	clock *perceptiontest.Clock
	eng   *engine.Engine
	rt    *perceptionRuntime
}

type harnessOpts struct {
	inbox      int  // engine inbox size; 0 = default
	run        bool // run the engine
	noRuntime  bool // the holder holds nothing
	noRegistry bool // perception runs, triggers do not
	token      string
}

func newTriggerHarness(t *testing.T, o harnessOpts) *triggerHarness {
	t.Helper()
	h := &triggerHarness{t: t, wm: &recordingWM{}, clock: perceptiontest.NewClock(time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC))}
	h.eng = engine.New(engine.Options{WorldModel: h.wm, InboxSize: o.inbox})
	if o.run {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = h.eng.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	}
	h.rt = &perceptionRuntime{engine: h.eng}
	if !o.noRegistry {
		reg, err := triggers.NewRegistry(context.Background(), sqlitextest.Open(t), func() string { return triggers.ModeLive }, nil)
		if err != nil {
			t.Fatal(err)
		}
		h.rt.triggers = reg
	}
	h.api = &triggerAPI{
		runtime: func() *perceptionRuntime {
			if o.noRuntime {
				return nil
			}
			return h.rt
		},
		mode:    func() string { return config.ProactiveModeLive },
		limiter: newTriggerLimiter(triggerRatePerMinute, triggerBurst, h.clock.Now),
		now:     h.clock.Now,
	}
	token := o.token
	if token == "" {
		token = triggerTestToken
	}
	h.mux = http.NewServeMux()
	registerTriggerAPI(h.mux, token, h.api)
	// A sibling route, registered the way the stats route is, to compare refusals.
	registerProactiveStatsAPI(h.mux, events.NewBus(), token)
	h.srv = httptest.NewServer(h.mux)
	t.Cleanup(h.srv.Close)
	return h
}

type triggerReply struct {
	code   int
	body   string
	header http.Header
}

func (h *triggerHarness) post(body string, hdr map[string]string) triggerReply {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+triggerRoute, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+triggerTestToken)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return triggerReply{code: resp.StatusCode, body: string(b), header: resp.Header}
}

func jsonBody(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTriggerRoute_RequiresToken(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{})
	body := `{"name":"backup-done"}`
	for name, hdr := range map[string]map[string]string{
		"no token":    {"Authorization": ""},
		"wrong token": {"Authorization": "Bearer not-the-token"},
		"not bearer":  {"Authorization": "Basic " + triggerTestToken},
	} {
		got := h.post(body, hdr)
		if got.code != http.StatusUnauthorized {
			t.Errorf("%s: status %d", name, got.code)
		}
	}
	if st := h.eng.Stats(); st.QueueDepth != 0 {
		t.Fatalf("a refused request reached the pipeline: %+v", st)
	}

	// The refusal is the one the sibling management route gives.
	sibling, err := http.Get(h.srv.URL + "/api/proactive/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer sibling.Body.Close()
	sb, _ := io.ReadAll(sibling.Body)
	ours := h.post(body, map[string]string{"Authorization": ""})
	if sibling.StatusCode != ours.code || string(sb) != ours.body {
		t.Fatalf("sibling refuses with %d %q, the trigger route with %d %q", sibling.StatusCode, sb, ours.code, ours.body)
	}
}

// A route without a token is not registered at all: the bearer middleware lets
// everything through when its token is empty.
func TestTriggerRoute_NotRegisteredWithoutAToken(t *testing.T) {
	mux := http.NewServeMux()
	registerTriggerAPI(mux, "", &triggerAPI{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, triggerRoute, strings.NewReader(`{"name":"x"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestTriggerRoute_RefusesNonLoopbackAndBrowsers(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{})
	// A request from another address, with the right token.
	req := httptest.NewRequest(http.MethodPost, triggerRoute, strings.NewReader(`{"name":"backup-done"}`))
	req.RemoteAddr = "192.168.1.20:40000"
	req.Header.Set("Authorization", "Bearer "+triggerTestToken)
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback: status %d", rec.Code)
	}
	for name, hdr := range map[string]map[string]string{
		"origin":          {"Origin": "http://evil.example"},
		"sec-fetch-site":  {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch": {"Sec-Fetch-Site": "same-site"},
	} {
		if got := h.post(`{"name":"backup-done"}`, hdr); got.code != http.StatusForbidden {
			t.Errorf("%s: status %d", name, got.code)
		}
	}
	if st := h.eng.Stats(); st.QueueDepth != 0 {
		t.Fatalf("a refused request reached the pipeline: %+v", st)
	}
}

func TestTriggerRoute_Returns202AndEmitsAnUntrustedEvent(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{run: true})
	got := h.post(jsonBody(t, map[string]any{
		"name": "backup-done", "message": "NAS backup finished with 2 errors", "severity": "critical",
		"data": map[string]any{"errors": 2, "nested": map[string]any{"a": []int{1, 2}}},
	}), nil)
	if got.code != http.StatusAccepted {
		t.Fatalf("status %d: %s", got.code, got.body)
	}
	var resp triggerResponse
	if err := json.Unmarshal([]byte(got.body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.EventID == "" || resp.Status != "accepted" || resp.Severity != "warning" || resp.Mode != config.ProactiveModeLive {
		t.Fatalf("response = %+v", resp)
	}
	waitUntil(t, "the event", func() bool { return len(h.wm.events()) == 1 })
	ev := h.wm.events()[0]
	p, ok := ev.Payload.(perception.ExternalTrigger)
	if !ok {
		t.Fatalf("payload = %T", ev.Payload)
	}
	if ev.ID != resp.EventID || ev.Kind != events.KindPerceptionExternalTrigger || ev.TrustLevel() != proactive.TrustUntrusted ||
		ev.Trust == nil || !ev.Time.Equal(h.clock.Now()) {
		t.Fatalf("event = %+v", ev)
	}
	if p.Name != "backup-done" || p.Message != "NAS backup finished with 2 errors" || p.Severity != "critical" ||
		string(p.Data) != `{"errors":2,"nested":{"a":[1,2]}}` || !p.At.Equal(h.clock.Now()) {
		t.Fatalf("payload = %+v", p)
	}
	if _, private := ev.Payload.(perception.PrivatePayload); !private {
		t.Fatal("the payload is not private")
	}
}

func TestTriggerRoute_ValidatesName(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{run: true})
	valid := []string{"a", "backup-done", "x_1.y-2", "0", strings.Repeat("a", 41)}
	invalid := []string{
		"", "-lead", "_lead", ".lead", "Upper", "has space", "semi;colon", "colon:name", "slash/name",
		"new\nline", "emoji\U0001F600", strings.Repeat("a", 42), "ünï", "a\x00b", "../../etc",
	}
	for _, n := range valid {
		if got := h.post(jsonBody(t, map[string]any{"name": n}), nil); got.code != http.StatusAccepted {
			t.Errorf("name %q: status %d: %s", n, got.code, got.body)
		}
		h.clock.Advance(time.Minute) // keep the rate limit out of this test
	}
	for _, n := range invalid {
		got := h.post(jsonBody(t, map[string]any{"name": n}), nil)
		if got.code != http.StatusBadRequest {
			t.Errorf("name %q: status %d", n, got.code)
		}
		h.clock.Advance(time.Minute)
	}
	// The name field must be there, and a string.
	for _, body := range []string{`{}`, `{"name":null}`, `{"name":5}`, `{"name":["a"]}`} {
		if got := h.post(body, nil); got.code != http.StatusBadRequest {
			t.Errorf("body %s: status %d", body, got.code)
		}
		h.clock.Advance(time.Minute)
	}
	waitUntil(t, "the events of the valid names", func() bool { return len(h.wm.events()) == len(valid) })
	time.Sleep(20 * time.Millisecond)
	if n := len(h.wm.events()); n != len(valid) {
		t.Fatalf("%d events reached the pipeline, want %d", n, len(valid))
	}
}

func TestTriggerRoute_ValidationBounds(t *testing.T) {
	deep := func(n int) string { return strings.Repeat(`{"a":`, n) + `1` + strings.Repeat(`}`, n) }
	bigData := func(n int) string { return `{"k":"` + strings.Repeat("x", n) + `"}` }
	// {"k":"..."} is 8 bytes of overhead: 4096 in all is the limit.
	cases := []struct {
		name string
		body string
		want int
	}{
		{"message at the limit", `{"name":"a","message":"` + strings.Repeat("x", 2000) + `"}`, 202},
		{"message over the limit", `{"name":"a","message":"` + strings.Repeat("x", 2001) + `"}`, 400},
		{"message in characters not bytes", `{"name":"a","message":"` + strings.Repeat("é", 2000) + `"}`, 202},
		{"message over in wide characters", `{"name":"a","message":"` + strings.Repeat("\U0001F600", 2001) + `"}`, 400},
		{"message with a newline", `{"name":"a","message":"one\ntwo\r\n\tthree"}`, 202},
		{"message with escape", `{"name":"a","message":"x\u001b[31m"}`, 400},
		{"message with NUL", `{"name":"a","message":"x\u0000y"}`, 400},
		{"message with RLO", `{"name":"a","message":"pay \u202e100"}`, 400},
		{"message with LRE", `{"name":"a","message":"x\u202ay"}`, 400},
		{"message with PDF", `{"name":"a","message":"x\u202cy"}`, 400},
		{"message with an isolate", `{"name":"a","message":"x\u2066y\u2069"}`, 400},
		{"message with LRM", `{"name":"a","message":"x\u200ey"}`, 400},
		{"message with RLM", `{"name":"a","message":"x\u200fy"}`, 400},
		{"message with ALM", `{"name":"a","message":"x\u061cy"}`, 400},
		{"message with a ZWJ emoji", `{"name":"a","message":"\ud83d\udc68\u200d\ud83d\udc69 ok"}`, 202},
		{"message with a ZWNJ", `{"name":"a","message":"a\u200cb"}`, 202},
		{"message not a string", `{"name":"a","message":5}`, 400},
		{"data at the limit", `{"name":"a","data":` + bigData(4096-8) + `}`, 202},
		{"data over the limit", `{"name":"a","data":` + bigData(4096-7) + `}`, 400},
		{"data depth at the limit", `{"name":"a","data":` + deep(6) + `}`, 202},
		{"data depth over the limit", `{"name":"a","data":` + deep(7) + `}`, 400},
		{"data deep array", `{"name":"a","data":{"a":` + strings.Repeat("[", 40) + strings.Repeat("]", 40) + `}}`, 400},
		{"brackets in a string are not depth", `{"name":"a","data":{"a":"` + strings.Repeat("[{", 50) + `"}}`, 202},
		{"data null", `{"name":"a","data":null}`, 202},
		{"data an array", `{"name":"a","data":[1,2]}`, 400},
		{"data a string", `{"name":"a","data":"x"}`, 400},
		{"data a number", `{"name":"a","data":5}`, 400},
		{"unknown field", `{"name":"a","admin":true}`, 400},
		{"unknown field in different case", `{"name":"a","Password":"x"}`, 400},
		{"not JSON", `name=a&message=b`, 400},
		{"empty body", ``, 400},
		{"truncated JSON", `{"name":"a"`, 400},
		{"an array", `["a"]`, 400},
		{"trailing object", `{"name":"a"}{"name":"b"}`, 400},
		{"trailing garbage", `{"name":"a"} garbage`, 400},
		{"severity warning", `{"name":"a","severity":"warning"}`, 202},
		{"severity critical is capped, not refused", `{"name":"a","severity":"critical"}`, 202},
		{"severity unknown", `{"name":"a","severity":"urgent"}`, 400},
		{"severity not a string", `{"name":"a","severity":3}`, 400},
		{"body over the cap", `{"name":"a","message":"` + strings.Repeat("x", maxTriggerBody) + `"}`, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggerHarness(t, harnessOpts{run: true})
			got := h.post(tc.body, nil)
			if got.code != tc.want {
				t.Fatalf("status %d (want %d): %.200s", got.code, tc.want, got.body)
			}
			if tc.want != 202 {
				// A refusal says what was wrong and never repeats what was sent.
				if strings.Contains(got.body, "xxxxxxxx") || strings.Contains(got.body, "garbage") || strings.Contains(got.body, "urgent") {
					t.Fatalf("the refusal echoes the request: %.200s", got.body)
				}
				if st := h.eng.Stats(); st.QueueDepth+int(st.Processed) != 0 {
					t.Fatalf("a refused request reached the pipeline: %+v", st)
				}
			}
		})
	}
}

// A request that is too large is read up to the cap and no further.
func TestTriggerRoute_BodyIsLimitedBeforeParsing(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{})
	got := h.post(`{"name":"a","message":"`+strings.Repeat("x", 10<<20)+`"}`, nil)
	if got.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", got.code)
	}
}

func TestTriggerRoute_RateLimit(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{run: true})
	body := `{"name":"a"}`
	for i := 0; i < triggerBurst; i++ {
		if got := h.post(body, nil); got.code != http.StatusAccepted {
			t.Fatalf("request %d: status %d", i, got.code)
		}
	}
	got := h.post(body, nil)
	if got.code != http.StatusTooManyRequests {
		t.Fatalf("status %d after the burst", got.code)
	}
	secs, err := strconv.Atoi(got.header.Get("Retry-After"))
	if err != nil || secs < 1 || secs > 3 { // 30 a minute is one every 2 seconds
		t.Fatalf("Retry-After = %q", got.header.Get("Retry-After"))
	}
	// A refused request does not spend a token or reach the pipeline.
	h.clock.Advance(2 * time.Second)
	if got := h.post(body, nil); got.code != http.StatusAccepted {
		t.Fatalf("after 2s: status %d", got.code)
	}
	if got := h.post(body, nil); got.code != http.StatusTooManyRequests {
		t.Fatalf("a second request in the same instant: status %d", got.code)
	}
	// A minute refills the bucket to the burst, and no further.
	h.clock.Advance(time.Hour)
	for i := 0; i < triggerBurst; i++ {
		if got := h.post(body, nil); got.code != http.StatusAccepted {
			t.Fatalf("after an hour, request %d: status %d", i, got.code)
		}
	}
	if got := h.post(body, nil); got.code != http.StatusTooManyRequests {
		t.Fatalf("after an hour: the bucket held more than the burst (status %d)", got.code)
	}
}

func TestTriggerLimiter_Rates(t *testing.T) {
	clock := perceptiontest.NewClock(time.Unix(1000, 0))
	l := newTriggerLimiter(triggerRatePerMinute, triggerBurst, clock.Now)
	for i := 0; i < triggerBurst; i++ {
		if ok, _ := l.allow(); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := l.allow()
	if ok || wait != 2*time.Second {
		t.Fatalf("allowed=%v wait=%v", ok, wait)
	}
	clock.Advance(time.Second)
	if ok, wait := l.allow(); ok || wait != time.Second {
		t.Fatalf("after 1s: allowed=%v wait=%v", ok, wait)
	}
	clock.Advance(time.Second)
	if ok, _ := l.allow(); !ok {
		t.Fatal("refused after the refill")
	}
}

// A full inbox is a 503 at once; the handler never waits for room.
func TestTriggerRoute_FullInboxIs503AndNeverBlocks(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{inbox: 2}) // the engine is not running: nothing drains
	body := `{"name":"a"}`
	for i := 0; i < 2; i++ {
		if got := h.post(body, nil); got.code != http.StatusAccepted {
			t.Fatalf("request %d: status %d", i, got.code)
		}
	}
	done := make(chan triggerReply, 1)
	go func() { done <- h.post(body, nil) }()
	select {
	case got := <-done:
		if got.code != http.StatusServiceUnavailable || got.header.Get("Retry-After") == "" {
			t.Fatalf("status %d, Retry-After %q", got.code, got.header.Get("Retry-After"))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler blocked on a full inbox")
	}
	if st := h.eng.Stats(); st.DroppedNonBlocking != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestTriggerRoute_409WhileThereIsNothingToTrigger(t *testing.T) {
	for name, o := range map[string]harnessOpts{
		"no pipeline":         {noRuntime: true},
		"triggers are off":    {noRegistry: true},
		"no pipeline, no reg": {noRuntime: true, noRegistry: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newTriggerHarness(t, o)
			got := h.post(`{"name":"a"}`, nil)
			if got.code != http.StatusConflict || !strings.Contains(got.body, `"error"`) {
				t.Fatalf("status %d: %s", got.code, got.body)
			}
			if st := h.eng.Stats(); st.QueueDepth != 0 {
				t.Fatalf("stats = %+v", st)
			}
			// It does not spend the rate limit either.
			if ok, _ := h.api.limiter.allow(); !ok {
				t.Fatal("limiter empty")
			}
		})
	}
}

// Nothing from the request reaches a log, at any level, but the name and sizes.
func TestTriggerRoute_NothingFromTheRequestIsLogged(t *testing.T) {
	prev := logger.GetLevel()
	logger.SetLevel(logger.DEBUG)
	var buf syncBuffer
	logger.SetAdditionalWriter(&buf)
	t.Cleanup(func() {
		logger.SetAdditionalWriter(nil)
		logger.SetLevel(prev)
	})

	h := newTriggerHarness(t, harnessOpts{run: true})
	const secret = "LOGGED-SECRET-5521"
	h.post(jsonBody(t, map[string]any{"name": "backup-done", "message": secret, "data": map[string]any{"k": secret}}), nil)
	h.post(jsonBody(t, map[string]any{"name": "bad name", "message": secret}), nil)
	h.post(`{"name":"a","message":"`+secret+`","extra":"`+secret+`"}`, nil)
	h.post(`{"name":"a","message":"`+secret+`"`, nil)
	waitUntil(t, "the event", func() bool { return len(h.wm.events()) == 1 })
	// Let the engine finish with it.
	waitUntil(t, "the pipeline", func() bool { return h.eng.Stats().Processed == 1 })
	logs := buf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("the request's text reached a log:\n%s", logs)
	}
	if !strings.Contains(logs, "backup-done") || !strings.Contains(logs, "message_bytes") {
		t.Fatalf("the name and sizes were not logged:\n%s", logs)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- the route in the real assembly ------------------------------------------

func postTrigger(t *testing.T, f *assemblyFixture, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url(triggerRoute), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// With the default configuration the route does not exist; with perception on and
// triggers off it answers 409; with triggers on it takes the request and the
// builtin spec is there; and it all comes back after a reload.
func TestTriggerRoute_InTheAssembly(t *testing.T) {
	t.Run("default config: no route", func(t *testing.T) {
		f := newAssemblyFixture(t, nil)
		if code, _ := postTrigger(t, f, testMgmtToken, `{"name":"a"}`); code != http.StatusNotFound {
			t.Fatalf("status %d", code)
		}
		if triggersTable(t, f) {
			t.Fatal("the triggers table exists")
		}
	})

	t.Run("perception on, triggers off: 409", func(t *testing.T) {
		useSources(t, false, &countingSource{name: "fake"})
		f := newAssemblyFixture(t, liveWithPerception())
		code, body := postTrigger(t, f, testMgmtToken, `{"name":"a"}`)
		if code != http.StatusConflict || !strings.Contains(body, "triggers are off") {
			t.Fatalf("status %d: %s", code, body)
		}
		if code, _ := postTrigger(t, f, "", `{"name":"a"}`); code != http.StatusUnauthorized {
			t.Fatalf("without a token: status %d", code)
		}
		if triggersTable(t, f) {
			t.Fatal("the triggers table exists")
		}
	})

	t.Run("triggers shadow: accepted, logged, never delivered; survives a reload", func(t *testing.T) {
		useSources(t, false, &countingSource{name: "fake"})
		f := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)))
		rt := f.services.Perception
		if rt == nil || rt.triggers == nil {
			t.Fatal("the registry was not built")
		}
		specs, _ := rt.triggers.List(context.Background(), triggers.ListFilter{Kind: triggers.KindExternal})
		if len(specs) != 1 || specs[0].ID != triggers.ExternalSpecID || specs[0].Mode != triggers.ModeLive || specs[0].Origin != "builtin" {
			t.Fatalf("specs = %+v", specs)
		}

		check := func(label string) {
			t.Helper()
			code, body := postTrigger(t, f, testMgmtToken, `{"name":"backup-done","message":"NAS backup finished","severity":"critical"}`)
			if code != http.StatusAccepted {
				t.Fatalf("%s: status %d: %s", label, code, body)
			}
			var resp triggerResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Mode != config.ProactiveModeShadow || resp.Severity != "warning" {
				t.Fatalf("%s: response %s (%v)", label, body, err)
			}
		}
		check("first build")
		db := fixtureDB(t, f)
		waitUntil(t, "the decision", func() bool {
			var n int
			_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE trigger_id = 'external:builtin'`).Scan(&n)
			return n == 1
		})
		var disp, reason string
		var shadow, sev int
		if err := db.QueryRow(`SELECT disposition, reason, shadow, severity FROM attention_decisions WHERE trigger_id = 'external:builtin'`).
			Scan(&disp, &reason, &shadow, &sev); err != nil {
			t.Fatal(err)
		}
		if disp != string(proactive.DispLog) || reason != "shadow" || shadow != 1 || proactive.Severity(sev) != proactive.SeverityWarning {
			t.Fatalf("decision = %s/%s shadow=%d severity=%d", disp, reason, shadow, sev)
		}
		if st := rt.engine.Stats(); st.StageErrors != 0 || st.StagePanics != 0 {
			t.Fatalf("engine stats = %+v", st)
		}

		// A reload rebuilds the pipeline and the mux, and the route is back; the
		// builtin spec is not added twice.
		if err := f.reload(compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow))); err != nil {
			t.Fatal(err)
		}
		check("after a reload")
		specs, _ = f.services.Perception.triggers.List(context.Background(), triggers.ListFilter{Kind: triggers.KindExternal})
		if len(specs) != 1 {
			t.Fatalf("specs after a reload = %+v", specs)
		}

		// Switching perception off removes the route.
		if err := f.reload(nil); err != nil {
			t.Fatal(err)
		}
		if code, _ := postTrigger(t, f, testMgmtToken, `{"name":"a"}`); code != http.StatusNotFound {
			t.Fatalf("with perception off: status %d", code)
		}
	})
}

// The limiter is per process: a reload does not refill it.
func TestTriggerRoute_RateLimitSurvivesAReload(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	mut := compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow))
	f := newAssemblyFixture(t, mut)
	for i := 0; i < triggerBurst; i++ {
		if code, body := postTrigger(t, f, testMgmtToken, `{"name":"a"}`); code != http.StatusAccepted {
			t.Fatalf("request %d: status %d: %s", i, code, body)
		}
	}
	if err := f.reload(mut); err != nil {
		t.Fatal(err)
	}
	if code, _ := postTrigger(t, f, testMgmtToken, `{"name":"a"}`); code != http.StatusTooManyRequests {
		t.Fatalf("after a reload: status %d, want 429", code)
	}
}

// The bidirectional controls the route refuses get a fixed text that names the kind
// of character and not the character.
func TestTriggerRoute_BidiRefusalText(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{})
	got := h.post(`{"name":"a","message":"pay \u202e100"}`, nil)
	if got.code != http.StatusBadRequest || !strings.Contains(got.body, "bidirectional control") || strings.Contains(got.body, "\u202e") {
		t.Fatalf("status %d: %s", got.code, got.body)
	}
}

// The hourly bucket: 300 accepted requests an hour, refilled evenly, checked together
// with the short one; a refusal takes from neither and the Retry-After is the larger
// wait.
func TestTriggerLimiter_HourlyCap(t *testing.T) {
	clock := perceptiontest.NewClock(time.Unix(1_000_000, 0))
	l := newTriggerLimiter(triggerRatePerMinute, triggerBurst, clock.Now)

	// One request every 2 seconds is the short bucket's own steady rate, so only the
	// hourly bucket (300, refilled 1 in 12 s) can stop the run: it holds 300 and gains
	// 1 for every 6 requests, so it is empty after 360.
	accepted := 0
	for ; accepted < 1000; accepted++ {
		ok, wait := l.allow()
		if !ok {
			l.mu.Lock()
			shortHasRoom := l.b[0].tokens >= 1
			l.mu.Unlock()
			if wait <= 0 || wait > 12*time.Second || !shortHasRoom {
				t.Fatalf("refused with a wait of %v (short bucket has room: %v): the hourly bucket should govern", wait, shortHasRoom)
			}
			break
		}
		clock.Advance(2 * time.Second)
	}
	if accepted < 355 || accepted > 362 {
		t.Fatalf("%d accepted before the hourly cap, want about 360", accepted)
	}

	// An hour later the hourly bucket is full again (and no fuller).
	clock.Advance(time.Hour)
	n := 0
	for i := 0; i < 1000; i++ {
		if ok, _ := l.allow(); ok {
			n++
		}
		clock.Advance(2 * time.Second)
		if n >= 361 {
			break
		}
	}
	if n < 355 {
		t.Fatalf("%d accepted after an hour", n)
	}
}

// The larger wait is the one reported, and a refusal consumes from neither bucket.
func TestTriggerLimiter_RefusalConsumesNothingAndReportsTheLargerWait(t *testing.T) {
	clock := perceptiontest.NewClock(time.Unix(2_000_000, 0))
	l := newTriggerLimiterWithHourly(30, 10, 20, clock.Now) // a small hourly cap to reach it

	for i := 0; i < 10; i++ {
		if ok, _ := l.allow(); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	// The short bucket is empty (2 s a token); the hourly one has 10 left (180 s a token).
	if ok, wait := l.allow(); ok || wait != 2*time.Second {
		t.Fatalf("allowed=%v wait=%v, want the short bucket's 2s", ok, wait)
	}
	l.mu.Lock()
	hourBefore := l.b[1].tokens
	l.mu.Unlock()
	for i := 0; i < 5; i++ {
		l.allow() // refused: takes from neither
	}
	l.mu.Lock()
	if l.b[1].tokens != hourBefore {
		t.Fatalf("a refusal took from the hourly bucket: %v -> %v", hourBefore, l.b[1].tokens)
	}
	l.mu.Unlock()

	// Drain the hourly bucket while the short one refills.
	for i := 0; i < 10; i++ {
		clock.Advance(2 * time.Second)
		if ok, _ := l.allow(); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	clock.Advance(20 * time.Second) // the short bucket is full again; the hourly one is not
	ok, wait := l.allow()
	if ok || wait < 100*time.Second {
		t.Fatalf("allowed=%v wait=%v, want the hourly bucket's wait", ok, wait)
	}
}

// Through the route: the hourly refusal is a 429 with the larger Retry-After.
func TestTriggerRoute_HourlyCapIs429(t *testing.T) {
	h := newTriggerHarness(t, harnessOpts{run: true})
	h.api.limiter = newTriggerLimiterWithHourly(triggerRatePerMinute, triggerBurst, 3, h.clock.Now)
	body := `{"name":"a"}`
	for i := 0; i < 3; i++ {
		if got := h.post(body, nil); got.code != http.StatusAccepted {
			t.Fatalf("request %d: status %d", i, got.code)
		}
		h.clock.Advance(2 * time.Second)
	}
	got := h.post(body, nil)
	secs, err := strconv.Atoi(got.header.Get("Retry-After"))
	if got.code != http.StatusTooManyRequests || err != nil || secs < 1000 { // 1200 s a token at 3 an hour
		t.Fatalf("status %d, Retry-After %q", got.code, got.header.Get("Retry-After"))
	}
}

// The hourly bucket is per process like the short one: a reload keeps it.
func TestTriggerRoute_HourlyCapSurvivesAReload(t *testing.T) {
	r := newSharedResources()
	t.Cleanup(func() { _ = r.Close() })
	if r.triggerLimiter() != r.triggerLimiter() {
		t.Fatal("the shared limiter is not one object")
	}
	l := r.triggerLimiter()
	l.mu.Lock()
	l.b[1].tokens = 7
	l.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.triggerLimiter().b[1].tokens != 7 {
		t.Fatal("the hourly bucket was reset")
	}
}
