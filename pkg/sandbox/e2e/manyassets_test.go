package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// pathRecorder is a Responder that never answers; it only records what the
// exchange clients asked for. Placed first in the responder chain, it gives an
// exact list of endpoints a tool touches, including the ones that 404 because
// no fixture exists.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
	seen  map[string]int
}

func newPathRecorder() *pathRecorder {
	return &pathRecorder{seen: map[string]int{}}
}

func (p *pathRecorder) Respond(venue, method, path string, _ *http.Request) (*sandbox.Response, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := fmt.Sprintf("%s %s %s", venue, method, path)
	if _, ok := p.seen[key]; !ok {
		p.paths = append(p.paths, key)
	}
	p.seen[key]++
	return nil, false // always fall through
}

// chainResponder queries responders in order, mirroring BuildRouter's variadic
// chain. Server.SetResponder only accepts one responder, so the chain has to be
// assembled here.
type chainResponder []sandbox.Responder

func (c chainResponder) Respond(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
	for _, resp := range c {
		if out, ok := resp.Respond(venue, method, path, r); ok {
			return out, true
		}
	}
	return nil, false
}

func (p *pathRecorder) Report() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.paths))
	for _, k := range p.paths {
		out = append(out, fmt.Sprintf("%s  (x%d)", k, p.seen[k]))
	}
	sort.Strings(out)
	return out
}

// manyAssetsEnv is a fully wired sandbox: fixtures on disk, a live server, the
// stateful simulator, and the global sandbox state armed so exchange clients
// route to it.
type manyAssetsEnv struct {
	t        *testing.T
	dir      string
	store    *sandbox.Store
	server   *sandbox.Server
	state    *sandbox.StateManager
	recorder *pathRecorder
	cfg      *config.Config
}

// writeFixtures persists a venue fixture file, mirroring what the MCP
// sandbox_write_fixtures tool does to disk.
func (e *manyAssetsEnv) writeFixtures(venue string, entries []sandbox.FixtureEntry) {
	e.t.Helper()
	venueDir := filepath.Join(e.dir, venue)
	if err := os.MkdirAll(venueDir, 0o755); err != nil {
		e.t.Fatalf("mkdir %s: %v", venueDir, err)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		e.t.Fatalf("marshal fixtures for %s: %v", venue, err)
	}
	if err := os.WriteFile(filepath.Join(venueDir, "fixtures.json"), data, 0o644); err != nil {
		e.t.Fatalf("write fixtures for %s: %v", venue, err)
	}
}

func fixture(method, path string, body any) sandbox.FixtureEntry {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return sandbox.FixtureEntry{
		Method:     method,
		PathPrefix: path,
		Response: sandbox.Response{
			Status:  200,
			Body:    raw,
			Headers: map[string]string{"Content-Type": "application/json"},
		},
	}
}

// start loads the fixtures written so far, boots the server and arms sandbox
// mode. Must be called after every writeFixtures call.
func (e *manyAssetsEnv) start() {
	e.t.Helper()

	e.store = sandbox.NewStore()
	if err := e.store.Load(e.dir); err != nil {
		e.t.Fatalf("load fixtures: %v", err)
	}

	e.server = sandbox.NewServer(e.store)
	e.recorder = newPathRecorder()
	e.state = sandbox.NewStateManager()

	// Same seeding the gateway does in startAndRegisterSandbox.
	for _, venue := range e.store.Venues() {
		vs := e.state.GetState(venue)
		sandbox.SeedMarketsFromFixtures(venue, e.store, vs)
		for symbol := range vs.Markets {
			vs.MarkPrices[symbol] = 50000
		}
		vs.Balances["USDT"] = sandbox.Balance{Free: 100000, Locked: 0}

		// Capture the seeded state as the reset baseline. Without this, calling the reset
		// tool will wipe all simulator state and leave it inert until process restart.
		e.state.SnapshotAsSeed(venue)
	}

	sim := sandbox.NewStatefulSimulator(e.state)
	// Recorder first (records, never answers), then the simulator, then fixtures.
	e.server.SetResponder(chainResponder{e.recorder, sim})

	if err := e.server.Start(context.Background()); err != nil {
		e.t.Fatalf("start sandbox server: %v", err)
	}

	sandbox.SetGlobalState(true, e.server.BaseURL())
	exchanges.ResetInstanceCache()

	e.t.Cleanup(func() {
		sandbox.SetGlobalState(false, "")
		exchanges.ResetInstanceCache()
		e.server.Stop()
	})
}

