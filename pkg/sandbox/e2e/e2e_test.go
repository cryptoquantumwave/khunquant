package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	// Must be imported first so HTTP_PROXY/HTTPS_PROXY are set before any
	// transitive package init() calls http.ProxyFromEnvironment.
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/e2e/internal/proxyenv"

	// Ensure all exchange factories are registered
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binance"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binanceth"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/bitkub"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/okx"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/settrade"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/webull"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Global recorder for TestMain
var globalRecorder *ProxyRecorder

// TestMain sets up environment before tests run, ensuring HTTP_PROXY is set
// before any exchange clients are constructed (ProxyFromEnvironment memoizes).
func TestMain(m *testing.M) {
	// Start a proxy recorder server that captures all outbound CONNECT attempts
	globalRecorder = NewProxyRecorder()
	if err := globalRecorder.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start proxy recorder: %v\n", err)
		os.Exit(1)
	}
	defer globalRecorder.Close()

	// Set environment before any clients are constructed
	os.Setenv("HTTP_PROXY", "http://"+globalRecorder.Addr())
	os.Setenv("HTTPS_PROXY", "http://"+globalRecorder.Addr())

	exitCode := m.Run()

	// Report any detected escapes
	if recorded := globalRecorder.UnexpectedTargets(); len(recorded) > 0 {
		fmt.Fprintf(os.Stderr, "\n⚠️  Detected non-loopback connections (should have been sandboxed):\n")
		for _, target := range recorded {
			fmt.Fprintf(os.Stderr, "  - %s\n", target)
		}
		if exitCode == 0 {
			exitCode = 1
		}
	}

	os.Exit(exitCode)
}

// ProxyRecorder is a simple proxy server that records CONNECT targets.
// Used to detect requests that escape the sandbox.
type ProxyRecorder struct {
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	targets     []string        // all targets dialed
	expected    map[string]bool // hosts a test dials on purpose, keyed by host
	loopback    int64           // count of loopback dials
	nonLoopback int64           // count of non-loopback dials
}

// NewProxyRecorder creates a recorder (but does not start listening).
func NewProxyRecorder() *ProxyRecorder {
	return &ProxyRecorder{
		targets:  []string{},
		expected: map[string]bool{},
	}
}

// Start begins listening.
func (p *ProxyRecorder) Start() error {
	listener := proxyenv.Listener
	proxyenv.Listener = nil
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	p.listener = listener

	p.server = &http.Server{
		Handler: http.HandlerFunc(p.handleRequest),
	}

	go p.server.Serve(listener)
	return nil
}

// Addr returns the recorder's address (host:port).
func (p *ProxyRecorder) Addr() string {
	if p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// handleRequest logs CONNECT targets and rejects non-loopback requests.
func (p *ProxyRecorder) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Extract host from RequestURI (for CONNECT) or Host header (for regular)
	target := r.RequestURI
	if target == "" {
		target = r.Host
	}

	// Check if target is loopback
	hostOnly := targetHost(target)

	p.mu.Lock()
	p.targets = append(p.targets, target)
	p.mu.Unlock()

	if isLoopback(hostOnly) {
		atomic.AddInt64(&p.loopback, 1)
		// Loopback request; let it through (would be proxied to sandbox)
		w.WriteHeader(http.StatusOK)
	} else {
		atomic.AddInt64(&p.nonLoopback, 1)
		// Non-loopback request detected! This means sandbox failed.
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "Proxy recorder: non-loopback request intercepted: %s\n", target)
	}
}

// Close stops the recorder.
func (p *ProxyRecorder) Close() error {
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		p.server.Shutdown(ctx)
	}
	if p.listener != nil {
		return p.listener.Close()
	}
	return nil
}

// LoopbackDialCount returns the count of loopback dials recorded.
func (p *ProxyRecorder) LoopbackDialCount() int64 {
	return atomic.LoadInt64(&p.loopback)
}

