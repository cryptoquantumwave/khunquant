package market

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"
)

func TestRender_MarkAndFunding_CCXTParses(t *testing.T) {
	r, want := pathRig(t)
	swap := r.binance(t, map[string]any{"defaultType": "future"})
	sym := "BTC/USDT:USDT"
	// (ccxt-go's single-symbol FetchMarkPrice cannot read the object Binance answers with.)
	mk, err := swap.FetchMarkPrices(ccxt.WithFetchMarkPricesSymbols([]string{sym}))
	if err != nil || mk.Tickers[sym].MarkPrice == nil || *mk.Tickers[sym].MarkPrice != want {
		t.Fatalf("binance FetchMarkPrices = %+v (%v), want mark %v", mk.Tickers, err, want)
	}
	fr, err := swap.FetchFundingRate(sym)
	if err != nil || fr.FundingRate == nil || *fr.FundingRate != DefaultFunding || fr.MarkPrice == nil || *fr.MarkPrice != want || fr.IndexPrice == nil || *fr.IndexPrice != want {
		t.Fatalf("binance FetchFundingRate = %+v (%v), want rate %v and mark/index %v", fr, err, DefaultFunding, want)
	}
	next := time.UnixMilli(int64(*fr.FundingTimestamp)).UTC()
	if !next.After(r.clk.Now()) || next.Sub(r.clk.Now()) > 8*time.Hour || next.Hour()%8 != 0 || next.Minute() != 0 {
		t.Errorf("next funding at %v, want the next 00/08/16 UTC after %v", next, r.clk.Now())
	}
	if err := r.book.SetFunding("", btc, -0.0003); err != nil {
		t.Fatal(err)
	}
	if fr, err = swap.FetchFundingRate(sym); err != nil || *fr.FundingRate != -0.0003 {
		t.Errorf("binance funding after set = %+v (%v)", fr, err)
	}
	if err := r.book.SetFunding("binance", btc, 0.002); err != nil {
		t.Fatal(err)
	}
	if all, err := swap.FetchFundingRates(); err != nil || all.FundingRates[sym].FundingRate == nil || *all.FundingRates[sym].FundingRate != 0.002 {
		t.Errorf("binance FetchFundingRates = %+v (%v), want the venue-specific rate", all.FundingRates, err)
	}

	okx := r.okx(t, nil)
	omk, err := okx.FetchMarkPrice(sym)
	if err != nil || omk.MarkPrice == nil || *omk.MarkPrice != want {
		t.Fatalf("okx FetchMarkPrice = %+v (%v), want mark %v", omk, err, want)
	}
	ofr, err := okx.FetchFundingRate(sym)
	if err != nil || ofr.FundingRate == nil || *ofr.FundingRate != -0.0003 || ofr.FundingTimestamp == nil || *ofr.FundingTimestamp != *fr.FundingTimestamp {
		t.Fatalf("okx FetchFundingRate = %+v (%v), want the wildcard rate -0.0003 settling at %v", ofr, err, *fr.FundingTimestamp)
	}
	if all, err := okx.FetchFundingRates(); err != nil || len(all.FundingRates) != 1 || all.FundingRates[sym].FundingRate == nil {
		t.Errorf("okx FetchFundingRates = %+v (%v), want just the modelled swap", all.FundingRates, err)
	}
	for _, c := range []struct{ venue, path string }{
		{"binance", "/fapi/v1/premiumIndex"}, {"okx", "/api/v5/public/mark-price"}, {"okx", "/api/v5/public/funding-rate"},
	} {
		r.sawStage(t, c.venue, c.path, "market")
	}
}

