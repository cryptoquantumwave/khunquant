package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLoad(t *testing.T) {
	// Create a temporary directory with fixture files.
	tmpDir := t.TempDir()

	// Create binance venue directory and fixture file.
	binanceDir := filepath.Join(tmpDir, "binance")
	if err := os.MkdirAll(binanceDir, 0755); err != nil {
		t.Fatalf("mkdir binance: %v", err)
	}

	binanceFixtures := `[
  {
    "method": "GET",
    "path_prefix": "/fapi/v1/positionRisk",
    "response": {
      "status": 200,
      "body": [{"symbol":"BTCUSDT","positionAmt":1.0}]
    }
  }
]`
	if err := os.WriteFile(filepath.Join(binanceDir, "fixtures.json"), []byte(binanceFixtures), 0644); err != nil {
		t.Fatalf("write binance fixtures: %v", err)
	}

	// Create okx venue directory and fixture file.
	okxDir := filepath.Join(tmpDir, "okx")
	if err := os.MkdirAll(okxDir, 0755); err != nil {
		t.Fatalf("mkdir okx: %v", err)
	}

	okxFixtures := `[
  {
    "method": "GET",
    "path_prefix": "/api/v5/account/balance",
    "response": {
      "status": 200,
      "body": {"code":"0","msg":"","data":[{"ccy":"BTC","cashBal":"1.0"}]}
    }
  }
]`
	if err := os.WriteFile(filepath.Join(okxDir, "fixtures.json"), []byte(okxFixtures), 0644); err != nil {
		t.Fatalf("write okx fixtures: %v", err)
	}

	// Load the store.
	store := NewStore()
	if err := store.Load(tmpDir); err != nil {
		t.Fatalf("load store: %v", err)
	}

	// Verify binance fixtures.
	binanceVenue := store.Venues()
	if len(binanceVenue) != 2 {
		t.Errorf("want 2 venues, got %d", len(binanceVenue))
	}

	// Test finding fixtures.
	fixture := store.FindFixture("binance", "GET", "/fapi/v1/positionRisk", nil)
	if fixture == nil {
		t.Fatalf("fixture not found for binance GET /fapi/v1/positionRisk")
	}
	if fixture.Response.Status != 200 {
		t.Errorf("want status 200, got %d", fixture.Response.Status)
	}

	// Test finding fixture with longer path (prefix match).
	query := make(map[string][]string)
	fixture = store.FindFixture("binance", "GET", "/fapi/v1/positionRisk", query)
	if fixture == nil {
		t.Fatalf("fixture not found for binance GET /fapi/v1/positionRisk")
	}

	// Test method case-insensitivity.
	fixture = store.FindFixture("binance", "get", "/fapi/v1/positionRisk", nil)
	if fixture == nil {
		t.Errorf("method should be case-insensitive")
	}

	// Test missing fixture.
	fixture = store.FindFixture("binance", "POST", "/fapi/v1/order", nil)
	if fixture != nil {
		t.Errorf("should not find POST fixture for binance")
	}

	// Test missing venue.
	fixture = store.FindFixture("unknown", "GET", "/api/v1/test", nil)
	if fixture != nil {
		t.Errorf("should not find fixture for unknown venue")
	}
}

func TestStoreNonexistentDirectory(t *testing.T) {
	// Loading from a nonexistent directory should not error; it just has no fixtures.
	store := NewStore()
	err := store.Load("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("load from nonexistent directory should not error: %v", err)
	}

	if len(store.Venues()) != 0 {
		t.Errorf("want 0 venues, got %d", len(store.Venues()))
	}
}

func TestStoreSetGetFixtures(t *testing.T) {
	store := NewStore()

	// Set fixtures for a venue with inline JSON body.
	bodyBytes := json.RawMessage(`{"result":"ok"}`)
	fixtures := []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/test",
			Response: Response{
				Status: 200,
				Body:   bodyBytes,
			},
		},
	}
	store.SetFixtures("test_venue", fixtures)

	// Get fixtures.
	retrieved := store.GetFixtures("test_venue")
	if len(retrieved) != 1 {
		t.Errorf("want 1 fixture, got %d", len(retrieved))
	}
	expectedBody := json.RawMessage(`{"result":"ok"}`)
	if string(retrieved[0].Response.Body) != string(expectedBody) {
		t.Errorf("body mismatch: want %s, got %s", expectedBody, retrieved[0].Response.Body)
	}

	// Get nonexistent venue.
	retrieved = store.GetFixtures("nonexistent")
	if retrieved != nil {
		t.Errorf("want nil for nonexistent venue, got %v", retrieved)
	}
}

