package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// A lost answer through create_order on the two equity venues of the lab (Settrade, Webull).
//
// written_then_timeout makes the simulator book the order and the answer never come. The tool
// sends the order once, waits the settle delay (real time: five seconds) and only looks. The
// look is by the order's shape on Settrade, which takes no client order ID, and by the
// client_order_id in the open and closed listings on Webull, which does. A lookup_lag longer
// than the wait hides the booked order from the look, so the outcome stays unknown. Either
// way the simulator holds exactly the one order: nothing is placed twice.

// equityLostAnswer is one venue's create_order: the placeholder account, a priced symbol, cash,
// and the rule that loses the answer to the first create.
type equityLostAnswer struct {
	venue, symbol, quote string
	args                 string // create_order's arguments
	enable               func(*config.Config)
	// clockStart is a Tuesday inside the venue's continuous session. create_order refuses a
	// closed market, and the adapters read the process clock (broker.Now), which a locked
	// gateway points at the manual sim clock, so the test does not depend on the hour it runs.
	clockStart string
	// standIns are fault rules that answer a route the simulator lacks (a gap of the
	// simulator, not of the tool).
	standIns []string
}

var equityVenues = []equityLostAnswer{
	{
		venue: "settrade", symbol: "PTT", quote: "THB",
		args:       `{"provider":"settrade","symbol":"PTT/THB","type":"limit","side":"buy","amount":100,"price":30,"confirm":true}`,
		enable:     func(c *config.Config) { _ = sandbox.ApplyPlaceholderAccounts(c, []string{"settrade"}) },
		clockStart: "2026-03-10T10:30:00+07:00", // SET morning session, 10:00-12:30 ICT
		// Settrade takes no client order ID, so the look is by shape and reads the account's
		// trades too (SEOS v4), a route the simulator does not serve: an empty list stands in.
		standIns: []string{`{"op":"add","match":{"venue":"settrade","method":"GET","path_prefix":"/api/seos/v4/"},"effect":{"body":{"data":[]}}}`},
	},
	{
		venue: "webull", symbol: "AAPL", quote: "USD",
		args:       `{"provider":"webull","symbol":"AAPL/USD","type":"market","side":"buy","amount":1,"confirm":true}`,
		enable:     func(c *config.Config) { _ = sandbox.ApplyPlaceholderAccounts(c, []string{"webull"}) },
		clockStart: "2026-03-10T11:00:00-04:00", // NYSE regular hours, 09:30-16:00 EDT
	},
}

func equityLostAnswerLab(t *testing.T, v equityLostAnswer, extraFaults ...string) (f *lockedFixture, countOrders func() int) {
	t.Helper()
	prev := faultMaxHold
	faultMaxHold = 300 * time.Millisecond
	t.Cleanup(func() { faultMaxHold = prev })
	prevClient := testHTTPClient
	testHTTPClient = &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(func() { testHTTPClient = prevClient })
	broker.ResetInstanceCache()
	t.Cleanup(broker.ResetInstanceCache)
	t.Setenv(lab.EnvClockStart, v.clockStart)
	t.Setenv(lab.EnvClockMode, "manual")
	// The fixture does not bind the process clock (gatewayCmd does); the manual clock never
	// moves in these tests, so the adapters' market gate reads its start.
	at, err := time.Parse(time.RFC3339, v.clockStart)
	if err != nil {
		t.Fatal(err)
	}
	broker.SetClock(func() time.Time { return at })
	t.Cleanup(func() { broker.SetClock(nil) })
	f = newLockedFixture(t, ownerConfig, func(c *config.Config) {
		c.Tools.CreateOrder.Enabled = true
		v.enable(c)
	})
	for _, body := range append([]string{
		`{"op":"set","venue":"` + v.venue + `","symbol":"` + v.symbol + "/" + v.quote + `","price":30}`,
	}, []string{}...) {
		if code, out := labPost(t, f, "market", body); code != http.StatusOK {
			t.Fatalf("market %s = %d %v", body, code, out)
		}
	}
	if code, out := labPost(t, f, "account", `{"op":"seed","venue":"`+v.venue+`","balances":{"`+v.quote+`":1000000}}`); code != http.StatusOK {
		t.Fatalf("seed = %d %v", code, out)
	}
	for _, r := range append(append([]string{}, v.standIns...), append([]string{
		`{"op":"add","id":"wtt","match":{"venue":"` + v.venue + `","order":"create"},"effect":{"order":"written_then_timeout"},"when":{"times":1}}`,
	}, extraFaults...)...) {
		if code, out := labPost(t, f, "fault", r); code != http.StatusOK {
			t.Fatalf("add %s = %d %v", r, code, out)
		}
	}
	countOrders = func() int {
		code, orders := labPost(t, f, "account", `{"op":"orders","venue":"`+v.venue+`"}`)
		if code != http.StatusOK {
			t.Fatalf("account orders = %d %v", code, orders)
		}
		list, _ := orders["orders"].([]any)
		return len(list)
	}
	return f, countOrders
}

