package fault

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binance"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// httpRig is the lab's HTTP path in miniature: a real sandbox server whose handler wrap and
// StageFault belong to the injector, the simulator behind them, and the recorded fixtures.
type httpRig struct {
	srv *sandbox.Server
	sim *sandbox.StatefulSimulator
	inj *Injector
	j   *memJournal
}

func newHTTPRig(t *testing.T, maxHold time.Duration) *httpRig {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(repoPath(t, "workspace/sandbox")); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	sim := sandbox.NewStatefulSimulator(sm)
	inj, _, j := newInj(t, Options{MaxHold: maxHold})
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageFault, inj.Middleware())
	chain.Set(sandbox.StageOrders, sandbox.AsMiddleware(sim))
	srv := sandbox.NewServer(store)
	srv.SetResponder(chain)
	srv.SetHandlerWrap(inj.HandlerWrap())
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inj.Close(); _ = srv.Stop(); broker.ResetInstanceCache() })
	return &httpRig{srv: srv, sim: sim, inj: inj, j: j}
}

func (r *httpRig) orders() (n int) {
	r.sim.Read("binance", func(vs *sandbox.VenueState) { n = len(vs.OpenOrders) + len(vs.ClosedOrders) })
	return n
}

func (r *httpRig) add(t *testing.T, rule Rule) string {
	t.Helper()
	id, err := r.inj.Add(rule)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// get is a plain GET through the sandbox address of venue.
func (r *httpRig) get(venue, host, path string) (int, string, error) {
	resp, err := http.Get(r.srv.BaseURL() + "/__sbx__/" + venue + "/" + host + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// place sends a futures market buy through the real Binance adapter and
// broker.PlaceOrderIdempotent, the path the agent's tools take.
func (r *httpRig) place(t *testing.T) error {
	t.Helper()
	cfg := &config.Config{}
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{{Name: "t", APIKey: *config.NewSecureString("sbx-test-api-key-0001"), Secret: *config.NewSecureString("sbx-test-secret-0001")}}
	p, err := broker.CreateProviderForAccount("binance", "t", cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.PlaceOrderIdempotent(context.Background(), p, broker.OrderRequest{
		Market: broker.OrderMarketFutures, Symbol: "BTC/USDT:USDT", Type: "market", Side: "buy", Amount: 0.001, PositionSide: "long",
	}, "idem-1", 1, time.Now())
	return err
}

// until yields until cond holds; a hold has no signal of its own to wait on. It gives up
// after a generous bound so a bug fails the test instead of hanging it.
func until(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		runtime.Gosched()
	}
}

var createFutures = Match{Venue: "binance", Order: "create", Market: "futures"}

// A request no rule matches is served as if there were no injector, and a status rule
// answers in the venue's shape; one request fires one rule, in insertion order.
func TestHTTP_StatusBodyAndOneRulePerRequest(t *testing.T) {
	r := newHTTPRig(t, time.Second)
	if code, _, err := r.get("okx", "www.okx.com", "/api/v5/public/time"); err != nil || code == 503 {
		t.Fatalf("an unmatched request = %d, %v", code, err)
	}
	first := r.add(t, Rule{Match: Match{Venue: "okx", PathPrefix: "/api/v5/public"}, Effect: Effect{Status: 503, Body: json.RawMessage(`{"code":"50001"}`)}})
	second := r.add(t, Rule{Match: Match{Venue: "okx"}, Effect: Effect{Error: "rate_limit"}})
	code, body, err := r.get("okx", "www.okx.com", "/api/v5/public/time")
	if err != nil || code != 503 || body != `{"code":"50001"}` {
		t.Fatalf("status rule = %d %q %v", code, body, err)
	}
	if code, body, _ = r.get("okx", "www.okx.com", "/api/v5/market/tickers"); code != 429 || !strings.Contains(body, "50011") {
		t.Fatalf("library rule = %d %q", code, body)
	}
	fired := map[string]int{}
	for _, s := range r.inj.List() {
		fired[s.Rule.ID] = s.Fired
	}
	if fired[first] != 1 || fired[second] != 1 {
		t.Fatalf("firings = %v, want one each: the first rule must win the first request alone", fired)
	}
	es := r.j.entries()
	if len(es) != 2 || !strings.Contains(string(es[0].Payload), `"path":"/api/v5/public/time"`) || !strings.Contains(string(es[0].Payload), `"rule_id":"`+first+`"`) {
		t.Fatalf("journal = %+v, want one fault.fired per firing with the rule and path", es)
	}
}

// A timeout rule: the client errors, and the simulator has no new order.
func TestTimeout_NothingCreated(t *testing.T) {
	r := newHTTPRig(t, 150*time.Millisecond)
	r.add(t, Rule{Match: createFutures, Effect: Effect{Timeout: true}})
	err := r.place(t)
	if err == nil || !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatalf("PlaceOrderIdempotent = %v, want ErrOutcomeUnknown", err)
	}
	if n := r.orders(); n != 0 {
		t.Fatalf("the simulator holds %d orders after a timeout, want none", n)
	}
}

// written_then_timeout: the simulator creates the order, the answer is lost, and the broker
// reports an unknown outcome that is safe to retry.
func TestWrittenThenTimeout_OrderExistsClientUnknown(t *testing.T) {
	r := newHTTPRig(t, 150*time.Millisecond)
	id := r.add(t, Rule{Match: Match{Venue: "binance", Market: "futures"}, Effect: Effect{Order: "written_then_timeout"}, When: When{Times: 1}})
	err := r.place(t)
	if !errors.Is(err, broker.ErrOutcomeUnknown) || !broker.Retryable(err) {
		t.Fatalf("PlaceOrderIdempotent = %v, want a retryable ErrOutcomeUnknown", err)
	}
	if n := r.orders(); n != 1 {
		t.Fatalf("the simulator holds %d orders, want exactly 1", n)
	}
	if s := r.inj.List()[0]; s.Rule.ID != id || s.Fired != 1 || s.Rule.Match.Order != "create" {
		t.Fatalf("rule state = %+v, want one firing and order defaulted to create", s)
	}
}

// drop_connection: a ccxt NetworkError, which the broker treats as an unknown outcome.
func TestDropConnection_NetworkError(t *testing.T) {
	r := newHTTPRig(t, time.Second)
	r.add(t, Rule{Match: createFutures, Effect: Effect{DropConnection: true}})
	err := r.place(t)
	// The broker's own error names the cause by its class ("ccxt NetworkError").
	if err == nil || !strings.Contains(err.Error(), "ccxt NetworkError") {
		t.Fatalf("error = %v, want a ccxt NetworkError", err)
	}
	if !broker.Retryable(err) {
		t.Fatalf("a dropped connection must be retryable: %v", err)
	}
	if n := r.orders(); n != 0 {
		t.Fatalf("%d orders after a connection dropped before the request was served", n)
	}
}

// reject: the exchange's own definite refusal, returned unchanged, with no order made.
func TestReject_DefiniteRejection(t *testing.T) {
	r := newHTTPRig(t, time.Second)
	r.add(t, Rule{Match: createFutures, Effect: Effect{Order: "reject", Code: "invalid_order"}})
	err := r.place(t)
	if err == nil || errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatalf("PlaceOrderIdempotent = %v, want the exchange error, not an unknown outcome", err)
	}
	if typ, _ := broker.CCXTErrorType(err); typ != "InvalidOrder" || broker.Retryable(err) {
		t.Fatalf("error class = %q retryable = %v (%v), want InvalidOrder and not retryable", typ, broker.Retryable(err), err)
	}
	if r.orders() != 0 {
		t.Fatal("a rejected order was created")
	}
	// reject touches only order operations: a non-order request passes.
	if code, _, _ := r.get("binance", "fapi.binance.com", "/fapi/v1/ping"); code == 400 {
		t.Fatal("a reject rule answered a request that is not an order")
	}
}

// A timeout against a client with no deadline of its own is closed after MaxHold, and
// the hold is visible to the server's and the injector's Idle meanwhile.
func TestTimeout_Capped(t *testing.T) {
	r := newHTTPRig(t, 200*time.Millisecond)
	r.add(t, Rule{Match: Match{Venue: "okx"}, Effect: Effect{Timeout: true}, When: When{Times: 1}})
	start := time.Now()
	type res struct{ err error }
	done := make(chan res, 1)
	go func() {
		_, _, err := r.get("okx", "www.okx.com", "/api/v5/public/time")
		done <- res{err}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a timed-out request got an answer")
		}
		if d := time.Since(start); d < 200*time.Millisecond {
			t.Fatalf("closed after %s, before MaxHold", d)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the hold was not capped by MaxHold")
	}
	if !r.inj.Idle() {
		t.Fatal("busy after the hold ended")
	}
}

// While a request is held the server and the injector are busy, so a settle waits; when the
// client goes away the hold ends and both are idle again.
func TestTimeout_HoldIsVisibleToIdle(t *testing.T) {
	r := newHTTPRig(t, time.Minute)
	r.add(t, Rule{Match: Match{Venue: "okx"}, Effect: Effect{Timeout: true}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.BaseURL()+"/__sbx__/okx/www.okx.com/api/v5/public/time", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	until(t, func() bool { return !r.inj.Idle() })
	if r.srv.Idle() || r.inj.Idle() {
		t.Fatal("not busy while a request is held")
	}
	cancel()
	<-done
	until(t, func() bool { return r.inj.Idle() && r.srv.Idle() })
}

// latency is wall time: the answer comes after at least that long, and a latency above
// MaxHold is refused at Add.
func TestLatency_IsWallAndBounded(t *testing.T) {
	r := newHTTPRig(t, time.Second)
	if _, err := r.inj.Add(Rule{Match: Match{Venue: "okx"}, Effect: Effect{Latency: Duration(2 * time.Second)}}); err == nil {
		t.Fatal("a latency above MaxHold was accepted")
	}
	r.add(t, Rule{Match: Match{Venue: "okx", PathPrefix: "/api/v5/public"}, Effect: Effect{Latency: Duration(200 * time.Millisecond)}})
	start := time.Now()
	if code, _, err := r.get("okx", "www.okx.com", "/api/v5/public/time"); err != nil || code == 0 {
		t.Fatalf("a delayed request = %d, %v", code, err)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("answered after %s, want at least 200ms", d)
	}
}

// A writer that cannot hijack gets a bodiless 504, never a panic.
func TestCloseConn_WithoutHijacker(t *testing.T) {
	i, _, _ := newInj(t, Options{})
	if _, err := i.Add(Rule{Match: Match{Venue: "okx"}, Effect: Effect{DropConnection: true}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/__sbx__/okx/www.okx.com/api/v5/public/time", nil)
	i.HandlerWrap()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the router ran for a dropped request") })).ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout || rec.Body.Len() != 0 {
		t.Fatalf("got %d with %d body bytes, want a bodiless 504", rec.Code, rec.Body.Len())
	}
	// A path that is not a sandbox address passes through untouched.
	called := false
	i.HandlerWrap()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	if !called {
		t.Fatal("a request that is not a sandbox address was not passed on")
	}
}

// The hold, in the injector alone: visible to Idle, ended by its context, by MaxHold and by Close.
func TestHold_IdleWhileHeldThenReleased(t *testing.T) {
	i, _, _ := newInj(t, Options{MaxHold: time.Minute})
	if !i.Idle() || !i.Idle() {
		t.Fatal("a fresh injector is idle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- i.Hold(ctx, time.Minute) }()
	until(t, func() bool { return !i.Idle() })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || !i.Idle() {
		t.Fatalf("Hold ended with %v, idle %v", err, i.Idle())
	}
	done = make(chan error, 1)
	go func() { done <- i.Hold(context.Background(), time.Minute) }()
	until(t, func() bool { return !i.Idle() })
	_ = i.Close()
	if err := <-done; err == nil {
		t.Fatal("Close did not release the hold")
	}
	if err := i.Hold(context.Background(), time.Minute); err == nil {
		t.Fatal("a hold after Close should return at once")
	}
	capped, _, _ := newInj(t, Options{MaxHold: 20 * time.Millisecond})
	if err := capped.Hold(context.Background(), time.Hour); err != nil {
		t.Fatalf("a hold past MaxHold = %v, want nil after MaxHold", err)
	}
}