func newManyAssetsEnv(t *testing.T) *manyAssetsEnv {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.Debug.Sandbox.Enabled = true
	cfg.Exchanges.Binance.Enabled = true
	cfg.Exchanges.Binance.Accounts = []config.ExchangeAccount{
		{Name: "main", APIKey: *config.NewSecureString("sandbox-key"), Secret: *config.NewSecureString("sandbox-secret")},
	}
	cfg.Exchanges.OKX.Enabled = true
	cfg.Exchanges.OKX.Accounts = []config.OKXExchangeAccount{
		{
			ExchangeAccount: config.ExchangeAccount{
				Name:   "main",
				APIKey: *config.NewSecureString("sandbox-key"),
				Secret: *config.NewSecureString("sandbox-secret"),
			},
			Passphrase: *config.NewSecureString("sandbox-pass"),
		},
	}

	return &manyAssetsEnv{t: t, dir: t.TempDir(), cfg: cfg}
}

// runBalanceTool executes get_assets_list and returns the rendered result.
func (e *manyAssetsEnv) runBalanceTool(exchange, walletType string) *tools.ToolResult {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tool := tools.NewExchangeBalanceTool(e.cfg)
	return tool.Execute(ctx, map[string]any{"exchange": exchange, "wallet_type": walletType})
}

// TestDiscoverBalanceEndpoints runs get_assets_list against a sandbox seeded
// only with the repo's own fixtures and reports every endpoint the tool asked
// for. It never fails; it exists to enumerate what a "many assets" mock must
// cover.
func TestDiscoverBalanceEndpoints(t *testing.T) {
	env := newManyAssetsEnv(t)

	// Copy the repo fixtures so we start from the shipped baseline.
	src := getFixturesDir(t)
	copyTree(t, src, env.dir)
	env.start()

	for _, tc := range []struct{ exchange, wallet string }{
		{"binance", "all"},
		{"binance", "spot"},
		{"okx", "all"},
	} {
		res := env.runBalanceTool(tc.exchange, tc.wallet)
		t.Logf("\n===== %s / %s =====\n%s", tc.exchange, tc.wallet, resultText(res))
	}

	t.Logf("\n===== endpoints requested =====")
	for _, p := range env.recorder.Report() {
		t.Logf("  %s", p)
	}
}

// TestManyAssetsBinanceAllWallets is the main whale-account run: every binance
// wallet holds the full roster, and get_assets_list must render all of them
// without erroring, dropping a wallet, or corrupting the table.
func TestManyAssetsBinanceAllWallets(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("binance", binanceManyAssetFixtures())
	env.start()

	res := env.runBalanceTool("binance", "all")
	if res == nil {
		t.Fatal("get_assets_list returned nil")
	}
	if res.IsError {
		t.Fatalf("get_assets_list failed: %s", res.ForLLM)
	}

	out := res.ForLLM
	t.Logf("response length: %d bytes, %d lines", len(out), strings.Count(out, "\n")+1)

	// Every wallet type must appear. A missing section means that wallet's
	// fetch failed and getAllBalances swallowed the error.
	for _, section := range []string{
		"## Spot", "## Funding", "## Cross Margin",
		"## Futures (Coin-M)", "## Simple Earn (Flexible)", "## Simple Earn (Locked)",
	} {
		if !strings.Contains(out, section) {
			t.Errorf("missing wallet section %q — the wallet fetch failed and was swallowed", section)
		}
	}

	// Every asset in the roster must survive into the output.
	for _, a := range manyAssets {
		if !strings.Contains(out, a.Asset) {
			t.Errorf("asset %s missing from output", a.Asset)
		}
	}

	t.Logf("\n%s", out)
}

