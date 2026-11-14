package okx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// fixturesDir walks up to go.mod and returns workspace/sandbox.
func fixturesDir(t *testing.T) string {
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

var mgnRatioField = regexp.MustCompile(`"mgnRatio":"[^"]*"`)

// adapterOnTheSimulatorWire builds the real adapter (NewOKXExchange, so its client reads the
// simulator's OKX wire) with one cross short, and returns it. stripMgnRatio blanks mgnRatio in
// the positions answer, as an account that does not report it.
func adapterOnTheSimulatorWire(t *testing.T, stripMgnRatio bool) *OKXBrokerAdapter {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(fixturesDir(t)); err != nil {
		t.Fatal(err)
	}
	if len(store.Venues()) == 0 {
		t.Fatal("no fixture venues loaded: the fixtures path is wrong")
	}
	sm := sandbox.NewStateManager()
	sandbox.SeedDevDefaults(store, sm)
	st := sm.GetState("okx")
	mk := st.Markets["BTC-USDT-SWAP"]
	mk.ContractSize = 0.01
	st.Markets["BTC-USDT-SWAP"] = mk
	st.Balances["USDT"] = sandbox.Balance{Free: 1000}
	st.MarkPrices["BTC-USDT-SWAP"] = 72000
	st.Positions["BTC-USDT-SWAP"] = &sandbox.Position{Symbol: "BTC-USDT-SWAP", Side: "short", Contracts: 10, ContractSize: 0.01,
		EntryPrice: 70000, Leverage: 10, UpdatedAt: time.Now()}

	router := sandbox.BuildRouter(store, sandbox.NewStatefulSimulator(sm))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !stripMgnRatio || !strings.HasSuffix(r.URL.Path, "/account/positions") {
			router(w, r)
			return
		}
		rec := httptest.NewRecorder()
		router(rec, r)
		body, _ := io.ReadAll(rec.Result().Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(mgnRatioField.ReplaceAll(body, []byte(`"mgnRatio":""`)))
	}))
	t.Cleanup(srv.Close)

	sandbox.SetGlobalState(true, srv.URL)
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })
	var creds config.OKXExchangeAccount
	creds.APIKey = *config.NewSecureString("sbx-test-api-key-0001")
	creds.Secret = *config.NewSecureString("sbx-test-secret-0001")
	creds.Passphrase = *config.NewSecureString("sbx-test-pass-0001")
	ex, err := NewOKXExchange(creds, false)
	if err != nil {
		t.Fatal(err)
	}
	return &OKXBrokerAdapter{OKXExchange: ex}
}

// FetchFuturesPositions itself applies OKX's own margin ratio (not ccxt's mmr / (imr + upl)):
// the test reads through the adapter, so removing the call there fails it.
func TestFetchFuturesPositions_AppliesOKXMarginRatio(t *testing.T) {
	a := adapterOnTheSimulatorWire(t, false)
	ps, err := a.FetchFuturesPositions(context.Background(), []string{"BTC/USDT:USDT"})
	if err != nil || len(ps) != 1 {
		t.Fatalf("FetchFuturesPositions = %d positions, %v", len(ps), err)
	}
	p := ps[0]
	if p.MarginMode == nil || *p.MarginMode != "cross" {
		t.Fatalf("margin mode %v, want cross", p.MarginMode)
	}
	// Maintenance 28.8 over the 800 of equity left: 0.036. ccxt's own number is 28.8 / 520.
	if p.MarginRatio == nil {
		t.Fatal("margin ratio is nil, want 0.036")
	}
	if got := *p.MarginRatio; got < 0.0359 || got > 0.0361 {
		t.Fatalf("margin ratio = %v, want 1/mgnRatio = 0.036 (ccxt's own would be 0.0554)", got)
	}
}

// With no mgnRatio on the wire a cross position's ratio is unknown (nil), not ccxt's number.
func TestFetchFuturesPositions_NoMgnRatioIsUnknown(t *testing.T) {
	a := adapterOnTheSimulatorWire(t, true)
	ps, err := a.FetchFuturesPositions(context.Background(), []string{"BTC/USDT:USDT"})
	if err != nil || len(ps) != 1 {
		t.Fatalf("FetchFuturesPositions = %d positions, %v", len(ps), err)
	}
	if ps[0].MarginRatio != nil {
		t.Fatalf("margin ratio = %v, want nil (unknown)", *ps[0].MarginRatio)
	}
}