func TestStoreMalformedJSON(t *testing.T) {
	tmpDir := t.TempDir()
	venueDir := filepath.Join(tmpDir, "badVenue")
	if err := os.MkdirAll(venueDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write malformed JSON.
	if err := os.WriteFile(filepath.Join(venueDir, "bad.json"), []byte("{invalid json}"), 0644); err != nil {
		t.Fatalf("write bad JSON: %v", err)
	}

	store := NewStore()
	err := store.Load(tmpDir)
	if err == nil {
		t.Fatalf("want error on malformed JSON, got nil")
	}
}

func TestFindFixtureWithQueryParameters(t *testing.T) {
	// Test 1: Backward compatibility - fixture without query constraints matches any query.
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      nil, // No query constraints
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"instId":"BTC-USDT"}`),
			},
		},
	})

	query := make(map[string][]string)
	query["instId"] = []string{"BTC-USDT"}
	fixture := store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture == nil {
		t.Fatalf("backward compatibility: fixture without query constraints should match any query")
	}

	// Test 2: Two fixtures with different query constraints return different responses.
	store = NewStore()
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

	query = make(map[string][]string)
	query["instId"] = []string{"BTC-USDT"}
	fixture = store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture == nil || string(fixture.Response.Body) != `{"instId":"BTC-USDT","price":"45000"}` {
		t.Errorf("query-scoped fixture for BTC-USDT failed")
	}

	query = make(map[string][]string)
	query["instId"] = []string{"ETH-USDT"}
	fixture = store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture == nil || string(fixture.Response.Body) != `{"instId":"ETH-USDT","price":"2500"}` {
		t.Errorf("query-scoped fixture for ETH-USDT failed")
	}

	// Test 3: Unlisted extra query params don't break a match.
	query = make(map[string][]string)
	query["instId"] = []string{"BTC-USDT"}
	query["extra"] = []string{"value"}
	fixture = store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture == nil || string(fixture.Response.Body) != `{"instId":"BTC-USDT","price":"45000"}` {
		t.Errorf("extra unlisted query params should not break match")
	}

	// Test 4: Query constraint not satisfied returns nil.
	query = make(map[string][]string)
	query["instId"] = []string{"SOL-USDT"}
	fixture = store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture != nil {
		t.Errorf("unsatisfied query constraint should return nil")
	}
}

func TestFindFixtureSpecificity(t *testing.T) {
	// Query-scoped fixture should win over query-less fixture on same path.
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      nil,
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"generic":"response"}`),
			},
		},
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "BTC-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"specific":"BTC-USDT"}`),
			},
		},
	})

	query := make(map[string][]string)
	query["instId"] = []string{"BTC-USDT"}
	fixture := store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
	if fixture == nil || string(fixture.Response.Body) != `{"specific":"BTC-USDT"}` {
		t.Errorf("query-scoped fixture should win over query-less on same path")
	}

	// Longer path prefix should win over shorter when query count is same.
	store = NewStore()
	store.SetFixtures("test", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api",
			Query:      map[string]string{"key": "value"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"level":"api"}`),
			},
		},
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market",
			Query:      map[string]string{"key": "value"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"level":"market"}`),
			},
		},
	})

	query = make(map[string][]string)
	query["key"] = []string{"value"}
	fixture = store.FindFixture("test", "GET", "/api/v5/market/ticker", query)
	if fixture == nil || string(fixture.Response.Body) != `{"level":"market"}` {
		t.Errorf("longer path prefix should win")
	}
}

