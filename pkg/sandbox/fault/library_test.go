package fault

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// repoPath resolves a path under the repository root by walking up to go.mod.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found above the test directory")
	}
}

// libRig serves the recorded fixtures and the simulator, except that while armed it
// answers one path prefix of one venue with a library error, as a plain responder (no
// injector): the library is pinned on its own.
type libRig struct {
	srv   *httptest.Server
	armed atomic.Pointer[armedErr]
}

type armedErr struct{ venue, prefix, name string }

func newLibRig(t *testing.T) *libRig {
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
	rig := &libRig{}
	rig.srv = httptest.NewServer(sandbox.BuildRouter(store, sandbox.ResponderFunc(func(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
		if a := rig.armed.Load(); a != nil && a.venue == venue && strings.HasPrefix(path, a.prefix) {
			return libraryResponse(venue, a.name)
		}
		return sim.Respond(venue, method, path, r)
	})))
	t.Cleanup(rig.srv.Close)
	return rig
}

func (r *libRig) binance(t *testing.T, opts map[string]any) *ccxt.Binance {
	t.Helper()
	ex := ccxt.NewBinance(opts)
	if err := sandbox.RewriteExchangeURLs("binance", ex, r.srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatalf("loading markets: %v", err)
	}
	return ex
}

func (r *libRig) okx(t *testing.T) *ccxt.Okx {
	t.Helper()
	ex := ccxt.NewOkx(nil)
	if err := sandbox.RewriteExchangeURLs("okx", ex, r.srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatalf("loading markets: %v", err)
	}
	return ex
}

// classOf runs call with the library error armed and returns the ccxt class it produced.
func (r *libRig) classOf(t *testing.T, venue, prefix, name string, call func() error) string {
	t.Helper()
	r.armed.Store(&armedErr{venue, prefix, name})
	defer r.armed.Store(nil)
	err := call()
	if err == nil {
		t.Errorf("%s %s: the call succeeded", venue, name)
		return ""
	}
	typ, ok := broker.CCXTErrorType(err)
	if !ok {
		t.Errorf("%s %s: %v is not a ccxt error", venue, name, err)
	}
	return typ
}

// Every Binance and OKX row, served to a real ccxt client, is the class the table says.
// The URL type matters for Binance (spot and linear have their own tables), so each row
// is tried on both where it applies.
func TestLibrary_CCXTClassifies(t *testing.T) {
	r := newLibRig(t)
	spot := r.binance(t, nil)
	swap := r.binance(t, map[string]any{"defaultType": "future"})
	okx := r.okx(t)
	binSpot := func() error { _, err := spot.FetchTicker("BTC/USDT"); return err }
	binSwap := func() error { _, err := swap.FetchTicker("BTC/USDT:USDT"); return err }
	okxCall := func() error { _, err := okx.FetchTicker("BTC/USDT:USDT"); return err }

	for name, want := range map[string]string{
		// The plan expected RateLimitExceeded: ccxt's Binance handleErrors throws
		// DDoSProtection for every HTTP 429 and 418 before it reads the body.
		"rate_limit": "DDoSProtection", "backend_timeout": "RequestTimeout", "invalid_order": "InvalidOrder",
		"order_not_found": "OrderNotFound", "auth": "AuthenticationError", "bad_symbol": "BadSymbol",
		"insufficient_funds": "InsufficientFunds",
	} {
		if got := r.classOf(t, "binance", "/api/v3/ticker", name, binSpot); got != want {
			t.Errorf("binance spot %s = %s, want %s", name, got, want)
		}
		if got := r.classOf(t, "binance", "/fapi/v1/ticker", name, binSwap); got != want {
			t.Errorf("binance futures %s = %s, want %s", name, got, want)
		}
	}
	for name, want := range map[string]string{
		"rate_limit": "RateLimitExceeded", "backend_timeout": "RequestTimeout", "insufficient_funds": "InsufficientFunds",
		"invalid_order": "InvalidOrder", "order_not_found": "OrderNotFound", "bad_symbol": "BadSymbol",
	} {
		if got := r.classOf(t, "okx", "/api/v5/market/ticker", name, okxCall); got != want {
			t.Errorf("okx %s = %s, want %s", name, got, want)
		}
	}
	// A raw venue code is written in the venue's shape and classified by the same tables.
	// Binance -2019 (insufficient margin) is InsufficientFunds only in ccxt's linear table,
	// which ccxt selects from the real host of the URL; the lab rewrites it, so in the lab it
	// is a plain ExchangeError, an unknown outcome. SBX-14a must not use it for the margin
	// rejection (see the library comment).
	if got := r.classOf(t, "binance", "/fapi/v1/ticker", "-2019", binSwap); got != "ExchangeError" {
		t.Errorf("binance raw -2019 = %s, want ExchangeError in the lab", got)
	}
	if got := r.classOf(t, "okx", "/api/v5/market/ticker", "51000", okxCall); got != "BadRequest" {
		t.Errorf("okx raw 51000 = %s, want BadRequest", got)
	}
}

// The untyped venues are driven by status alone; every row says so, with a JSON body.
func TestLibrary_UntypedVenuesCarryStatus(t *testing.T) {
	for venue, want := range map[string]map[string]int{
		"bitkub":   {"rate_limit": 429, "server_error": 503, "bad_request": 400},
		"settrade": {"rate_limit": 429, "server_error": 503, "bad_request": 400},
		"webull":   {"rate_limit": 429, "server_error": 503, "bad_request": 400},
	} {
		for name, status := range want {
			resp, ok := libraryResponse(venue, name)
			if !ok || resp.Status != status || len(resp.Body) == 0 {
				t.Errorf("%s %s = %+v, %v; want status %d and a body", venue, name, resp, ok, status)
			}
		}
	}
	if _, ok := libraryResponse("webull", "nope"); ok {
		t.Error("an unknown name produced a response")
	}
	if _, ok := libraryResponse("webull", "-2019"); ok {
		t.Error("a raw code was written for a venue without a raw shape")
	}
	if !libraryHas("binanceth", "rate_limit", false) || libraryHas("*", "rate_limit", false) || libraryHas("llm", "rate_limit", false) {
		t.Error("binanceth uses Binance's errors; a wildcard or llm venue has none")
	}
}