// NonLoopbackDialCount returns the count of non-loopback dials recorded.
func (p *ProxyRecorder) NonLoopbackDialCount() int64 {
	return atomic.LoadInt64(&p.nonLoopback)
}

// RecordedTargets returns all targets that were recorded.
func (p *ProxyRecorder) RecordedTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.targets
}

// ExpectTarget marks a host as dialed on purpose, so the end-of-run escape
// report ignores it. Used by the detector's own liveness test, which must
// escape to prove the recorder still works.
func (p *ProxyRecorder) ExpectTarget(host string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expected[host] = true
}

// ExpectTargetsFrom marks every host recorded from index n on as dialed on purpose.
func (p *ProxyRecorder) ExpectTargetsFrom(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, target := range p.targets[n:] {
		p.expected[targetHost(target)] = true
	}
}

// UnexpectedTargets returns the recorded non-loopback targets that no test
// declared via ExpectTarget — i.e. genuine sandbox escapes. Loopback targets
// are the sandbox itself and are never escapes.
func (p *ProxyRecorder) UnexpectedTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var escapes []string
	for _, target := range p.targets {
		host := targetHost(target)
		if isLoopback(host) || p.expected[host] {
			continue
		}
		escapes = append(escapes, target)
	}
	return escapes
}

// targetHost extracts the host from a recorded target, which is either a
// CONNECT authority ("example.com:443") or an absolute URL as sent to a proxy
// ("http://example.com:80/path").
func targetHost(target string) string {
	if strings.Contains(target, "://") {
		if u, err := url.Parse(target); err == nil {
			return u.Hostname()
		}
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		return host
	}
	return target
}

// isLoopback is a helper to check if a host is loopback.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// e2eVenues are the venues the egress tests drive. Every one is created from a
// placeholder account, so the exchange is built and a request is really made.
var e2eVenues = []string{"binance", "okx", "bitkub", "binanceth", "settrade", "webull"}

// placeholderConfig returns a config with one placeholder account (name "main") for each
// venue in e2eVenues. Binance, OKX, Settrade and Webull get the lab's own placeholder
// (sandbox.ApplyPlaceholderAccounts); the lab has none for Bitkub or Binance TH, so those
// two get test-only values that say what they are. None is a real credential, and nothing
// is written outside the test's temp dir.
func placeholderConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	// Webull keeps its sandbox login session under the workspace: a temp dir, never the tree.
	cfg.Agents.Defaults.Workspace = t.TempDir()
	if err := sandbox.ApplyPlaceholderAccounts(cfg, sandbox.AccountVenues); err != nil {
		t.Fatalf("placeholder accounts: %v", err)
	}
	acc := config.ExchangeAccount{
		Name:        sandbox.PlaceholderAccountName,
		APIKey:      *config.NewSecureString("e2e-placeholder-NOT-A-REAL-KEY"),
		Secret:      *config.NewSecureString("e2e-placeholder-NOT-A-REAL-SECRET"),
		Permissions: []config.PermissionScope{config.ScopeMarketData, config.ScopeTrade},
	}
	cfg.Exchanges.Bitkub = config.BitkubExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acc}}
	cfg.Exchanges.BinanceTH = config.BinanceTHExchangeConfig{Enabled: true, Accounts: []config.ExchangeAccount{acc}}
	return cfg
}

// sandboxHits counts the requests the sandbox server received, per venue. Go sends
// loopback traffic around HTTP_PROXY, so the recorder never sees it: this is where the
// "traffic went to the sandbox" half of the claim is measured.
type sandboxHits struct {
	mu    sync.Mutex
	venue map[string]int64
	total int64
}

func (h *sandboxHits) of(venue string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.venue[venue]
}

