package fault

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// raw sends a request through the rig's sandbox address and returns the status and body.
func (r *orderRig) raw(t *testing.T, method, venue, host, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.BaseURL()+"/__sbx__/"+venue+"/"+host+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func jsonField(t *testing.T, body, path string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	for _, k := range strings.Split(path, ".") {
		if m, ok := v.(map[string]any); ok {
			v = m[k]
		}
		if l, ok := v.([]any); ok && len(l) > 0 { // data: [ {...} ] is read at its first row
			v = l[0]
		}
	}
	return v
}

// The simulator copies the library's error bodies (it cannot import this package), and
// TestLibrary_CCXTClassifies pins how ccxt reads the library's. This pins the copies to
// the originals, so the simulator's refusals are classified as the library's are.
func TestLibrary_SimulatorBodiesMatch(t *testing.T) {
	r := newOrderRig(t)
	same := func(t *testing.T, venue string, status int, body, name string) {
		t.Helper()
		want, ok := libraryResponse(venue, name)
		if !ok {
			t.Fatalf("no library row %s/%s", venue, name)
		}
		if status != want.Status {
			t.Errorf("%s/%s: simulator status %d, library %d", venue, name, status, want.Status)
		}
		if venue == "binance" && body != string(want.Body) {
			t.Errorf("%s/%s: simulator %s, library %s", venue, name, body, want.Body)
		}
		if venue == "okx" && name == "order_not_found" {
			if jsonField(t, body, "code") != jsonField(t, string(want.Body), "code") || jsonField(t, body, "msg") != jsonField(t, string(want.Body), "msg") {
				t.Errorf("%s/%s: simulator %s, library %s", venue, name, body, want.Body)
			}
		}
	}

	status, body := r.raw(t, "GET", "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&orderId=777", "")
	same(t, "binance", status, body, "order_not_found")
	_ = r.sim.Mutate("binance", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil })
	status, body = r.raw(t, "POST", "binance", "fapi.binance.com", "/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=10", "")
	same(t, "binance", status, body, "insufficient_funds")

	status, body = r.raw(t, "GET", "okx", "www.okx.com", "/api/v5/trade/order?instId=BTC-USDT-SWAP&ordId=777", "")
	same(t, "okx", status, body, "order_not_found")
	limit := `{"instId":"BTC-USDT-SWAP","tdMode":"cross","side":"buy","ordType":"limit","sz":"1","px":"90000","clOrdId":"x1"}`
	r.raw(t, "POST", "okx", "www.okx.com", "/api/v5/trade/order", limit)
	_, body = r.raw(t, "POST", "okx", "www.okx.com", "/api/v5/trade/order", limit)
	lib, _ := libraryResponse("okx", "invalid_order")
	if jsonField(t, body, "data.sCode") != jsonField(t, string(lib.Body), "data.sCode") {
		t.Errorf("okx duplicate: simulator %s, library invalid_order %s", body, lib.Body)
	}
	_ = r.sim.Mutate("okx", func(st *sandbox.VenueState) error { st.Balances["USDT"] = sandbox.Balance{Free: 1}; return nil })
	_, body = r.raw(t, "POST", "okx", "www.okx.com", "/api/v5/trade/order", `{"instId":"BTC-USDT-SWAP","tdMode":"cross","side":"buy","ordType":"market","sz":"10"}`)
	lib, _ = libraryResponse("okx", "insufficient_funds")
	if jsonField(t, body, "data.sCode") != jsonField(t, string(lib.Body), "data.sCode") || jsonField(t, body, "code") != jsonField(t, string(lib.Body), "code") {
		t.Errorf("okx insufficient_funds: simulator %s, library %s", body, lib.Body)
	}
}