// TestBalanceTableAlignmentWithExtraColumns checks the rendered table is
// actually a table. The earn wallets populate WalletBalance.Extra, which adds
// columns, and the huge/dust holdings stress the fixed-width numeric columns.
func TestBalanceTableAlignmentWithExtraColumns(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("binance", binanceManyAssetFixtures())
	env.start()

	res := env.runBalanceTool("binance", "earn_flexible")
	if res == nil || res.IsError {
		t.Fatalf("earn_flexible fetch failed: %v", res)
	}

	lines := strings.Split(strings.TrimRight(res.ForLLM, "\n"), "\n")
	var headerIdx = -1
	for i, l := range lines {
		if strings.HasPrefix(l, "Asset ") {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		t.Fatalf("no table header found in:\n%s", res.ForLLM)
	}

	header := lines[headerIdx]
	// The column where the first Extra column starts, per the header.
	extraStart := strings.Index(header, "apr")
	if extraStart < 0 {
		t.Fatalf("expected an 'apr' extra column in header %q", header)
	}

	// Data rows begin after the separator line.
	misaligned := 0
	for _, l := range lines[headerIdx+2:] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		// Find where this row's extra value actually starts: it is whatever
		// follows the Free/Locked columns.
		fields := strings.Fields(l)
		if len(fields) < 4 {
			continue
		}
		got := strings.Index(l, fields[3])
		if got != extraStart {
			misaligned++
			if misaligned <= 3 {
				t.Logf("row extra column at %d, header says %d:\n  H: %q\n  R: %q",
					got, extraStart, header, l)
			}
		}
	}
	if misaligned > 0 {
		t.Errorf("%d/%d data rows do not line up with the header's extra column",
			misaligned, len(lines)-headerIdx-2)
	}
}

// TestDustAndWhaleAmountRendering pins how formatAmount handles the extremes a
// many-asset account produces.
func TestDustAndWhaleAmountRendering(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("binance", binanceManyAssetFixtures())
	env.start()

	res := env.runBalanceTool("binance", "spot")
	if res == nil || res.IsError {
		t.Fatalf("spot fetch failed: %v", res)
	}

	for line := range strings.SplitSeq(res.ForLLM, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "DUST", "WEI":
			if fields[1] == "0" {
				t.Errorf("%s holds a non-zero balance but renders as %q — "+
					"formatAmount's %%.8f truncation makes real dust look like nothing",
					fields[0], fields[1])
			}
		case "SHIB", "PEPE", "BONK":
			t.Logf("whale row: %q", line)
		}
	}
}