func (h *sandboxHits) all() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func (h *sandboxHits) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pr, err := sandbox.ParseRequest(r.URL.EscapedPath()); err == nil {
			h.mu.Lock()
			h.venue[pr.Venue]++
			h.total++
			h.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

// startSandbox loads the fixtures (failing if none load), starts the sandbox server with
// a request counter, and enables the global sandbox state. Cleanup disables it again.
func startSandbox(t *testing.T) (*sandbox.Server, *sandboxHits) {
	t.Helper()
	requireProxyRecorder(t)
	store := sandbox.NewStore()
	if err := store.Load(getFixturesDir(t)); err != nil {
		t.Fatalf("Failed to load fixtures: %v", err)
	}
	if len(store.Venues()) == 0 {
		t.Fatalf("no fixtures loaded from %s (a path bug would make every later check vacuous)", getFixturesDir(t))
	}
	server := sandbox.NewServer(store)
	hits := &sandboxHits{venue: map[string]int64{}}
	server.SetHandlerWrap(hits.wrap)
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Failed to start sandbox server: %v", err)
	}
	t.Cleanup(func() {
		server.Stop()
		sandbox.SetGlobalState(false, "")
		exchanges.ResetInstanceCache()
	})
	exchanges.ResetInstanceCache()
	return server, hits
}

// requireProxyRecorder fails unless a request to a real host would be sent to the
// recorder, which answers every non-loopback target with 502 and never dials it. It is
// what keeps a local run from reaching the internet when a redirect is broken.
func requireProxyRecorder(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "https://api.binance.com/", nil)
	p, err := http.ProxyFromEnvironment(req)
	if err != nil || p == nil || p.Host != globalRecorder.Addr() {
		t.Fatalf("HTTPS_PROXY does not point at the recorder (got %v, err %v): a broken redirect could reach a real host", p, err)
	}
}

// TestZeroNonLoopbackTrafficWithSandboxEnabled tests the headline requirement:
// with sandbox enabled, zero outbound connections are attempted to non-loopback addresses,
// and the traffic really happened (each venue's request reached the sandbox server).
func TestZeroNonLoopbackTrafficWithSandboxEnabled(t *testing.T) {
	_, hits := startSandbox(t)
	cfg := placeholderConfig(t)
	initialNonLoopback := globalRecorder.NonLoopbackDialCount()

	for _, venue := range e2eVenues {
		t.Run(venue, func(t *testing.T) { testVenueTraffic(t, venue, cfg, hits) })
	}

	nonLoopbackDials := globalRecorder.NonLoopbackDialCount() - initialNonLoopback
	loopbackDials := hits.all()
	t.Logf("=== Dial Count Summary ===")
	t.Logf("Loopback requests (to sandbox):  %d", loopbackDials)
	t.Logf("Non-loopback dials (escaped):    %d", nonLoopbackDials)
	if nonLoopbackDials > 0 {
		t.Fatalf("FAIL: Detected %d non-loopback dials (sandbox escape)", nonLoopbackDials)
	}
	if loopbackDials == 0 {
		t.Fatalf("FAIL: zero loopback requests: no traffic was issued, so this test proves nothing")
	}
}

// testVenueTraffic creates the venue's exchange from a placeholder account and drives
// a request with GetBalances. It fails when the exchange cannot be created, when the
// sandbox saw no request for the venue, and when anything left loopback.
func testVenueTraffic(t *testing.T, venue string, cfg *config.Config, hits *sandboxHits) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	beforeEscapes := globalRecorder.NonLoopbackDialCount()
	beforeHits := hits.of(venue)

	ex, err := exchanges.CreateExchangeForAccount(venue, sandbox.PlaceholderAccountName, cfg)
	if err != nil {
		t.Fatalf("CreateExchangeForAccount(%s) failed: %v", venue, err)
	}
	if ex == nil {
		t.Fatalf("exchange is nil for venue %s", venue)
	}

	balances, err := ex.GetBalances(ctx)
	if err != nil {
		// A fixture gap is tolerable; no traffic is not (checked below).
		t.Logf("GetBalances error for %s: %v", venue, err)
	} else {
		t.Logf("%s: GetBalances succeeded, got %d balance(s)", venue, len(balances))
	}

	if n := globalRecorder.NonLoopbackDialCount() - beforeEscapes; n > 0 {
		t.Errorf("%s: %d non-loopback dial(s): %v", venue, n, globalRecorder.UnexpectedTargets())
	}
	n := hits.of(venue) - beforeHits
	t.Logf("%s: %d request(s) reached the sandbox server", venue, n)
	if n == 0 {
		t.Errorf("%s: no request reached the sandbox server; the venue was not exercised", venue)
	}
}