func TestRender_MarkAndFunding_Wire(t *testing.T) {
	r, want := pathRig(t)
	code, body := r.get(t, "binance", "fapi.binance.com", "/fapi/v1/premiumIndex")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || code != 200 || len(rows) != 1 || rows[0]["markPrice"] != num(want) || rows[0]["symbol"] != "BTCUSDT" {
		t.Errorf("premiumIndex (all) = %d %s (%v)", code, body, err)
	}
	if code, body = r.get(t, "binance", "fapi.binance.com", "/fapi/v1/premiumIndex?symbol=BTCUSDT"); code != 200 || strings.HasPrefix(body, "[") {
		t.Errorf("premiumIndex (one) = %d %s, want an object", code, body)
	}
	if code, body = r.get(t, "okx", "www.okx.com", "/api/v5/public/mark-price?instType=SWAP"); code != 200 || !strings.Contains(body, `"markPx":"`+num(want)+`"`) {
		t.Errorf("mark-price = %d %s", code, body)
	}
	if code, body = r.get(t, "okx", "www.okx.com", "/api/v5/public/funding-rate?instId=ANY"); code != 200 || !strings.Contains(body, `"instId":"BTC-USDT-SWAP"`) {
		t.Errorf("funding-rate = %d %s", code, body)
	}

	// Never invented: an unmodelled symbol, a spot instrument asked for funding, a delisted one.
	for _, tc := range []struct{ venue, host, path string }{
		{"binance", "fapi.binance.com", "/fapi/v1/premiumIndex?symbol=XRPUSDT"},
		{"okx", "www.okx.com", "/api/v5/public/mark-price?instId=XRP-USDT-SWAP"},
		{"okx", "www.okx.com", "/api/v5/public/mark-price"},
		{"okx", "www.okx.com", "/api/v5/public/funding-rate?instId=XRP-USDT-SWAP"},
		{"okx", "www.okx.com", "/api/v5/public/funding-rate?instId=BTC-USDT"},
		{"bitkub", "api.bitkub.com", "/tradingview/history?symbol=XRP_THB&resolution=1&from=1&to=2"},
	} {
		if code, body := r.get(t, tc.venue, tc.host, tc.path); code != 404 || !strings.Contains(body, "no fixture configured") {
			t.Errorf("%s %s = %d %s, want the router's 404", tc.venue, tc.path, code, body)
		}
	}
	if err := r.book.Delist("", "BTC/USDT"); err != nil {
		t.Fatal(err)
	}
	if code, body = r.get(t, "binance", "fapi.binance.com", "/fapi/v1/premiumIndex?symbol=BTCUSDT"); code != 400 || !strings.Contains(body, "-1121") {
		t.Errorf("premiumIndex of a delisted pair = %d %s", code, body)
	}
	if code, body = r.get(t, "okx", "www.okx.com", "/api/v5/public/funding-rate?instId=BTC-USDT-SWAP"); !strings.Contains(body, "51001") {
		t.Errorf("funding-rate of a delisted pair = %d %s", code, body)
	}
	if code, body = r.get(t, "okx", "www.okx.com", "/api/v5/public/funding-rate?instId=ANY"); code != 404 {
		t.Errorf("funding-rate (all) with every swap delisted = %d %s, want no data invented", code, body)
	}
}

func TestRender_BitkubWire(t *testing.T) {
	r := newFeedRig(t)
	// No THB model: the recorded fixture answers, unchanged.
	if code, body := r.get(t, "bitkub", "api.bitkub.com", "/api/v3/market/ticker"); code != 200 || !strings.Contains(body, "1234567.89") {
		t.Fatalf("unmodelled ticker = %d %s, want the fixture", code, body)
	}
	r.sawStage(t, "bitkub", "/api/v3/market/ticker", "fixtures")

	thb := Pair{"BTC", "THB"}
	if err := r.book.Set("", thb, 3_000_000); err != nil {
		t.Fatal(err)
	}
	if err := r.book.Set("", btc, 98000); err != nil { // not a THB pair: never on Bitkub
		t.Fatal(err)
	}
	code, body := r.get(t, "bitkub", "api.bitkub.com", "/api/v3/market/ticker")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil || code != 200 || len(rows) != 1 || rows[0]["symbol"] != "BTC_THB" || rows[0]["last"] != float64(3_000_000) {
		t.Fatalf("modelled ticker = %d %s (%v)", code, body, err)
	}
	r.sawStage(t, "bitkub", "/api/v3/market/ticker", "market")
	if _, body = r.get(t, "bitkub", "api.bitkub.com", "/api/v3/market/ticker?sym=btc_thb"); !strings.Contains(body, `"BTC_THB"`) {
		t.Errorf("sym filter = %s", body)
	}

	// History: parallel arrays, oldest first, the forming bar last.
	now := r.clk.Now()
	path := "/tradingview/history?symbol=BTC_THB&resolution=1&from=" + itoa(now.Add(-3*time.Minute).Unix()) + "&to=" + itoa(now.Unix())
	var h struct {
		S string    `json:"s"`
		T []int64   `json:"t"`
		C []float64 `json:"c"`
	}
	code, body = r.get(t, "bitkub", "api.bitkub.com", path)
	if err := json.Unmarshal([]byte(body), &h); err != nil || code != 200 || h.S != "ok" || len(h.T) != 4 || h.C[3] != 3_000_000 || h.T[3] != alignDown(now, time.Minute).Unix() {
		t.Errorf("history = %d %s (%v)", code, body, err)
	}

	// Delisted: omitted from the array, no history.
	_ = r.book.Delist("bitkub", "BTC_THB")
	if code, body = r.get(t, "bitkub", "api.bitkub.com", "/api/v3/market/ticker?sym=BTC_THB"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("delisted sym ticker = %d %s", code, body)
	}
	if _, body = r.get(t, "bitkub", "api.bitkub.com", path); !strings.Contains(body, "no_data") {
		t.Errorf("delisted history = %s", body)
	}
	if _, body = r.get(t, "bitkub", "api.bitkub.com", "/api/v3/market/symbols"); strings.Contains(body, "BTC_THB") || !strings.Contains(body, `"result"`) {
		t.Errorf("symbols listing = %s, want the delisted pair gone", body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