// TestBinanceFuturesAccountIsSimulatorOwned proves that the /fapi/v3/account
// endpoint (Binance futures account) is owned by the stateful simulator,
// so any fixture written for it is shadowed. This is discoverable through
// SimulatorOwnedPath, and MCP write/upsert handlers warn when attempting
// to create/update a fixture for a shadowed route.
func TestBinanceFuturesAccountIsSimulatorOwned(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("binance", binanceManyAssetFixtures())
	env.start()

	// Assert 1: The simulator answers the /fapi/v3/account route and the response
	// reflects simulator state (USDT=100000 seeded in the harness), not the fixture.
	res := env.runBalanceTool("binance", "futures_usdt")
	if res == nil {
		t.Fatal("nil result")
	}
	t.Logf("futures_usdt result:\n%s", resultText(res))

	// The simulator seeds only USDT=100000 in the harness, so if the fixture
	// were honoured we would see the full asset roster (BTC, ETH, etc). Since we
	// don't, the simulator is answering, not the fixture.
	if strings.Contains(res.ForLLM, "BTC") {
		t.Errorf("fixture was honoured instead of simulator; " +
			"expected simulator-only USDT=100000, got fixture assets")
	} else {
		t.Log("simulator answered /fapi/v3/account (fixture shadowed)")
	}

	// Assert 2: SimulatorOwnedPath correctly identifies that the futures account
	// endpoint is simulator-owned.
	if !sandbox.SimulatorOwnedPath("binance", "GET", "/fapi/v3/account") {
		t.Error("SimulatorOwnedPath should return true for GET /fapi/v3/account")
	}
	t.Log("SimulatorOwnedPath confirmed: GET /fapi/v3/account is simulator-owned")

	// Assert 3: A fixture-served route (e.g., spot /api/v3/account) is NOT
	// simulator-owned, so it would be honoured if written.
	if sandbox.SimulatorOwnedPath("binance", "GET", "/api/v3/account") {
		t.Error("SimulatorOwnedPath should return false for GET /api/v3/account (spot endpoint)")
	}
	t.Log("SimulatorOwnedPath confirmed: GET /api/v3/account is fixture-served")
}

// TestOKXManyAssets drives OKX with a full roster in the funding wallet and a
// simulator-seeded trading wallet.
func TestOKXManyAssets(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("okx", okxManyAssetFixtures())
	env.start()

	// Seed the trading wallet directly: it is simulator-owned, so fixtures
	// cannot reach it.
	okxState := env.state.GetState("okx")
	for _, a := range manyAssets {
		okxState.Balances[a.Asset] = sandbox.Balance{Free: a.Free, Locked: a.Locked}
	}

	res := env.runBalanceTool("okx", "all")
	if res == nil {
		t.Fatal("nil result")
	}
	if res.IsError {
		t.Fatalf("okx get_assets_list failed: %s", res.ForLLM)
	}

	out := res.ForLLM
	t.Logf("response length: %d bytes, %d lines", len(out), strings.Count(out, "\n")+1)

	missing := 0
	for _, a := range manyAssets {
		if !strings.Contains(out, a.Asset) {
			missing++
			t.Errorf("asset %s missing from okx output", a.Asset)
		}
	}
	if missing == 0 {
		t.Logf("all %d assets present", len(manyAssets))
	}
	t.Logf("\n%s", out)
}

// TestEarnPaginationTerminates covers the Simple Earn pager. It compares the
// running count of *kept* balances against the server's total row count, so a
// page whose rows are all filtered out never advances the loop. Because a
// fixture is matched by path prefix, every page request returns the same body,
// and the pager cannot make progress.
func TestEarnPaginationTerminates(t *testing.T) {
	env := newManyAssetsEnv(t)

	entries := binanceManyAssetFixtures()
	// A full page (100 rows) of positions that were fully redeemed, so every
	// row has totalAmount 0 and is filtered out by the adapter.
	rows := make([]map[string]any, 0, 100)
	for i := range 100 {
		rows = append(rows, map[string]any{
			"asset":                      fmt.Sprintf("TOK%d", i),
			"totalAmount":                "0",
			"latestAnnualPercentageRate": "0.01",
			"productId":                  fmt.Sprintf("TOK%d001", i),
		})
	}
	for i := range entries {
		if entries[i].PathPrefix == "/sapi/v1/simple-earn/flexible/position" {
			entries[i] = fixture("GET", "/sapi/v1/simple-earn/flexible/position",
				map[string]any{"rows": rows, "total": 100})
		}
	}
	env.writeFixtures("binance", entries)
	env.start()

	done := make(chan *tools.ToolResult, 1)
	go func() { done <- env.runBalanceTool("binance", "earn_flexible") }()

	select {
	case res := <-done:
		t.Logf("returned: %s", resultText(res))
	case <-time.After(20 * time.Second):
		t.Fatal("get_assets_list(earn_flexible) did not return within 20s: " +
			"the pager compares filtered output length against the server's total " +
			"row count, so a page of zero-amount rows never advances it")
	}
}

