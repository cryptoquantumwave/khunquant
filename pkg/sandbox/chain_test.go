package sandbox

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func chainReq(path string) *http.Request {
	return httptest.NewRequest("GET", "/__sbx__/binance/api.binance.com"+path, nil)
}

func tag(order *[]string, name string, answer bool) Middleware {
	return func(next Responder) Responder {
		return ResponderFunc(func(v, m, p string, r *http.Request) (*Response, bool) {
			*order = append(*order, name)
			if answer {
				return &Response{Status: 200, BodyText: name}, true
			}
			return next.Respond(v, m, p, r)
		})
	}
}

func TestChain_OrderAndNext(t *testing.T) {
	store := NewStore()
	store.SetFixtures("binance", []FixtureEntry{{Method: "GET", PathPrefix: "/fx", Response: Response{Status: 200, BodyText: "fixture"}}})
	c := NewChain(store)
	var order []string
	c.Set(StageOrders, tag(&order, "orders", false))
	c.Set(StageFault, tag(&order, "fault", false))
	c.Set(StageMarket, tag(&order, "market", false))
	// StageWeb stays nil: skipped.
	resp, ok := c.Respond("binance", "GET", "/fx", chainReq("/fx"))
	if !ok || resp.BodyText != "fixture" {
		t.Fatalf("got %v %v, want the fixture", resp, ok)
	}
	if got := strings.Join(order, ","); got != "fault,market,orders" {
		t.Fatalf("order = %s", got)
	}

	// A stage can call next and post-process what comes back.
	c.Set(StageMarket, func(next Responder) Responder {
		return ResponderFunc(func(v, m, p string, r *http.Request) (*Response, bool) {
			resp, ok := next.Respond(v, m, p, r)
			if !ok {
				return nil, false
			}
			return &Response{Status: 200, BodyText: resp.BodyText + "+filtered"}, true
		})
	})
	if resp, ok := c.Respond("binance", "GET", "/fx", chainReq("/fx")); !ok || resp.BodyText != "fixture+filtered" {
		t.Fatalf("post-processed = %v %v", resp, ok)
	}
	// Set(nil) clears a stage.
	c.Set(StageMarket, nil)
	if resp, _ := c.Respond("binance", "GET", "/fx", chainReq("/fx")); resp.BodyText != "fixture" {
		t.Fatalf("after clearing: %v", resp)
	}
}

func TestChain_JournalsExchangeReq(t *testing.T) {
	store := NewStore()
	store.SetFixtures("binance", []FixtureEntry{{Method: "GET", PathPrefix: "/fx", Response: Response{Status: 200, BodyText: "f"}}})
	c := NewChain(store)
	var order []string
	c.Set(StageMarket, func(next Responder) Responder {
		return ResponderFunc(func(v, m, p string, r *http.Request) (*Response, bool) {
			if p == "/mk" {
				return &Response{Status: 201}, true
			}
			return next.Respond(v, m, p, r)
		})
	})
	c.Set(StageFault, tag(&order, "fault", false))
	var mu sync.Mutex
	var got []ReqInfo
	c.Observe(func(i ReqInfo) { mu.Lock(); got = append(got, i); mu.Unlock() })

	for _, p := range []string{"/mk", "/fx", "/nothing"} {
		c.Respond("binance", "GET", p, chainReq(p))
	}
	want := []ReqInfo{
		{"binance", "api.binance.com", "GET", "/mk", "market", 201},
		{"binance", "api.binance.com", "GET", "/fx", "fixtures", 200},
		{"binance", "api.binance.com", "GET", "/nothing", "none", 404},
	}
	if len(got) != len(want) {
		t.Fatalf("%d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The chain behind a real router: an unmatched path ends in the router's 404 text.
func TestChain_UnmatchedFallsToRouter404(t *testing.T) {
	store := NewStore()
	rec := httptest.NewRecorder()
	BuildRouter(store, NewChain(store))(rec, httptest.NewRequest("GET", "/__sbx__/binance/api.binance.com/api/v3/ticker/24hr?symbol=XYZUSDT", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "no fixture configured") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
