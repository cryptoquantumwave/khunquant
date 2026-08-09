package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterHit(t *testing.T) {
	// Create a store with a fixture.
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"result":"ok"}`),
				Headers: map[string]string{
					"X-Custom": "header-value",
				},
			},
		},
	})

	// Build the router.
	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Make a request.
	resp, err := http.Get(server.URL + "/__sbx__/test/api.test.com/api/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("want status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"result":"ok"}` {
		t.Errorf("want {\"result\":\"ok\"}, got %s", string(body))
	}

	if resp.Header.Get("X-Custom") != "header-value" {
		t.Errorf("want header X-Custom=header-value, got %s", resp.Header.Get("X-Custom"))
	}
}

func TestRouterMiss(t *testing.T) {
	// Create an empty store.
	store := NewStore()

	// Build the router.
	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Make a request that doesn't match.
	resp, err := http.Get(server.URL + "/__sbx__/okx/api.okx.com/api/v5/account/balance")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want status 404, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var errResp errorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}

	// Verify the error message names the venue, method and path.
	if !contains(errResp.Error, "okx") {
		t.Errorf("error should name venue 'okx': %s", errResp.Error)
	}
	if !contains(errResp.Error, "GET") {
		t.Errorf("error should name method 'GET': %s", errResp.Error)
	}
	if !contains(errResp.Error, "/api/v5/account/balance") {
		t.Errorf("error should name path '/api/v5/account/balance': %s", errResp.Error)
	}
}

func TestRouterResponderFallthrough(t *testing.T) {
	// Create a responder that only responds to POST requests.
	responder := ResponderFunc(func(venue, method, path string, r *http.Request) (*Response, bool) {
		if method == "POST" {
			return &Response{
				Status: 201,
				Body:   json.RawMessage(`{"id":123}`),
			}, true
		}
		return nil, false // fall through
	})

	// Create a store with a fixture for GET.
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"data":"ok"}`),
			},
		},
	})

	// Build the router with the responder.
	handler := BuildRouter(store, responder)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Test GET: should hit the fixture.
	resp, err := http.Get(server.URL + "/__sbx__/test/api.test.com/api/test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("GET: want 200, got %d", resp.StatusCode)
	}

	// Test POST: should hit the responder.
	req, err := http.NewRequest("POST", server.URL+"/__sbx__/test/api.test.com/api/test", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("POST: want 201, got %d", resp.StatusCode)
	}
}

func TestRouterInvalidPath(t *testing.T) {
	store := NewStore()
	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Request with invalid sandbox path.
	resp, err := http.Get(server.URL + "/not/a/sandbox/path")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("want status 400, got %d", resp.StatusCode)
	}
}

func TestRouterPrefixMatch(t *testing.T) {
	// Create a store with a fixture that has a path prefix.
	store := NewStore()
	store.SetFixtures("binance", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/fapi/v1",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"data":"matched"}`),
			},
		},
	})

	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Test requests with the same prefix.
	tests := []string{
		"/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk",
		"/__sbx__/binance/fapi.binance.com/fapi/v1/order/openOrders",
		"/__sbx__/binance/fapi.binance.com/fapi/v1",
	}

	for _, path := range tests {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Errorf("path %s: get failed: %v", path, err)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("path %s: want 200, got %d", path, resp.StatusCode)
		}
	}
}

func TestRouterMethodCaseInsensitive(t *testing.T) {
	store := NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "post",
			PathPrefix: "/api",
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"ok":true}`),
			},
		},
	})

	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Test with uppercase method.
	req, err := http.NewRequest("POST", server.URL+"/__sbx__/test/api.test.com/api", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("want 200, got %d", resp.StatusCode)
	}
}

func TestRouterQueryMatch(t *testing.T) {
	// Create a store with query-scoped fixtures.
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "BTC-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"instId":"BTC-USDT","price":"45000"}`),
			},
		},
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "ETH-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"instId":"ETH-USDT","price":"2500"}`),
			},
		},
	})

	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Request for BTC-USDT.
	resp, err := http.Get(server.URL + "/__sbx__/okx/api.okx.com/api/v5/market/ticker?instId=BTC-USDT")
	if err != nil {
		t.Fatalf("get BTC-USDT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("BTC-USDT: want status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !contains(string(body), "BTC-USDT") {
		t.Errorf("BTC-USDT: expected BTC-USDT in response, got %s", string(body))
	}

	// Request for ETH-USDT.
	resp, err = http.Get(server.URL + "/__sbx__/okx/api.okx.com/api/v5/market/ticker?instId=ETH-USDT")
	if err != nil {
		t.Fatalf("get ETH-USDT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("ETH-USDT: want status 200, got %d", resp.StatusCode)
	}
	body, _ = io.ReadAll(resp.Body)
	if !contains(string(body), "ETH-USDT") {
		t.Errorf("ETH-USDT: expected ETH-USDT in response, got %s", string(body))
	}
}

func TestRouterNearMissError(t *testing.T) {
	// Create a store with a query-scoped fixture.
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "BTC-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"instId":"BTC-USDT"}`),
			},
		},
	})

	handler := BuildRouter(store)
	server := httptest.NewServer(handler)
	defer server.Close()

	// Request for SOL-USDT (query doesn't match, but path does).
	resp, err := http.Get(server.URL + "/__sbx__/okx/api.okx.com/api/v5/market/ticker?instId=SOL-USDT")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("want status 404, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var errResp errorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("unmarshal error response: %v", err)
	}

	// Verify the error message mentions that fixtures exist but query didn't match.
	errorStr := errResp.Error
	if !contains(errorStr, "fixtures exist for this path") {
		t.Errorf("error should mention 'fixtures exist for this path', got: %s", errorStr)
	}
	if !contains(errorStr, "query") {
		t.Errorf("error should mention 'query', got: %s", errorStr)
	}
}

// Helper function to check if a string contains a substring.
func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