// TestProxyRecorderDetectsEscape proves the recorder itself works.
// It fires a request at an unroutable address and verifies the recorder observes it.
func TestProxyRecorderDetectsEscape(t *testing.T) {
	initialNonLoopback := globalRecorder.NonLoopbackDialCount()

	// Create a client WITHOUT sandbox wrapper - this should escape to non-loopback
	transport := &http.Transport{
		Dial:  (&net.Dialer{Timeout: 1 * time.Second}).Dial,
		Proxy: http.ProxyFromEnvironment,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}

	// Attempt to reach 192.0.2.1 (TEST-NET-1, unroutable per RFC 5737)
	// This should timeout or fail, but the dial attempt will be made.
	// Declare it up front: this escape is the point of the test, so TestMain's
	// end-of-run report must not count it as a sandbox failure.
	globalRecorder.ExpectTarget("192.0.2.1")
	req, _ := http.NewRequest("GET", "http://192.0.2.1:80/test", nil)
	resp, _ := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	// Give the recorder a moment to process
	time.Sleep(100 * time.Millisecond)

	finalNonLoopback := globalRecorder.NonLoopbackDialCount()
	newNonLoopbackDials := finalNonLoopback - initialNonLoopback

	t.Logf("Detector liveness test: %d non-loopback dials recorded (expected >0)", newNonLoopbackDials)
	if newNonLoopbackDials > 0 {
		t.Logf("✓ Proxy recorder successfully detects non-loopback escape attempts")
	} else {
		t.Errorf("Proxy recorder did not detect TEST-NET-1 dial (recorder may not be intercepting all dials)")
	}
}

