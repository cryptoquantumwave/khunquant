package sandbox

import (
	"net/url"
	"testing"
)

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name        string
		venue       string
		originalURL string
		baseURL     string
		expected    string
	}{
		{
			name:        "binance futures position risk",
			venue:       "binance",
			originalURL: "https://fapi.binance.com/fapi/v1/positionRisk?symbol=BTCUSDT",
			baseURL:     "http://127.0.0.1:8080",
			expected:    "http://127.0.0.1:8080/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk?symbol=BTCUSDT",
		},
		{
			name:        "okx account balance",
			venue:       "okx",
			originalURL: "https://www.okx.com/api/v5/account/balance",
			baseURL:     "http://127.0.0.1:9090",
			expected:    "http://127.0.0.1:9090/__sbx__/okx/www.okx.com/api/v5/account/balance",
		},
		{
			name:        "bitkub market balances",
			venue:       "bitkub",
			originalURL: "https://api.bitkub.com/api/v3/market/balances",
			baseURL:     "http://127.0.0.1:7070",
			expected:    "http://127.0.0.1:7070/__sbx__/bitkub/api.bitkub.com/api/v3/market/balances",
		},
		{
			name:        "url with encoded slash in query",
			venue:       "binance",
			originalURL: "https://fapi.binance.com/fapi/v1/orders?symbol=BTC%2FUSDT",
			baseURL:     "http://127.0.0.1:8080",
			expected:    "http://127.0.0.1:8080/__sbx__/binance/fapi.binance.com/fapi/v1/orders?symbol=BTC%2FUSDT",
		},
		{
			name:        "url with multiple query params",
			venue:       "okx",
			originalURL: "https://www.okx.com/api/v5/account/balance?ccy=BTC&ccy=ETH",
			baseURL:     "http://127.0.0.1:9090",
			expected:    "http://127.0.0.1:9090/__sbx__/okx/www.okx.com/api/v5/account/balance?ccy=BTC&ccy=ETH",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig, err := url.Parse(tc.originalURL)
			if err != nil {
				t.Fatalf("parse original URL: %v", err)
			}

			result, err := BuildURL(tc.venue, orig, tc.baseURL)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}

			resultStr := result.String()

			if resultStr != tc.expected {
				t.Errorf("want %s, got %s", tc.expected, resultStr)
			}
		})
	}
}

func TestParseRequest(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantVenue string
		wantHost  string
		wantPath  string
		wantErr   bool
	}{
		{
			name:      "binance position risk",
			path:      "/__sbx__/binance/fapi.binance.com/fapi/v1/positionRisk",
			wantVenue: "binance",
			wantHost:  "fapi.binance.com",
			wantPath:  "/fapi/v1/positionRisk",
		},
		{
			name:      "okx account balance",
			path:      "/__sbx__/okx/www.okx.com/api/v5/account/balance",
			wantVenue: "okx",
			wantHost:  "www.okx.com",
			wantPath:  "/api/v5/account/balance",
		},
		{
			name:      "bitkub market balances",
			path:      "/__sbx__/bitkub/api.bitkub.com/api/v3/market/balances",
			wantVenue: "bitkub",
			wantHost:  "api.bitkub.com",
			wantPath:  "/api/v3/market/balances",
		},
		{
			name:      "path with encoded slash",
			path:      "/__sbx__/binance/fapi.binance.com/fapi/v1/orders%2Fhistory",
			wantVenue: "binance",
			wantHost:  "fapi.binance.com",
			wantPath:  "/fapi/v1/orders%2Fhistory",
		},
		{
			name:      "root path (no additional path)",
			path:      "/__sbx__/binance/api.binance.com",
			wantVenue: "binance",
			wantHost:  "api.binance.com",
			wantPath:  "/",
		},
		{
			name:    "invalid prefix",
			path:    "/fapi/v1/positionRisk",
			wantErr: true,
		},
		{
			name:    "incomplete path (missing host)",
			path:    "/__sbx__/binance",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseRequest(tc.path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("parse request: %v", err)
			}

			if parsed.Venue != tc.wantVenue {
				t.Errorf("venue: want %s, got %s", tc.wantVenue, parsed.Venue)
			}
			if parsed.Host != tc.wantHost {
				t.Errorf("host: want %s, got %s", tc.wantHost, parsed.Host)
			}
			if parsed.Path != tc.wantPath {
				t.Errorf("path: want %s, got %s", tc.wantPath, parsed.Path)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	// Test that a URL can be parsed back after building.
	testCases := []struct {
		venue       string
		originalURL string
		baseURL     string
	}{
		{
			venue:       "binance",
			originalURL: "https://fapi.binance.com/fapi/v1/positionRisk?symbol=BTCUSDT",
			baseURL:     "http://127.0.0.1:8080",
		},
		{
			venue:       "okx",
			originalURL: "https://www.okx.com/api/v5/account/balance",
			baseURL:     "http://127.0.0.1:9090",
		},
		{
			venue:       "bitkub",
			originalURL: "https://api.bitkub.com/api/v3/market/balances",
			baseURL:     "http://127.0.0.1:7070",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.venue, func(t *testing.T) {
			// Parse original URL.
			orig, err := url.Parse(tc.originalURL)
			if err != nil {
				t.Fatalf("parse original URL: %v", err)
			}

			// Build the sandbox URL.
			sandbox, err := BuildURL(tc.venue, orig, tc.baseURL)
			if err != nil {
				t.Fatalf("BuildURL: %v", err)
			}
			sandboxStr := sandbox.String()

			// Parse the sandbox URL and extract components.
			parsed, err := ParseRequest(sandbox.EscapedPath())
			if err != nil {
				t.Fatalf("parse sandbox path: %v", err)
			}

			// Verify that the components match.
			if parsed.Venue != tc.venue {
				t.Errorf("venue round-trip: want %s, got %s", tc.venue, parsed.Venue)
			}

			// Verify that the reconstructed URL string matches.
			expectedStr := sandbox.String()
			if sandboxStr != expectedStr {
				t.Errorf("round-trip URL string: want %s, got %s", expectedStr, sandboxStr)
			}
		})
	}
}

func TestBuildURLEmptyBaseURL(t *testing.T) {
	orig, _ := url.Parse("https://api.example.com/test")
	_, err := BuildURL("test", orig, "")
	if err == nil {
		t.Fatalf("want error for empty baseURL, got nil")
	}
}

func TestBuildURLMalformedBaseURL(t *testing.T) {
	orig, _ := url.Parse("https://api.example.com/test")
	// Use an invalid URL with invalid characters that url.Parse actually rejects
	_, err := BuildURL("test", orig, "ht!tp://[invalid")
	if err == nil {
		t.Fatalf("want error for malformed baseURL, got nil")
	}
}
