package sandbox_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

var fundingT0 = time.Date(2026, 3, 10, 8, 0, 0, 0, time.UTC)

func twoPayments(sym string) []sandbox.IncomeEntry {
	return []sandbox.IncomeEntry{
		{Symbol: sym, Type: "FUNDING_FEE", Income: -1.5, Rate: 0.0001, Mark: 70000, Time: fundingT0},
		{Symbol: sym, Type: "FUNDING_FEE", Income: 0.75, Rate: -0.0001, Mark: 70000, Time: fundingT0.Add(8 * time.Hour)},
	}
}

// With nothing paid the income route is the recorded fixture, as it was before funding
// existed; once a payment is in the ledger the ledger answers.
func TestIncomeFixtureFallthrough(t *testing.T) {
	var sm *sandbox.StateManager
	url := ccxtRig(t, func(m *sandbox.StateManager) { sm = m })
	income := url + "/__sbx__/binance/fapi.binance.com/fapi/v1/income"
	if got := getBody(t, income); !strings.Contains(got, "COMMISSION") {
		t.Fatalf("empty ledger: %s, want the recorded fixture", got)
	}
	sm.GetState("binance").Income = twoPayments("BTCUSDT")
	if got := getBody(t, income+"?incomeType=FUNDING_FEE"); !strings.Contains(got, `"incomeType":"FUNDING_FEE"`) || strings.Contains(got, "COMMISSION") {
		t.Fatalf("with a payment: %s, want the ledger", got)
	}
	if got := getBody(t, income+"?symbol=ETHUSDT"); got != "[]" {
		t.Errorf("another symbol: %s, want none", got)
	}
}

