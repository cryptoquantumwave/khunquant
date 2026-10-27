package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func sandboxCall(t *testing.T, f *lockedFixture, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.lab.Server.BaseURL()+"/__sbx__/"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

// The locked gateway has the market group (route and tool); its prices reach the
// simulator's fills; the chain journals every exchange request with the stage that
// answered; and a reload keeps the book.
func TestLockedGatewayMarketGroupPricesFillsAndJournalsRequests(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) { c.Debug.DevMCP.Enabled = true })
	url := f.url("/api/sandbox/lab/market")
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_market"] {
		t.Fatal("tools/list of the first generation has no sandbox_market")
	}
	if _, ok := f.lab.Server.GetResponder().(*sandbox.Chain); !ok {
		t.Fatalf("a locked server's responder is %T, want the chain", f.lab.Server.GetResponder())
	}

	for _, body := range []string{
		`{"op":"set","symbol":"BTC/USDT","price":98000}`,
		`{"op":"path","symbol":"BTC/USDT","to":100500,"over":"3m"}`,
		`{"op":"shock","symbol":"BTC/USDT","pct":0}`,
	} {
		if code, out, err := postJSON(url, lockTestToken, body); err != nil || code != http.StatusOK {
			t.Fatalf("%s = %d (%v) %s", body, code, err, out)
		}
	}
	for _, bad := range []string{
		`{"op":"set","symbol":"BTC/USDT","price":-1}`,
		`{"op":"set","symbol":"BTC/USDT","prise":5}`,
		`{"op":"set","symbol":"BTCUSDT","price":5}`,
		`{"op":"path","symbol":"ETH/USDT","to":5,"over":"1m"}`, // no starting price
		`{"op":"walk","symbol":"BTC/USDT","vol":0.01}`,         // no seed
	} {
		if code, out, _ := postJSON(url, lockTestToken, bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", bad, code, out)
		}
	}

	// A market order fills at the book's price, a point on the path (not the 50000 seed).
	code, body := sandboxCall(t, f, "POST", "binance/fapi.binance.com/fapi/v1/order?symbol=BTCUSDT&side=BUY&type=market&quantity=0.5&positionSide=LONG")
	var o struct {
		AvgPrice string `json:"avgPrice"`
	}
	if err := json.Unmarshal([]byte(body), &o); err != nil || code != http.StatusOK || o.AvgPrice == "" || o.AvgPrice == "50000" {
		t.Fatalf("order = %d %s (%v), want a fill on the 98000 -> 100500 path", code, body, err)
	}
	if _, out, _ := postJSON(url, lockTestToken, `{"op":"get","symbol":"BTC/USDT"}`); !strings.Contains(out, `"model":"set"`) && !strings.Contains(out, `"model":"path"`) {
		t.Errorf("get = %s", out)
	}

	// exchange.req: one entry per request; the stage that answered, or none.
	if code, _ := sandboxCall(t, f, "GET", "binance/api.binance.com/api/v3/ticker/24hr?symbol=NOPE"); code != http.StatusNotFound {
		t.Fatalf("unmatched request = %d, want the router's 404", code)
	}
	reqs := journalEntries(t, f.url("/api/sandbox/lab/inspect"), "exchange.req")
	if len(reqs) != 2 {
		t.Fatalf("exchange.req entries = %v, want the order and the unmatched request", reqs)
	}
	if reqs[0]["stage"] != "orders" || reqs[0]["method"] != "POST" || reqs[0]["path"] != "/fapi/v1/order" ||
		reqs[0]["venue"] != "binance" || reqs[0]["host"] != "fapi.binance.com" || reqs[0]["status"] != float64(200) {
		t.Errorf("order entry = %v", reqs[0])
	}
	if reqs[1]["stage"] != "none" || reqs[1]["status"] != float64(404) {
		t.Errorf("unmatched entry = %v", reqs[1])
	}

	// A reload builds a new generation; the book (and the group) stay.
	if err := f.reload(func(c *config.Config) { c.Debug.DevMCP.Enabled = true }); err != nil {
		t.Fatal(err)
	}
	if !mcpTools(t, f.assemblyFixture, lockTestToken)["sandbox_market"] {
		t.Fatal("tools/list after the reload has no sandbox_market")
	}
	if _, out, _ := postJSON(url, lockTestToken, `{"op":"get"}`); !strings.Contains(out, `"BTC/USDT"`) {
		t.Errorf("get after the reload = %s", out)
	}
}

// An ordinary gateway has no market route or tool, and its sandbox server (the
// developer sandbox) answers with the bare simulator, not a chain.
func TestMarketGroup_NotRegisteredUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if code, body, err := postJSON(f.url("/api/sandbox/lab/market"), testSandboxToken, `{"op":"get"}`); err != nil || code != http.StatusNotFound {
		t.Fatalf("POST /api/sandbox/lab/market = %d (%v) %s, want 404", code, err, body)
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_market"] {
		t.Fatal("an unlocked gateway lists sandbox_market")
	}
	if f.services.SandboxServer == nil {
		t.Fatal("the fixture did not start a developer sandbox server")
	}
	if _, ok := f.services.SandboxServer.GetResponder().(*sandbox.StatefulSimulator); !ok {
		t.Fatalf("the unlocked sandbox responder is %T, want the bare *StatefulSimulator", f.services.SandboxServer.GetResponder())
	}
}