func firedOnce(t *testing.T, f *lockedFixture, id string) {
	t.Helper()
	code, out := labPost(t, f, "fault", `{"op":"list"}`)
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	rules, _ := out["rules"].([]any)
	for _, r := range rules {
		st, _ := r.(map[string]any)
		rule, _ := st["rule"].(map[string]any)
		if rid, _ := rule["id"].(string); rid == id {
			if n, _ := st["fired"].(float64); n != 1 {
				t.Fatalf("rule %s fired %v times, want 1 (else nothing was lost)", id, n)
			}
			return
		}
	}
	t.Fatalf("rule %s not listed: %v", id, out)
}

// Found: the booked order is visible to the look after the wait, so create_order carries on as
// a success that says the answer was lost, and the simulator holds the one order.
func TestCreateOrder_LostAnswer_EquityVenues_Found(t *testing.T) {
	for _, v := range equityVenues {
		t.Run(v.venue, func(t *testing.T) {
			f, countOrders := equityLostAnswerLab(t, v)
			start := time.Now()
			code, out := callToolVia(t, f, `{"name":"create_order","as":"owner","args":`+v.args+`}`)
			t.Logf("create_order = %d %v (%s)", code, out, time.Since(start))
			text, _ := out["for_llm"].(string)
			if code != http.StatusOK || out["is_error"] != false {
				t.Fatalf("the tool answered %d %v, want 200 with is_error: false", code, out)
			}
			if !strings.Contains(text, "answer to this order was lost") || strings.Contains(text, "OUTCOME UNKNOWN") {
				t.Errorf("a found order must say the answer was lost and not read as unknown: %s", text)
			}
			if d := time.Since(start); d < 5*time.Second {
				t.Errorf("the tool answered after %s, want it to wait out the 5s settle delay before looking", d)
			}
			firedOnce(t, f, "wtt")
			if n := countOrders(); n != 1 {
				t.Fatalf("the simulator holds %d orders, want the one it booked (nothing placed twice)", n)
			}
		})
	}
}

// Unknown: a lookup_lag longer than the wait hides the booked order from the look. The result
// is an unknown outcome that names no failure and tells the caller to check before placing it
// again, and the simulator still holds exactly the one order.
func TestCreateOrder_LostAnswer_EquityVenues_Unknown(t *testing.T) {
	for _, v := range equityVenues {
		t.Run(v.venue, func(t *testing.T) {
			f, countOrders := equityLostAnswerLab(t, v,
				`{"op":"add","id":"lag","match":{"venue":"`+v.venue+`"},"effect":{"order":"lookup_lag","lag":"30s"},"when":{"times":1}}`)
			start := time.Now()
			code, out := callToolVia(t, f, `{"name":"create_order","as":"owner","args":`+v.args+`}`)
			t.Logf("create_order = %d %v (%s)", code, out, time.Since(start))
			text, _ := out["for_llm"].(string)
			if code != http.StatusOK || out["is_error"] != true {
				t.Fatalf("the tool answered %d %v, want 200 with is_error: true", code, out)
			}
			for _, want := range []string{"ORDER OUTCOME UNKNOWN", "may exist", "before placing this order again"} {
				if !strings.Contains(text, want) {
					t.Errorf("the result must say %q, got: %s", want, text)
				}
			}
			if strings.Contains(strings.ToLower(text), "createorder failed") {
				t.Errorf("an unknown outcome must not read as a failure: %s", text)
			}
			if d := time.Since(start); d < 5*time.Second {
				t.Errorf("the tool answered after %s, want it to wait out the 5s settle delay before looking", d)
			}
			firedOnce(t, f, "wtt")
			firedOnce(t, f, "lag")
			if n := countOrders(); n != 1 {
				t.Fatalf("the simulator holds %d orders, want the one it booked (nothing placed twice)", n)
			}
		})
	}
}