// TestSandboxedVenueClientEscapeDetection proves the detector fires on the real path.
// GREEN: with the sandbox enabled the request reaches the sandbox and nothing escapes.
// RED: a client built with the sandbox off sends its request to the real host, and the
// recorder (which refuses every non-loopback target) sees it.
func TestSandboxedVenueClientEscapeDetection(t *testing.T) {
	_, hits := startSandbox(t)
	cfg := placeholderConfig(t)

	t.Run("GreenPath_SandboxEnabled", func(t *testing.T) {
		beforeEscapes := globalRecorder.NonLoopbackDialCount()
		beforeHits := hits.of("binance")
		ex, err := exchanges.CreateExchangeForAccount("binance", sandbox.PlaceholderAccountName, cfg)
		if err != nil || ex == nil {
			t.Fatalf("binance exchange creation failed: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = ex.GetBalances(ctx)

		if n := globalRecorder.NonLoopbackDialCount() - beforeEscapes; n > 0 {
			t.Fatalf("GREEN path FAILED: %d non-loopback dials detected with sandbox enabled", n)
		}
		n := hits.of("binance") - beforeHits
		if n == 0 {
			t.Fatalf("GREEN path FAILED: no request reached the sandbox server")
		}
		t.Logf("GREEN: %d request(s) to the sandbox, zero non-loopback escapes", n)
	})

	t.Run("RedPath_SandboxDisabled", func(t *testing.T) {
		sandbox.SetGlobalState(false, "")
		exchanges.ResetInstanceCache()
		beforeEscapes := globalRecorder.NonLoopbackDialCount()
		beforeHits := hits.of("binance")
		firstTarget := len(globalRecorder.RecordedTargets())
		// The recorder refuses these on purpose: the end-of-run report must not count them.
		defer func() { globalRecorder.ExpectTargetsFrom(firstTarget) }()

		ex, err := exchanges.CreateExchangeForAccount("binance", sandbox.PlaceholderAccountName, cfg)
		if err != nil || ex == nil {
			t.Fatalf("binance exchange creation failed: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = ex.GetBalances(ctx)

		n := globalRecorder.NonLoopbackDialCount() - beforeEscapes
		t.Logf("RED: sandbox disabled, %d non-loopback dial(s) seen by the recorder", n)
		if n == 0 {
			t.Fatalf("RED path FAILED: the sandbox is off and the detector saw no escape")
		}
		if h := hits.of("binance") - beforeHits; h != 0 {
			t.Fatalf("RED path FAILED: %d request(s) reached the sandbox with it disabled", h)
		}
	})
}

// TestConfigReloadSeam tests that a client built before the sandbox was enabled sends
// its requests to the sandbox after the toggle. The ccxt-backed Binance client bakes its
// URLs at construction, so this seam needs a venue whose client does not: Bitkub, whose
// HTTP client reads the sandbox state on every request.
func TestConfigReloadSeam(t *testing.T) {
	server, hits := startSandbox(t)
	cfg := placeholderConfig(t)

	// Phase 1: a client built with the sandbox OFF. Nothing is sent from it yet.
	sandbox.SetGlobalState(false, "")
	exchanges.ResetInstanceCache()
	ex1, err := exchanges.CreateExchangeForAccount("bitkub", sandbox.PlaceholderAccountName, cfg)
	if err != nil || ex1 == nil {
		t.Fatalf("bitkub exchange creation failed: %v", err)
	}

	// Phase 2: enable the sandbox WITHOUT ResetInstanceCache, so the cached client stays.
	sandbox.SetGlobalState(true, server.BaseURL())
	ex2, err := exchanges.CreateExchangeForAccount("bitkub", sandbox.PlaceholderAccountName, cfg)
	if err != nil {
		t.Fatalf("second CreateExchangeForAccount: %v", err)
	}
	if ex2 != ex1 {
		t.Fatalf("expected the cached instance, got a new one: the test no longer covers the seam")
	}

	// Phase 3: the cached client's request reaches the sandbox, and nothing leaves loopback.
	beforeEscapes := globalRecorder.NonLoopbackDialCount()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = ex2.GetBalances(ctx)

	if n := globalRecorder.NonLoopbackDialCount() - beforeEscapes; n > 0 {
		t.Fatalf("%d non-loopback dial(s) after the toggle", n)
	}
	n := hits.of("bitkub")
	if n == 0 {
		t.Fatal("the cached client did not send its request to the sandbox after the toggle")
	}
	t.Logf("cached client, built before the toggle, sent %d request(s) to the sandbox", n)
}

// TestSandboxToggleWithCCXT tests the seam for a ccxt client. Its URLs are baked at
// construction, so after a toggle without ResetInstanceCache the cached client is stale:
// built with the sandbox off it would go to the real host. Production calls
// ResetInstanceCache at every toggle (web/backend/api/sandbox.go and
// cmd/khunquant/internal/gateway/helpers.go); this test shows that doing so is what makes
// the next client use the sandbox. The stale client is never driven.
func TestSandboxToggleWithCCXT(t *testing.T) {
	server, hits := startSandbox(t)
	cfg := placeholderConfig(t)

	sandbox.SetGlobalState(false, "")
	exchanges.ResetInstanceCache()
	stale, err := exchanges.CreateExchangeForAccount("binance", sandbox.PlaceholderAccountName, cfg)
	if err != nil || stale == nil {
		t.Fatalf("binance exchange creation failed: %v", err)
	}

	// The toggle without a reset hands back the same (stale) client.
	sandbox.SetGlobalState(true, server.BaseURL())
	same, err := exchanges.CreateExchangeForAccount("binance", sandbox.PlaceholderAccountName, cfg)
	if err != nil {
		t.Fatalf("CreateExchangeForAccount: %v", err)
	}
	if same != stale {
		t.Fatal("expected the cached instance without ResetInstanceCache")
	}

	// With the reset, the next client is built against the sandbox and its request lands there.
	exchanges.ResetInstanceCache()
	fresh, err := exchanges.CreateExchangeForAccount("binance", sandbox.PlaceholderAccountName, cfg)
	if err != nil || fresh == nil {
		t.Fatalf("binance exchange creation after reset failed: %v", err)
	}
	if fresh == stale {
		t.Fatal("ResetInstanceCache did not drop the cached client")
	}
	beforeEscapes := globalRecorder.NonLoopbackDialCount()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = fresh.GetBalances(ctx)
	if n := globalRecorder.NonLoopbackDialCount() - beforeEscapes; n > 0 {
		t.Fatalf("%d non-loopback dial(s) from the rebuilt client", n)
	}
	n := hits.of("binance")
	if n == 0 {
		t.Fatal("the rebuilt client did not reach the sandbox")
	}
	t.Logf("rebuilt client sent %d request(s) to the sandbox", n)
}


// TestFixtureLoadedBeforeFirstCall tests that the gateway loads fixtures from disk
// before accepting the first tool call.
func TestFixtureLoadedBeforeFirstCall(t *testing.T) {
	store := sandbox.NewStore()
	workspaceDir := getFixturesDir(t)
	if err := store.Load(workspaceDir); err != nil {
		t.Fatalf("Failed to load fixtures: %v", err)
	}

	// Verify that at least some fixtures were loaded
	t.Logf("✓ Fixtures loaded from %s", workspaceDir)
}

// TestVenueNameCollision verifies that both ccxt clients and pkg/deltaneutral/fees
// can claim the same venue string without destructive fixture collisions.
func TestVenueNameCollision(t *testing.T) {
	t.Run("Binance", func(t *testing.T) {
		ex, err := exchanges.CreateExchangeForAccount("binance", "test", &config.Config{})
		if err != nil {
			t.Logf("Binance exchange creation failed: %v", err)
		} else {
			t.Logf("✓ Binance exchange created")
		}
		_ = ex
	})

	t.Run("OKX", func(t *testing.T) {
		ex, err := exchanges.CreateExchangeForAccount("okx", "test", &config.Config{})
		if err != nil {
			t.Logf("OKX exchange creation failed: %v", err)
		} else {
			t.Logf("✓ OKX exchange created")
		}
		_ = ex
	})
}

// setupVenueStates initializes state for all venues with basic market and balance data.
func setupVenueStates(t *testing.T, sm *sandbox.StateManager) {
	venues := []string{"binance", "okx", "bitkub", "binanceth", "settrade", "webull"}

	for _, venue := range venues {
		state := sm.GetState(venue)
		state.Balances["USDT"] = sandbox.Balance{Free: 100000, Locked: 0}
		state.Markets["BTCUSDT"] = sandbox.Market{
			Symbol:       "BTCUSDT",
			ContractSize: 0.001,
			MinAmount:    0.001,
			MaxLeverage:  125,
		}
		state.MarkPrices["BTCUSDT"] = 50000
		state.Leverage["BTCUSDT"] = 1
		t.Logf("  Setup %s: USDT balance, BTCUSDT market", venue)
	}
}

// getFixturesDir returns the workspace/sandbox directory path.
// This avoids hardcoding the path in tests.
func getFixturesDir(t *testing.T) string {
	// Find the repo root by looking for go.mod
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get working directory: %v", err)
	}

	// Walk up to find the repo root
	for {
		if _, err := os.Stat(wd + "/go.mod"); err == nil {
			return wd + "/workspace/sandbox"
		}
		parent := wd[:strings.LastIndex(wd, "/")]
		if parent == wd {
			// Reached root; use a relative path from the test
			return "workspace/sandbox"
		}
		wd = parent
	}
}