// ccxt's fetchFundingHistory reads the ledger from Binance income and from OKX bills (type
// 8); both are read through the real ccxt, and the OKX bills and bills-archive routes raw.
func TestFunding_HistoryCCXT(t *testing.T) {
	url := ccxtRig(t, func(sm *sandbox.StateManager) {
		sm.GetState("binance").Income = twoPayments("BTCUSDT")
		sm.GetState("okx").Income = twoPayments("BTC-USDT-SWAP")
	})
	bn := ccxt.NewBinanceusdm(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001"})
	if err := sandbox.RewriteExchangeURLs("binance", bn, url); err != nil {
		t.Fatal(err)
	}
	if _, err := bn.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	hist, err := bn.FetchFundingHistory(ccxt.WithFetchFundingHistorySymbol("BTC/USDT:USDT"))
	if err != nil || len(hist) != 2 {
		t.Fatalf("FetchFundingHistory = %d rows, %v, want the 2 payments", len(hist), err)
	}
	var sum float64
	for _, h := range hist {
		if h.Amount == nil || h.Timestamp == nil {
			t.Fatalf("row %+v lacks an amount or a time", h)
		}
		sum += *h.Amount
	}
	if sum != -0.75 {
		t.Errorf("amounts sum to %v, want -0.75", sum)
	}

	// The OKX swap is one market by id now that the SPOT instruments answer lists only spot rows
	// (it listed BTC-USDT-SWAP as well, and ccxt could not tell the two rows apart).
	ok := ccxt.NewOkx(map[string]any{"apiKey": "sbx-test-api-key-0001", "secret": "sbx-test-secret-0001", "password": "sbx-test-pass-0001"})
	if err := sandbox.RewriteExchangeURLs("okx", ok, url); err != nil {
		t.Fatal(err)
	}
	if _, err := ok.LoadMarkets(); err != nil {
		t.Fatal(err)
	}
	okHist, err := ok.FetchFundingHistory(ccxt.WithFetchFundingHistorySymbol("BTC/USDT:USDT"))
	if err != nil || len(okHist) != 2 {
		t.Fatalf("okx FetchFundingHistory = %d rows, %v, want the 2 payments", len(okHist), err)
	}
	sum = 0
	for _, h := range okHist {
		if h.Amount == nil || h.Timestamp == nil {
			t.Fatalf("okx row %+v lacks an amount or a time", h)
		}
		sum += *h.Amount
	}
	if sum != -0.75 {
		t.Errorf("okx amounts sum to %v, want -0.75", sum)
	}

	for _, route := range []string{"bills", "bills-archive"} {
		got := getBody(t, url+"/__sbx__/okx/www.okx.com/api/v5/account/"+route+"?type=8&instType=SWAP&ccy=USDT")
		// Newest first: the 0.75 received (subType 174) before the 1.5 paid (173).
		i, j := strings.Index(got, `"balChg":"0.75"`), strings.Index(got, `"balChg":"-1.5"`)
		if i < 0 || j < 0 || i > j || !strings.Contains(got, `"subType":"173"`) || !strings.Contains(got, `"type":"8"`) {
			t.Errorf("okx %s = %s", route, got)
		}
	}
	if got := getBody(t, url+"/__sbx__/okx/www.okx.com/api/v5/account/bills?type=2"); strings.Contains(got, "balChg") {
		t.Errorf("another bill type got funding rows: %s", got)
	}
}

// Each instType of the recorded OKX instruments answers its own rows and no other: the SPOT
// answer listed BTC-USDT-SWAP too, so one market id came back twice.
func TestOKXInstrumentsFixture_EachTypeListsOnlyItsRows(t *testing.T) {
	url := ccxtRig(t, func(*sandbox.StateManager) {})
	for _, typ := range []string{"SPOT", "SWAP", "FUTURES", "OPTION"} {
		got := getBody(t, url+"/__sbx__/okx/www.okx.com/api/v5/public/instruments?instType="+typ)
		lists := func(typ string) bool { return regexp.MustCompile(`"instType":\s*"` + typ + `"`).MatchString(got) }
		if !lists(typ) {
			t.Errorf("%s answer lists no %s row: %s", typ, typ, got)
		}
		for _, other := range []string{"SPOT", "SWAP", "FUTURES", "OPTION"} {
			if other != typ && lists(other) {
				t.Errorf("%s answer also lists %s rows", typ, other)
			}
		}
	}
}

// forceOrders lists the liquidation orders, and an empty list when there are none.
func TestForceOrders_ListsLiquidations(t *testing.T) {
	var sm *sandbox.StateManager
	url := ccxtRig(t, func(m *sandbox.StateManager) { sm = m })
	route := url + "/__sbx__/binance/fapi.binance.com/fapi/v1/forceOrders"
	if got := getBody(t, route); got != "[]" {
		t.Fatalf("no liquidations: %s", got)
	}
	sm.GetState("binance").Liquidations = []*sandbox.Order{{ID: "1001", Symbol: "BTCUSDT", OrderType: "market", Side: "sell", Amount: 0.1,
		Filled: 0.1, Average: 64000, Status: "FILLED", CreatedAt: fundingT0, UpdatedAt: fundingT0, Market: "futures", ReduceOnly: true}}
	if got := getBody(t, route+"?autoCloseType=LIQUIDATION"); !strings.Contains(got, `"autoCloseType":"LIQUIDATION"`) || !strings.Contains(got, `"avgPrice":"64000"`) {
		t.Errorf("forceOrders = %s", got)
	}
	if got := getBody(t, route+"?autoCloseType=ADL"); got != "[]" {
		t.Errorf("ADL orders = %s, want none: the simulator models no ADL", got)
	}
}

// A simulator whose price source is no book (an unlocked gateway's has none at all) pays no
// funding and liquidates nothing, however far the clock goes and however thin the wallet.
func TestDerivs_NoDerivSourceNoChange(t *testing.T) {
	sm := sandbox.NewStateManager()
	st := sm.GetState("binance")
	st.Balances["USDT"] = sandbox.Balance{Free: 1}
	st.MarkPrices["BTCUSDT"] = 90000
	st.Positions["BTCUSDT"] = &sandbox.Position{Symbol: "BTCUSDT", Side: "short", Contracts: 1, ContractSize: 1, EntryPrice: 70000, Leverage: 10}
	now := fundingT0
	sim := sandbox.NewStatefulSimulator(sm)
	sim.SetClock(func() time.Time { return now })
	for _, src := range []sandbox.PriceSource{nil, plainPrices{}} {
		sim.SetPriceSource(src)
		sim.Advance(now)
		now = now.Add(72 * time.Hour)
		sim.Advance(now)
		if len(st.Positions) != 1 || st.Balances["USDT"].Free != 1 || len(st.Income) != 0 || len(st.Liquidations) != 0 || !st.LastDerivs.IsZero() {
			t.Fatalf("source %T changed the state: positions %d wallet %v income %d liquidations %d lastDerivs %v",
				src, len(st.Positions), st.Balances["USDT"], len(st.Income), len(st.Liquidations), st.LastDerivs)
		}
	}
}

// plainPrices is a PriceSource that is not a DerivSource.
type plainPrices struct{}

func (plainPrices) MarkPrice(string, string) (float64, bool) { return 90000, true }
