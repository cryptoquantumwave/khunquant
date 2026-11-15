package scenario

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// repoFixtures is workspace/sandbox, found by walking up to go.mod.
func repoFixtures(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "workspace", "sandbox")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// An order the real OKX adapter places on the simulated wire is counted by `order: {venue:
// okx}` (TRUST-8). ccxt creates it through /api/v5/trade/batch-orders, which the request-log
// path list did not name, so the venue-only form was empty for OKX whatever happened.
func TestEval_VenueOnlyOrderCountsAnOKXOrderPlacedThroughCCXT(t *testing.T) {
	f := newEvalFixture(t)
	store := sandbox.NewStore()
	if err := store.Load(repoFixtures(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	st := sm.GetState("okx")
	st.Balances["USDT"] = sandbox.Balance{Free: 100000}
	st.MarkPrices["BTC-USDT"] = 50000
	sim := sandbox.NewStatefulSimulator(sm)
	sim.SetJournal(func(kind, subject string, payload any) { // as the gateway wires it
		b, _ := json.Marshal(payload)
		e := labjournal.Entry{Kind: kind, Source: "sandbox", Subject: subject, Payload: b}
		if p, ok := payload.(sandbox.OrderEntry); ok {
			e.SimTime = p.At
		}
		f.in.j.Append(e)
	})
	var paths []string
	router := sandbox.BuildRouter(store, sim)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		router(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		if r.Method == http.MethodPost { // the request log: exchange.req, as the chain's observer writes it
			paths = append(paths, r.URL.Path)
			b, _ := json.Marshal(map[string]any{"method": r.Method, "path": r.URL.Path, "status": rec.Code})
			f.in.j.Append(labjournal.Entry{Kind: "exchange.req", Source: "sandbox", Subject: "okx", SimTime: evalStart, Payload: b})
		}
	}))
	t.Cleanup(srv.Close)

	ex := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ex, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	f.check(`order: {venue: okx, count: 0}`, true) // nothing placed yet
	if _, err := ex.CreateOrder("BTC/USDT", "market", "buy", 0.5, ccxt.WithCreateOrderPrice(50000)); err != nil {
		t.Fatalf("CreateOrder = %v", err)
	}
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/api/v5/trade/batch-orders") {
		t.Fatalf("the adapter's POSTs = %v; the finding is that ccxt creates through batch-orders", paths)
	}
	f.check(`order: {venue: okx, count: 1}`, true)
	f.check(`order: {venue: okx}`, true)
	f.check(`order: {venue: okx, symbol: "BTC/USDT", count: 1}`, true) // the same order, asked another way
	// The control: a venue that placed nothing counts 0 and fails "at least one".
	f.check(`order: {venue: binance, count: 0}`, true)
	f.check(`order: {venue: binance}`, false)
	f.check(`order: {venue: okx, count: 0}`, false)
}

// Which journal an order expectation reads: the ledger for every simulated venue, the
// request log only where there is no ledger (Bitkub) or no venue to ask it for.
func TestOrderJournalKind_VenueOnly(t *testing.T) {
	for _, c := range []struct {
		fields map[string]any
		want   string
	}{
		{map[string]any{"venue": "okx"}, "exchange.order"},
		{map[string]any{"venue": "binance"}, "exchange.order"},
		{map[string]any{"venue": "settrade"}, "exchange.order"},
		{map[string]any{"venue": "webull"}, "exchange.order"},
		{map[string]any{"venue": "bitkub"}, "exchange.req"},
		{map[string]any{}, "exchange.req"},
		{map[string]any{"venue": "okx", "side": "buy"}, "exchange.order"},
		{map[string]any{"venue": "bitkub", "side": "buy"}, "exchange.order"},
	} {
		if got := OrderJournalKind(c.fields); got != c.want {
			t.Errorf("OrderJournalKind(%v) = %s, want %s", c.fields, got, c.want)
		}
	}
}
