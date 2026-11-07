package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// equityOrderBody is a Settrade order the lab would refuse for want of a price: either way
// the unlocked server must not look at it.
const equityOrderBody = `{"side":"Buy","symbol":"PTT","volume":100,"price":30,"priceType":"Limit","validityType":"Day"}`

// webullOrderBody is a Webull order the lab refuses for want of a price.
const webullOrderBody = `{"account_id":"A","new_orders":[{"client_order_id":"kq1","instrument_type":"EQUITY","order_type":"MARKET","side":"BUY","symbol":"AAPL","time_in_force":"DAY","quantity":"1"}]}`

func sbxDo(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Settrade and Webull are simulated only in a locked lab. The sandbox
// server an ordinary gateway runs has the stateful simulator as its responder, and it must
// leave their routes to the fixtures exactly as before: an order and an orders listing are
// fixture misses here (this workspace records none for the route), with nothing booked. The
// locked lab's simulator answers both itself.
func TestSettradeWebull_NotSimulatedWhenUnlocked(t *testing.T) {
	const orders = "/__sbx__/settrade/open-api.settrade.com/api/seos/v3/B/accounts/A/orders"
	f := newAssemblyFixture(t, enableAllRoutes)
	srv := f.services.SandboxServer
	if srv == nil || !srv.IsRunning() {
		t.Fatal("the unlocked fixture runs no sandbox server, so there is nothing to check")
	}
	sim, ok := srv.Responder().(*sandbox.StatefulSimulator)
	if !ok {
		t.Fatalf("the unlocked server's responder is %T, want the stateful simulator", srv.Responder())
	}
	if st, body := sbxDo(t, "POST", srv.BaseURL()+orders, equityOrderBody); st != http.StatusNotFound || !strings.Contains(body, "no fixture configured") {
		t.Errorf("unlocked settrade order: %d %s, want the router's fixture miss", st, body)
	}
	if st, body := sbxDo(t, "GET", srv.BaseURL()+orders, ""); st != http.StatusNotFound || !strings.Contains(body, "no fixture configured") {
		t.Errorf("unlocked settrade orders listing: %d %s, want the router's fixture miss, not the simulator's empty list", st, body)
	}
	const webull = "/__sbx__/webull/api.webull.co.th/openapi/trade/order/"
	if st, body := sbxDo(t, "POST", srv.BaseURL()+webull+"place", webullOrderBody); st != http.StatusNotFound || !strings.Contains(body, "no fixture configured") {
		t.Errorf("unlocked webull order: %d %s, want the router's fixture miss", st, body)
	}
	if st, body := sbxDo(t, "GET", srv.BaseURL()+webull+"open?account_id=A", ""); st != http.StatusNotFound || !strings.Contains(body, "no fixture configured") {
		t.Errorf("unlocked webull open orders: %d %s, want the router's fixture miss, not the simulator's empty list", st, body)
	}
	var booked int
	for _, venue := range []string{"settrade", "webull"} {
		sim.Read(venue, func(st *sandbox.VenueState) { booked += len(st.OpenOrders) + len(st.ClosedOrders) })
	}
	if booked != 0 {
		t.Errorf("the unlocked simulator booked %d equity orders", booked)
	}

	locked := newLockedFixture(t)
	base := locked.lab.Server.BaseURL()
	if st, body := sbxDo(t, "POST", base+orders, equityOrderBody); st != http.StatusBadRequest || !strings.Contains(body, `"code":"SBX-`) {
		t.Errorf("locked settrade order: %d %s, want the simulator's SBX- refusal (no price yet)", st, body)
	}
	if st, body := sbxDo(t, "GET", base+orders, ""); st != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("locked settrade orders listing: %d %s, want the simulator's empty list", st, body)
	}
	if st, body := sbxDo(t, "POST", base+webull+"place", webullOrderBody); st != http.StatusBadRequest || !strings.Contains(body, `"error_code":"SBX-`) {
		t.Errorf("locked webull order: %d %s, want the simulator's SBX- refusal (no price yet)", st, body)
	}
	if st, body := sbxDo(t, "GET", base+webull+"open?account_id=A", ""); st != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Errorf("locked webull open orders: %d %s, want the simulator's empty list", st, body)
	}
}