// TestEarnPaginationDoesNotDuplicate covers the milder, live-API-reachable form
// of the same pager flaw: when a page contains rows the adapter filters out,
// the loop keeps requesting further pages and concatenates them, so assets come
// back more than once.
func TestEarnPaginationDoesNotDuplicate(t *testing.T) {
	env := newManyAssetsEnv(t)

	// 100 rows, half of them zero-amount so they are filtered out, and a total
	// that says there are more pages to come.
	rows := make([]map[string]any, 0, 100)
	for i := range 100 {
		amount := "125.5"
		if i%2 == 0 {
			amount = "0"
		}
		rows = append(rows, map[string]any{
			"asset":                      fmt.Sprintf("TOK%d", i),
			"totalAmount":                amount,
			"latestAnnualPercentageRate": "0.01",
		})
	}
	entries := binanceManyAssetFixtures()
	for i := range entries {
		if entries[i].PathPrefix == "/sapi/v1/simple-earn/flexible/position" {
			entries[i] = fixture("GET", "/sapi/v1/simple-earn/flexible/position",
				map[string]any{"rows": rows, "total": 250})
		}
	}
	env.writeFixtures("binance", entries)
	env.start()

	res := env.runBalanceTool("binance", "earn_flexible")
	if res == nil || res.IsError {
		t.Fatalf("earn_flexible failed: %v", res)
	}

	counts := map[string]int{}
	for line := range strings.SplitSeq(res.ForLLM, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "TOK") {
			counts[fields[0]]++
		}
	}
	dupes := 0
	for asset, n := range counts {
		if n > 1 {
			dupes++
			if dupes <= 3 {
				t.Logf("%s appears %d times", asset, n)
			}
		}
	}
	if dupes > 0 {
		t.Errorf("%d assets appear more than once: the pager re-requested pages "+
			"and concatenated them, inflating the reported portfolio", dupes)
	}
}

// TestResetSimulatorKeepsVenueUsable covers sandbox_reset_simulator. The gateway
// never calls StateManager.LoadState, so no seed is ever registered and Reset
// falls into its no-seed branch, which builds an empty state instead of
// restoring the markets, mark prices and balances that startAndRegisterSandbox
// seeded from fixtures.
func TestResetSimulatorKeepsVenueUsable(t *testing.T) {
	env := newManyAssetsEnv(t)
	env.writeFixtures("binance", binanceManyAssetFixtures())
	env.writeFixtures("okx", okxManyAssetFixtures())
	env.start()

	before := env.state.GetState("okx")
	if len(before.Balances) == 0 {
		t.Fatal("expected the startup seed to have put a balance in place")
	}
	t.Logf("before reset: %d balances, %d markets, %d mark prices",
		len(before.Balances), len(before.Markets), len(before.MarkPrices))

	// What the MCP sandbox_reset_simulator tool ends up calling.
	for _, venue := range env.store.Venues() {
		if err := env.state.Reset(venue); err != nil {
			t.Fatalf("reset %s: %v", venue, err)
		}
	}

	after := env.state.GetState("okx")
	t.Logf("after reset:  %d balances, %d markets, %d mark prices",
		len(after.Balances), len(after.Markets), len(after.MarkPrices))

	if len(after.Balances) == 0 {
		t.Errorf("reset wiped every balance and restored none: the simulator is " +
			"inert until the gateway restarts")
	}
	if after.MarkPrices == nil {
		t.Errorf("reset left MarkPrices as a nil map; every other code path that " +
			"builds a VenueState initialises it, so a write here panics")
	}
}

