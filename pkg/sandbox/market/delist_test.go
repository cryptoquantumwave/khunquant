package market

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

var (
	eth = Pair{"ETH", "USDT"}
	sol = Pair{"SOL", "USDT"}
)

// addSOL lists SOLUSDT in the recorded Binance exchangeInfo (the repo's fixtures carry
// only BTC and ETH), so a test has a third symbol to delist.
func (r *feedRig) addSOL(t *testing.T) {
	t.Helper()
	fx := r.store.GetFixtures("binance")
	n := 0
	for i := range fx {
		if !strings.HasSuffix(fx[i].PathPrefix, "/exchangeInfo") || fx[i].PathPrefix == "/dapi/v1/exchangeInfo" {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(fx[i].Response.Body, &doc); err != nil {
			t.Fatal(err)
		}
		rows := doc["symbols"].([]any)
		row := map[string]any{}
		for k, v := range rows[0].(map[string]any) {
			row[k] = v
		}
		row["symbol"], row["baseAsset"] = "SOLUSDT", "SOL"
		doc["symbols"] = append(rows, row)
		fx[i].Response.Body, _ = json.Marshal(doc)
		n++
	}
	if n != 2 {
		t.Fatalf("patched %d exchangeInfo fixtures, want 2 (spot and futures)", n)
	}
	r.store.SetFixtures("binance", fx)
}

func (r *feedRig) post(t *testing.T, venue, host, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(r.srv.URL+"/__sbx__/"+venue+"/"+host+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func delistRig(t *testing.T) *feedRig {
	t.Helper()
	r := newFeedRig(t)
	r.addSOL(t)
	for p, px := range map[Pair]float64{btc: 100000, eth: 3000, sol: 150} {
		if err := r.book.Set("", p, px); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestDelist_WireAndExchangeInfo(t *testing.T) {
	r := delistRig(t)
	const bin, fapi = "api.binance.com", "fapi.binance.com"
	symbolsOf := func(venue, host, path, key, field string) string {
		code, body := r.get(t, venue, host, path)
		var doc map[string]json.RawMessage
		var rows []map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil || code != 200 || json.Unmarshal(doc[key], &rows) != nil || len(rows) == 0 {
			t.Fatalf("%s %s = %d %s (%v)", venue, path, code, body, err)
		}
		var ids []string
		for _, row := range rows {
			ids = append(ids, row[field].(string))
		}
		sort.Strings(ids) // the simulator lists its markets in map order
		return strings.Join(ids, ",")
	}
	if got := symbolsOf("binance", bin, "/api/v3/exchangeInfo", "symbols", "symbol"); got != "BTCUSDT,ETHUSDT,SOLUSDT" {
		t.Fatalf("spot exchangeInfo before = %s", got)
	}

	if err := r.book.Delist("binance", "SOLUSDT"); err != nil { // a wire symbol is accepted
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v3/ticker/24hr?symbol=SOLUSDT", "/api/v3/ticker/24hr?symbols=%5B%22BTCUSDT%22%2C%22SOLUSDT%22%5D", "/api/v3/klines?symbol=SOLUSDT&interval=1m"} {
		if code, body := r.get(t, "binance", bin, path); code != 400 || !strings.Contains(body, `"code":-1121`) || !strings.Contains(body, "Invalid symbol.") {
			t.Errorf("%s = %d %s, want the Binance invalid-symbol error", path, code, body)
		}
	}
	if code, body := r.get(t, "binance", fapi, "/fapi/v1/ticker/24hr?symbol=SOLUSDT"); code != 400 {
		t.Errorf("futures ticker of a delisted pair = %d %s", code, body)
	}
	if code, body := r.get(t, "binance", bin, "/api/v3/ticker/24hr?symbol=BTCUSDT"); code != 200 || !strings.Contains(body, `"BTCUSDT"`) {
		t.Errorf("a listed symbol = %d %s", code, body)
	}
	if _, body := r.get(t, "binance", bin, "/api/v3/ticker/24hr"); strings.Contains(body, "SOLUSDT") || !strings.Contains(body, "ETHUSDT") {
		t.Errorf("all tickers = %s, want ETH but not the delisted SOL", body)
	}
	if got := symbolsOf("binance", bin, "/api/v3/exchangeInfo", "symbols", "symbol"); got != "BTCUSDT,ETHUSDT" {
		t.Errorf("spot exchangeInfo = %s, want SOL gone and the others kept", got)
	}
	if got := symbolsOf("binance", fapi, "/fapi/v1/exchangeInfo", "symbols", "symbol"); got != "BTCUSDT,ETHUSDT" {
		t.Errorf("futures exchangeInfo = %s", got)
	}

	// An order for it is refused before the orders stage; one for a listed symbol is not.
	order := "/fapi/v1/order?side=BUY&type=market&quantity=1&positionSide=LONG&symbol="
	if code, body := r.post(t, "binance", fapi, order+"SOLUSDT", ""); code != 400 || !strings.Contains(body, "-1121") {
		t.Errorf("order for a delisted symbol = %d %s", code, body)
	}
	r.sawStage(t, "binance", "/fapi/v1/order", "market")
	if code, body := r.post(t, "binance", fapi, order+"BTCUSDT", ""); code != 200 {
		t.Errorf("order for a listed symbol = %d %s", code, body)
	}
	r.sawStage(t, "binance", "/fapi/v1/order", "orders")

	// OKX: errors in the body, the listing filtered per instType.
	if err := r.book.Delist("okx", "BTC/USDT"); err != nil {
		t.Fatal(err)
	}
	const okx = "www.okx.com"
	if code, body := r.get(t, "okx", okx, "/api/v5/market/ticker?instId=BTC-USDT-SWAP"); code != 200 || !strings.Contains(body, `"code":"51001"`) {
		t.Errorf("okx ticker of a delisted pair = %d %s", code, body)
	}
	if code, body := r.post(t, "okx", okx, "/api/v5/trade/order", `{"instId":"BTC-USDT-SWAP","side":"buy","ordType":"market","sz":"1","tdMode":"cross"}`); code != 200 || !strings.Contains(body, `"code":"51001"`) {
		t.Errorf("okx order for a delisted pair = %d %s", code, body)
	}
	r.sawStage(t, "okx", "/api/v5/trade/order", "market")
	if got := symbolsOf("okx", okx, "/api/v5/public/instruments?instType=SPOT", "data", "instId"); got != "ETH-USDT" {
		t.Errorf("okx SPOT instruments = %s, want only the spot row that is still listed", got)
	}
	if got := symbolsOf("okx", okx, "/api/v5/public/instruments?instType=SWAP", "data", "instId"); strings.Contains(got, "BTC-USDT-SWAP") || !strings.Contains(got, "ETH-USDT-SWAP") {
		t.Errorf("okx SWAP instruments = %s", got)
	}

	// Relist restores all of it.
	if err := r.book.Relist("binance", "SOL/USDT"); err != nil {
		t.Fatal(err)
	}
	if err := r.book.Relist("okx", "BTC/USDT"); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.get(t, "binance", bin, "/api/v3/ticker/24hr?symbol=SOLUSDT"); code != 200 {
		t.Errorf("relisted ticker = %d", code)
	}
	if got := symbolsOf("binance", bin, "/api/v3/exchangeInfo", "symbols", "symbol"); got != "BTCUSDT,ETHUSDT,SOLUSDT" {
		t.Errorf("relisted exchangeInfo = %s", got)
	}
	if got := symbolsOf("okx", okx, "/api/v5/public/instruments?instType=SPOT", "data", "instId"); got != "BTC-USDT,ETH-USDT" {
		t.Errorf("relisted okx SPOT instruments = %s", got)
	}
	if code, body := r.post(t, "binance", fapi, order+"SOLUSDT", ""); strings.Contains(body, "-1121") {
		t.Errorf("order after relist = %d %s", code, body)
	}
	if err := r.book.Delist("kraken", "SOLUSDT"); err == nil {
		t.Error("an unknown venue was accepted")
	}
	if got := r.book.Delisted(""); len(got) != 0 {
		t.Errorf("Delisted after relisting everything = %v", got)
	}
}

// P2A-1: a client that already loaded markets keeps a delisted symbol; once the client
// cache is reset (reload_markets), a fresh one fails client-side with BadSymbol.
func TestDelist_ReloadMarketsGivesBadSymbol(t *testing.T) {
	r := delistRig(t)
	all := []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}
	stale := r.binance(t, nil)
	if res, err := stale.FetchTickers(ccxt.WithFetchTickersSymbols(all)); err != nil || len(res.Tickers) != 3 {
		t.Fatalf("before the delist: %d tickers (%v)", len(res.Tickers), err)
	}

	if err := r.book.Delist("binance", "SOL/USDT"); err != nil {
		t.Fatal(err)
	}
	exchanges.ResetInstanceCache() // what reload_markets does: the next client loads markets afresh
	fresh := r.binance(t, nil)
	if _, err := fresh.FetchTickers(ccxt.WithFetchTickersSymbols(all)); err == nil || !strings.Contains(err.Error(), "SOL/USDT") {
		t.Errorf("fresh client FetchTickers([BTC ETH SOL]) = %v, want a BadSymbol naming SOL/USDT", err)
	}
	res, err := fresh.FetchTickers(ccxt.WithFetchTickersSymbols(all[:2]))
	if err != nil || len(res.Tickers) != 2 {
		t.Errorf("fresh client FetchTickers([BTC ETH]) = %d tickers (%v), want 2", len(res.Tickers), err)
	}
}