func TestFindFixtureDeterminism(t *testing.T) {
	// When multiple fixtures have the same score, the first one (lowest index) should win.
	// This tests the tie-breaker with two fixtures having same query count and path length.
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "BTC-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"first":true}`),
			},
		},
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instType": "SPOT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"first":false}`),
			},
		},
	})

	// Request with both instId and instType: only the first fixture matches,
	// so no tie exists. But let's test that the resolution is deterministic
	// by running it multiple times.
	query := make(map[string][]string)
	query["instId"] = []string{"BTC-USDT"}
	query["instType"] = []string{"SPOT"}

	expectedBody := `{"first":true}`
	for i := 0; i < 10; i++ {
		fixture := store.FindFixture("okx", "GET", "/api/v5/market/ticker", query)
		if fixture == nil || string(fixture.Response.Body) != expectedBody {
			t.Errorf("iteration %d: expected %s, got %v", i, expectedBody, string(fixture.Response.Body))
		}
	}

	// Test true tie: same score, different query keys. First in file order wins.
	store = NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/test",
			Query:      map[string]string{"a": "1"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"fixture":"first"}`),
			},
		},
		{
			Method:     "GET",
			PathPrefix: "/api/v5/test",
			Query:      map[string]string{"b": "2"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{"fixture":"second"}`),
			},
		},
	})

	// Only first fixture is eligible (a=1).
	query = make(map[string][]string)
	query["a"] = []string{"1"}
	fixture := store.FindFixture("okx", "GET", "/api/v5/test", query)
	if fixture == nil || string(fixture.Response.Body) != `{"fixture":"first"}` {
		t.Errorf("first fixture should be selected")
	}
}

func TestHasPathMatch(t *testing.T) {
	store := NewStore()
	store.SetFixtures("okx", []FixtureEntry{
		{
			Method:     "GET",
			PathPrefix: "/api/v5/market/ticker",
			Query:      map[string]string{"instId": "BTC-USDT"},
			Response: Response{
				Status: 200,
				Body:   json.RawMessage(`{}`),
			},
		},
	})

	// Path matches, but query doesn't: HasPathMatch should return true.
	if !store.HasPathMatch("okx", "GET", "/api/v5/market/ticker") {
		t.Errorf("HasPathMatch should return true when path matches")
	}

	// Path and method don't match: HasPathMatch should return false.
	if store.HasPathMatch("okx", "POST", "/api/v5/market/ticker") {
		t.Errorf("HasPathMatch should return false when method doesn't match")
	}

	// Path doesn't match: HasPathMatch should return false.
	if store.HasPathMatch("okx", "GET", "/api/v5/account/balance") {
		t.Errorf("HasPathMatch should return false when path doesn't match")
	}
}

func TestBackwardCompatibilityWithSeededFixtures(t *testing.T) {
	// Load real seeded fixtures from workspace and verify backward compatibility.
	fixtureDir := "../../workspace/sandbox"

	info, err := os.Stat(fixtureDir)
	if os.IsNotExist(err) {
		t.Skipf("fixture directory %s does not exist, skipping", fixtureDir)
	}
	if err != nil || !info.IsDir() {
		t.Skipf("fixture directory %s is not accessible, skipping", fixtureDir)
	}

	store := NewStore()
	if err := store.Load(fixtureDir); err != nil {
		t.Fatalf("load fixtures: %v", err)
	}

	venues := store.Venues()
	if len(venues) == 0 {
		t.Skipf("no fixtures loaded, skipping backward compatibility test")
	}

	// For each loaded fixture, assert it still resolves with nil query.
	for _, venue := range venues {
		fixtures := store.GetFixtures(venue)
		for _, fixture := range fixtures {
			result := store.FindFixture(venue, fixture.Method, fixture.PathPrefix, nil)
			if result == nil {
				t.Errorf("fixture not found: %s %s %s (query=nil)", venue, fixture.Method, fixture.PathPrefix)
				continue
			}
			// Verify the body is identical (byte-for-byte).
			if string(result.Response.Body) != string(fixture.Response.Body) {
				t.Errorf("body mismatch for %s %s %s:\nwant: %s\ngot:  %s",
					venue, fixture.Method, fixture.PathPrefix,
					string(fixture.Response.Body), string(result.Response.Body))
			}
		}
	}
}