// TestPartialWalletFailureIsReported checks what the user sees when some
// wallets are reachable and others are not — the normal state of a real account
// that has not enabled every product.
func TestPartialWalletFailureIsReported(t *testing.T) {
	env := newManyAssetsEnv(t)

	// Keep spot; drop margin, coin-M, funding and both earn endpoints.
	var kept []sandbox.FixtureEntry
	for _, e := range binanceManyAssetFixtures() {
		switch e.PathPrefix {
		case "/sapi/v1/margin/account", "/dapi/v1/account",
			"/sapi/v1/asset/get-funding-asset",
			"/sapi/v1/simple-earn/flexible/position",
			"/sapi/v1/simple-earn/locked/position":
			continue
		}
		kept = append(kept, e)
	}
	env.writeFixtures("binance", kept)
	env.start()

	res := env.runBalanceTool("binance", "all")
	if res == nil {
		t.Fatal("nil result")
	}
	t.Logf("result with 5 of 7 wallets unreachable:\n%s", resultText(res))

	if res.IsError {
		t.Log("tool surfaced an error")
		return
	}
	lower := strings.ToLower(res.ForLLM)
	for _, marker := range []string{"error", "failed", "unavailable", "could not"} {
		if strings.Contains(lower, marker) {
			t.Logf("output mentions the failures via %q", marker)
			return
		}
	}
	t.Errorf("5 of 7 wallet fetches failed but the output says nothing about it; " +
		"a user with assets in those wallets sees a silently incomplete portfolio")
}

// TestResponseSizeAtScale measures what get_assets_list actually hands to the
// LLM as the roster grows. pkg/agent/loop.go puts ToolResult.ContentForLLM()
// into the tool message verbatim — the utils.Truncate(…, 5000) nearby is only
// applied to the log line — so whatever this prints is what enters the model's
// context window.
func TestResponseSizeAtScale(t *testing.T) {
	for _, n := range []int{50, 250, 500} {
		t.Run(fmt.Sprintf("%dassets", n), func(t *testing.T) {
			saved := manyAssets
			manyAssets = syntheticAssets(n)
			t.Cleanup(func() { manyAssets = saved })

			env := newManyAssetsEnv(t)
			env.writeFixtures("binance", binanceManyAssetFixtures())
			env.start()

			res := env.runBalanceTool("binance", "all")
			if res == nil || res.IsError {
				t.Fatalf("get_assets_list failed: %v", res)
			}

			out := res.ForLLM
			t.Logf("%d assets → %d bytes, %d lines, ~%d tokens (4 bytes/token)",
				n, len(out), strings.Count(out, "\n")+1, len(out)/4)

			// A truncation cap would show up as the byte count flattening, or as
			// a trailing ellipsis / cut-off final row.
			trimmed := strings.TrimRight(out, "\n")
			if strings.HasSuffix(trimmed, "...") || strings.HasSuffix(trimmed, "…") {
				t.Errorf("output ends in an ellipsis at %d assets: something truncated "+
					"the table mid-render", n)
			}
			if !utf8.ValidString(out) {
				t.Errorf("output is not valid UTF-8 at %d assets: a cut landed "+
					"mid-rune", n)
			}
		})
	}
}

// syntheticAssets generates a roster of n holdings, preserving the interesting
// magnitudes from the hand-written list at the front.
func syntheticAssets(n int) []assetHolding {
	out := make([]assetHolding, 0, n)
	for i, a := range manyAssetsBase {
		if i >= n {
			break
		}
		out = append(out, a)
	}
	for i := len(out); i < n; i++ {
		out = append(out, assetHolding{
			Asset:  fmt.Sprintf("TOK%03d", i),
			Free:   float64(i)*137.25 + 0.5,
			Locked: float64(i % 7),
		})
	}
	return out
}

func resultText(r *tools.ToolResult) string {
	if r == nil {
		return "<nil ToolResult>"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[error=%v] for_llm=%d bytes for_user=%d bytes\n",
		r.IsError, len(r.ForLLM), len(r.ForUser))
	sb.WriteString(r.ForLLM)
	return sb.String()
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixtures: %v", err)
	}
}
