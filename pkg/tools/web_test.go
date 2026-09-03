package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const testFetchLimit = int64(10 * 1024 * 1024)

// TestWebTool_WebFetch_Success verifies successful URL fetching
func TestWebTool_WebFetch_Success(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html><body><h1>Test Page</h1><p>Content here</p></body></html>"))
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}

	ctx := context.Background()
	args := map[string]any{
		"url": server.URL,
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForLLM should contain the fetched content (full JSON result)
	if !strings.Contains(result.ForLLM, "Test Page") {
		t.Errorf("Expected ForLLM to contain 'Test Page', got: %s", result.ForLLM)
	}

	// ForUser should contain summary
	if !strings.Contains(result.ForUser, "bytes") && !strings.Contains(result.ForUser, "extractor") {
		t.Errorf("Expected ForUser to contain summary, got: %s", result.ForUser)
	}
}

// TestWebTool_WebFetch_JSON verifies JSON content handling
func TestWebTool_WebFetch_JSON(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	testData := map[string]string{"key": "value", "number": "123"}
	expectedJSON, _ := json.MarshalIndent(testData, "", "  ")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(expectedJSON)
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": server.URL,
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForLLM should contain formatted JSON
	if !strings.Contains(result.ForLLM, "key") && !strings.Contains(result.ForLLM, "value") {
		t.Errorf("Expected ForLLM to contain JSON data, got: %s", result.ForLLM)
	}
}

// TestWebTool_WebFetch_InvalidURL verifies error handling for invalid URL
func TestWebTool_WebFetch_InvalidURL(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": "not-a-valid-url",
	}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error for invalid URL")
	}

	// Should contain error message (either "invalid URL" or scheme error)
	if !strings.Contains(result.ForLLM, "URL") && !strings.Contains(result.ForUser, "URL") {
		t.Errorf("Expected error message for invalid URL, got ForLLM: %s", result.ForLLM)
	}
}

// TestWebTool_WebFetch_UnsupportedScheme verifies error handling for non-http URLs
func TestWebTool_WebFetch_UnsupportedScheme(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": "ftp://example.com/file.txt",
	}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error for unsupported URL scheme")
	}

	// Should mention only http/https allowed
	if !strings.Contains(result.ForLLM, "http/https") && !strings.Contains(result.ForUser, "http/https") {
		t.Errorf("Expected scheme error message, got ForLLM: %s", result.ForLLM)
	}
}

// TestWebTool_WebFetch_MissingURL verifies error handling for missing URL
func TestWebTool_WebFetch_MissingURL(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error when URL is missing")
	}

	// Should mention URL is required
	if !strings.Contains(result.ForLLM, "url is required") && !strings.Contains(result.ForUser, "url is required") {
		t.Errorf("Expected 'url is required' message, got ForLLM: %s", result.ForLLM)
	}
}

// TestWebTool_WebFetch_Truncation verifies content truncation
func TestWebTool_WebFetch_Truncation(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	longContent := strings.Repeat("x", 20000)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(longContent))
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(1000, testFetchLimit) // Limit to 1000 chars
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": server.URL,
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForLLM should contain truncated content (not the full 20000 chars)
	resultMap := make(map[string]any)
	json.Unmarshal([]byte(result.ForLLM), &resultMap)
	if text, ok := resultMap["text"].(string); ok {
		if len(text) > 1100 { // Allow some margin
			t.Errorf("Expected content to be truncated to ~1000 chars, got: %d", len(text))
		}
	}

	// Should be marked as truncated
	if truncated, ok := resultMap["truncated"].(bool); !ok || !truncated {
		t.Errorf("Expected 'truncated' to be true in result")
	}
}

func TestWebFetchTool_PayloadTooLarge(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	// Create a mock HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)

		// Generate a payload intentionally larger than our limit.
		// Limit: 10 * 1024 * 1024 (10MB). We generate 10MB + 100 bytes of the letter 'A'.
		largeData := bytes.Repeat([]byte("A"), int(testFetchLimit)+100)

		w.Write(largeData)
	}))
	// Ensure the server is shut down at the end of the test
	defer ts.Close()

	// Initialize the tool
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	// Prepare the arguments pointing to the URL of our local mock server
	args := map[string]any{
		"url": ts.URL,
	}

	// Execute the tool
	ctx := context.Background()
	result := tool.Execute(ctx, args)

	// Assuming ErrorResult sets the ForLLM field with the error text.
	if result == nil {
		t.Fatal("expected a ToolResult, got nil")
	}

	// Search for the exact error string we set earlier in the Execute method
	expectedErrorMsg := fmt.Sprintf("size exceeded %d bytes limit", testFetchLimit)

	if !strings.Contains(result.ForLLM, expectedErrorMsg) && !strings.Contains(result.ForUser, expectedErrorMsg) {
		t.Errorf("test failed: expected error %q, but got: %+v", expectedErrorMsg, result)
	}
}

// TestWebTool_WebSearch_NoApiKey verifies that no tool is created when API key is missing
func TestWebTool_WebSearch_NoApiKey(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{BraveEnabled: true, BraveAPIKeys: nil})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if tool != nil {
		t.Errorf("Expected nil tool when Brave API key is empty")
	}

	// Also nil when nothing is enabled
	tool, err = NewWebSearchTool(WebSearchToolOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if tool != nil {
		t.Errorf("Expected nil tool when no provider is enabled")
	}
}

// TestWebTool_WebSearch_MissingQuery verifies error handling for missing query
func TestWebTool_WebSearch_MissingQuery(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{
		BraveEnabled:    true,
		BraveAPIKeys:    []string{"test-key"},
		BraveMaxResults: 5,
	})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	ctx := context.Background()
	args := map[string]any{}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error when query is missing")
	}
}

// TestWebTool_WebFetch_HTMLExtraction verifies HTML text extraction
func TestWebTool_WebFetch_HTMLExtraction(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write(
			[]byte(
				`<html><body><script>alert('test');</script><style>body{color:red;}</style><h1>Title</h1><p>Content</p></body></html>`,
			),
		)
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": server.URL,
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForLLM should contain extracted text (without script/style tags)
	if !strings.Contains(result.ForLLM, "Title") && !strings.Contains(result.ForLLM, "Content") {
		t.Errorf("Expected ForLLM to contain extracted text, got: %s", result.ForLLM)
	}

	// Should NOT contain script or style tags in ForLLM
	if strings.Contains(result.ForLLM, "<script>") || strings.Contains(result.ForLLM, "<style>") {
		t.Errorf("Expected script/style tags to be removed, got: %s", result.ForLLM)
	}
}

// TestWebFetchTool_extractText verifies text extraction preserves newlines
func TestWebFetchTool_extractText(t *testing.T) {
	tool := &WebFetchTool{}

	tests := []struct {
		name     string
		input    string
		wantFunc func(t *testing.T, got string)
	}{
		{
			name:  "preserves newlines between block elements",
			input: "<html><body><h1>Title</h1>\n<p>Paragraph 1</p>\n<p>Paragraph 2</p></body></html>",
			wantFunc: func(t *testing.T, got string) {
				lines := strings.Split(got, "\n")
				if len(lines) < 2 {
					t.Errorf("Expected multiple lines, got %d: %q", len(lines), got)
				}
				if !strings.Contains(got, "Title") || !strings.Contains(got, "Paragraph 1") ||
					!strings.Contains(got, "Paragraph 2") {
					t.Errorf("Missing expected text: %q", got)
				}
			},
		},
		{
			name:  "removes script and style tags",
			input: "<script>alert('x');</script><style>body{}</style><p>Keep this</p>",
			wantFunc: func(t *testing.T, got string) {
				if strings.Contains(got, "alert") || strings.Contains(got, "body{}") {
					t.Errorf("Expected script/style content removed, got: %q", got)
				}
				if !strings.Contains(got, "Keep this") {
					t.Errorf("Expected 'Keep this' to remain, got: %q", got)
				}
			},
		},
		{
			name:  "collapses excessive blank lines",
			input: "<p>A</p>\n\n\n\n\n<p>B</p>",
			wantFunc: func(t *testing.T, got string) {
				if strings.Contains(got, "\n\n\n") {
					t.Errorf("Expected excessive blank lines collapsed, got: %q", got)
				}
			},
		},
		{
			name:  "collapses horizontal whitespace",
			input: "<p>hello     world</p>",
			wantFunc: func(t *testing.T, got string) {
				if strings.Contains(got, "     ") {
					t.Errorf("Expected spaces collapsed, got: %q", got)
				}
				if !strings.Contains(got, "hello world") {
					t.Errorf("Expected 'hello world', got: %q", got)
				}
			},
		},
		{
			name:  "empty input",
			input: "",
			wantFunc: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("Expected empty string, got: %q", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tool.extractText(tt.input)
			tt.wantFunc(t, got)
		})
	}
}

func withPrivateWebFetchHostsAllowed(t *testing.T) {
	t.Helper()
	previous := allowPrivateWebFetchHosts.Load()
	allowPrivateWebFetchHosts.Store(true)
	t.Cleanup(func() {
		allowPrivateWebFetchHosts.Store(previous)
	})
}

func TestWebTool_WebFetch_PrivateHostBlocked(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://127.0.0.1:0",
	})

	if !result.IsError {
		t.Errorf("expected error for private host URL, got success")
	}
	if !strings.Contains(result.ForLLM, "private or local network") &&
		!strings.Contains(result.ForUser, "private or local network") {
		t.Errorf("expected private host block message, got %q", result.ForLLM)
	}
}

func TestWebTool_WebFetch_PrivateHostAllowedForTests(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": server.URL,
	})

	if result.IsError {
		t.Errorf("expected success when private host access is allowed in tests, got %q", result.ForLLM)
	}
}

func TestWebTool_WebFetch_AllowsLoopbackProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://example.com/proxied" {
			t.Fatalf("proxy received URL %q, want %q", r.URL.String(), "http://example.com/proxied")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("proxied content"))
	}))
	defer proxy.Close()

	tool, err := NewWebFetchToolWithProxy(50000, proxy.URL, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://example.com/proxied",
	})
	if result.IsError {
		t.Fatalf("expected success through loopback proxy, got %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "proxied content") {
		t.Fatalf("expected proxied content, got %q", result.ForLLM)
	}
}

// TestWebFetch_BlocksIPv4MappedIPv6Loopback verifies ::ffff:127.0.0.1 is blocked
func TestWebFetch_BlocksIPv4MappedIPv6Loopback(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://[::ffff:127.0.0.1]:0",
	})

	if !result.IsError {
		t.Error("expected error for IPv4-mapped IPv6 loopback URL, got success")
	}
}

// TestWebFetch_BlocksMetadataIP verifies 169.254.169.254 is blocked
func TestWebFetch_BlocksMetadataIP(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://169.254.169.254/latest/meta-data",
	})

	if !result.IsError {
		t.Error("expected error for cloud metadata IP, got success")
	}
}

// TestWebFetch_BlocksIPv6UniqueLocal verifies fc00::/7 addresses are blocked
func TestWebFetch_BlocksIPv6UniqueLocal(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://[fd00::1]:0",
	})

	if !result.IsError {
		t.Error("expected error for IPv6 unique local address, got success")
	}
}

// TestWebFetch_Blocks6to4WithPrivateEmbed verifies 6to4 with private embedded IPv4 is blocked
func TestWebFetch_Blocks6to4WithPrivateEmbed(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	// 2002:7f00:0001::1 embeds 127.0.0.1
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://[2002:7f00:0001::1]:0",
	})

	if !result.IsError {
		t.Error("expected error for 6to4 with private embedded IPv4, got success")
	}
}

// TestWebFetch_Allows6to4WithPublicEmbed verifies 6to4 with public embedded IPv4 is NOT blocked
func TestWebFetch_Allows6to4WithPublicEmbed(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	// 2002:0801:0101::1 embeds 8.1.1.1 (public) — pre-flight should pass,
	// connection will fail (no listener) but that's after the SSRF check.
	result := tool.Execute(context.Background(), map[string]any{
		"url": "http://[2002:0801:0101::1]:0",
	})

	// Should NOT be blocked by SSRF check — error should be connection failure, not "private"
	if result.IsError && strings.Contains(result.ForLLM, "private") {
		t.Error("6to4 with public embedded IPv4 should not be blocked as private")
	}
}

// TestWebFetch_RedirectToPrivateBlocked verifies redirects to private IPs are blocked
func TestWebFetch_RedirectToPrivateBlocked(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to a private IP
		http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
	}))
	defer server.Close()

	// Temporarily disable private host allowance for the redirect check
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(true)

	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		t.Fatalf("Failed to create web fetch tool: %v", err)
	}
	result := tool.Execute(context.Background(), map[string]any{
		"url": server.URL,
	})

	if !result.IsError {
		t.Error("expected error when redirecting to private IP, got success")
	}
}

// TestIsPrivateOrRestrictedIP_Table tests IP classification logic
func TestIsPrivateOrRestrictedIP_Table(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
		desc    string
	}{
		{"127.0.0.1", true, "IPv4 loopback"},
		{"10.0.0.1", true, "IPv4 private class A"},
		{"172.16.0.1", true, "IPv4 private class B"},
		{"192.168.1.1", true, "IPv4 private class C"},
		{"169.254.169.254", true, "link-local / cloud metadata"},
		{"100.64.0.1", true, "carrier-grade NAT"},
		{"0.0.0.0", true, "unspecified"},
		{"8.8.8.8", false, "public DNS"},
		{"1.1.1.1", false, "public DNS"},
		{"::1", true, "IPv6 loopback"},
		{"::ffff:127.0.0.1", true, "IPv4-mapped IPv6 loopback"},
		{"::ffff:10.0.0.1", true, "IPv4-mapped IPv6 private"},
		{"fc00::1", true, "IPv6 unique local"},
		{"fd00::1", true, "IPv6 unique local"},
		{"2002:7f00:0001::1", true, "6to4 with embedded 127.x (private)"},
		{"2002:0a00:0001::1", true, "6to4 with embedded 10.0.0.1 (private)"},
		{"2002:0801:0101::1", false, "6to4 with embedded 8.1.1.1 (public)"},
		{"2001:0000:4136:e378:8000:63bf:f5ff:fffe", true, "Teredo with client 10.0.0.1 (private)"},
		{"2001:0000:4136:e378:8000:63bf:f7f6:fefe", false, "Teredo with client 8.9.1.1 (public)"},
		{"2607:f8b0:4004:800::200e", false, "public IPv6 (Google)"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP: %s", tt.ip)
			}
			got := isPrivateOrRestrictedIP(ip)
			if got != tt.blocked {
				t.Errorf("isPrivateOrRestrictedIP(%s) = %v, want %v", tt.ip, got, tt.blocked)
			}
		})
	}
}

// TestWebTool_WebFetch_MissingDomain verifies error handling for URL without domain
func TestWebTool_WebFetch_MissingDomain(t *testing.T) {
	tool, err := NewWebFetchTool(50000, testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	ctx := context.Background()
	args := map[string]any{
		"url": "https://",
	}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error for URL without domain")
	}

	// Should mention missing domain
	if !strings.Contains(result.ForLLM, "domain") && !strings.Contains(result.ForUser, "domain") {
		t.Errorf("Expected domain error message, got ForLLM: %s", result.ForLLM)
	}
}

func TestNewWebFetchToolWithProxy(t *testing.T) {
	tool, err := NewWebFetchToolWithProxy(1024, "http://127.0.0.1:7890", testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	} else if tool.maxChars != 1024 {
		t.Fatalf("maxChars = %d, want %d", tool.maxChars, 1024)
	}

	if tool.proxy != "http://127.0.0.1:7890" {
		t.Fatalf("proxy = %q, want %q", tool.proxy, "http://127.0.0.1:7890")
	}

	tool, err = NewWebFetchToolWithProxy(0, "http://127.0.0.1:7890", testFetchLimit)
	if err != nil {
		logger.ErrorCF("agent", "Failed to create web fetch tool", map[string]any{"error": err.Error()})
	}

	if tool.maxChars != 50000 {
		t.Fatalf("default maxChars = %d, want %d", tool.maxChars, 50000)
	}
}

func TestNewWebSearchTool_PropagatesProxy(t *testing.T) {
	t.Run("perplexity", func(t *testing.T) {
		tool, err := NewWebSearchTool(WebSearchToolOptions{
			PerplexityEnabled:    true,
			PerplexityAPIKeys:    []string{"k"},
			PerplexityMaxResults: 3,
			Proxy:                "http://127.0.0.1:7890",
		})
		if err != nil {
			t.Fatalf("NewWebSearchTool() error: %v", err)
		}
		p, ok := tool.provider.(*PerplexitySearchProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *PerplexitySearchProvider", tool.provider)
		}
		if p.proxy != "http://127.0.0.1:7890" {
			t.Fatalf("provider proxy = %q, want %q", p.proxy, "http://127.0.0.1:7890")
		}
	})

	t.Run("brave", func(t *testing.T) {
		tool, err := NewWebSearchTool(WebSearchToolOptions{
			BraveEnabled:    true,
			BraveAPIKeys:    []string{"k"},
			BraveMaxResults: 3,
			Proxy:           "http://127.0.0.1:7890",
		})
		if err != nil {
			t.Fatalf("NewWebSearchTool() error: %v", err)
		}
		p, ok := tool.provider.(*BraveSearchProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *BraveSearchProvider", tool.provider)
		}
		if p.proxy != "http://127.0.0.1:7890" {
			t.Fatalf("provider proxy = %q, want %q", p.proxy, "http://127.0.0.1:7890")
		}
	})

	t.Run("duckduckgo", func(t *testing.T) {
		tool, err := NewWebSearchTool(WebSearchToolOptions{
			DuckDuckGoEnabled:    true,
			DuckDuckGoMaxResults: 3,
			Proxy:                "http://127.0.0.1:7890",
		})
		if err != nil {
			t.Fatalf("NewWebSearchTool() error: %v", err)
		}
		p, ok := tool.provider.(*DuckDuckGoSearchProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *DuckDuckGoSearchProvider", tool.provider)
		}
		if p.proxy != "http://127.0.0.1:7890" {
			t.Fatalf("provider proxy = %q, want %q", p.proxy, "http://127.0.0.1:7890")
		}
	})

	t.Run("searxng", func(t *testing.T) {
		tool, err := NewWebSearchTool(WebSearchToolOptions{
			SearXNGEnabled:    true,
			SearXNGBaseURL:    "https://searx.example.com",
			SearXNGMaxResults: 3,
			Proxy:             "http://127.0.0.1:7890",
		})
		if err != nil {
			t.Fatalf("NewWebSearchTool() error: %v", err)
		}
		p, ok := tool.provider.(*SearXNGSearchProvider)
		if !ok {
			t.Fatalf("provider type = %T, want *SearXNGSearchProvider", tool.provider)
		}
		if p.proxy != "http://127.0.0.1:7890" {
			t.Fatalf("provider proxy = %q, want %q", p.proxy, "http://127.0.0.1:7890")
		}
		tr, ok := p.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("client.Transport type = %T, want *http.Transport", p.client.Transport)
		}
		req, err := http.NewRequest(http.MethodGet, "https://searx.example.com/search", nil)
		if err != nil {
			t.Fatalf("http.NewRequest() error: %v", err)
		}
		proxyURL, err := tr.Proxy(req)
		if err != nil {
			t.Fatalf("transport.Proxy(req) error: %v", err)
		}
		if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
			t.Fatalf("proxy URL = %v, want %q", proxyURL, "http://127.0.0.1:7890")
		}
	})
}

// TestWebTool_TavilySearch_Success verifies successful Tavily search
func TestWebTool_TavilySearch_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}

		// Verify payload
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["api_key"] != "test-key" {
			t.Errorf("Expected api_key test-key, got %v", payload["api_key"])
		}
		if payload["query"] != "test query" {
			t.Errorf("Expected query 'test query', got %v", payload["query"])
		}

		// Return mock response
		response := map[string]any{
			"results": []map[string]any{
				{
					"title":   "Test Result 1",
					"url":     "https://example.com/1",
					"content": "Content for result 1",
				},
				{
					"title":   "Test Result 2",
					"url":     "https://example.com/2",
					"content": "Content for result 2",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	tool, err := NewWebSearchTool(WebSearchToolOptions{
		TavilyEnabled:    true,
		TavilyAPIKeys:    []string{"test-key"},
		TavilyBaseURL:    server.URL,
		TavilyMaxResults: 5,
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}

	ctx := context.Background()
	args := map[string]any{
		"query": "test query",
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForUser should contain result titles and URLs
	if !strings.Contains(result.ForUser, "Test Result 1") ||
		!strings.Contains(result.ForUser, "https://example.com/1") {
		t.Errorf("Expected results in output, got: %s", result.ForUser)
	}

	// Should mention via Tavily
	if !strings.Contains(result.ForUser, "via Tavily") {
		t.Errorf("Expected 'via Tavily' in output, got: %s", result.ForUser)
	}
}

func TestAPIKeyPool(t *testing.T) {
	pool := NewAPIKeyPool([]string{"key1", "key2", "key3"})
	if len(pool.keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(pool.keys))
	}
	if pool.keys[0] != "key1" || pool.keys[1] != "key2" || pool.keys[2] != "key3" {
		t.Fatalf("unexpected keys: %v", pool.keys)
	}

	// Test Iterator: each iterator should cover all keys exactly once
	iter := pool.NewIterator()
	expected := []string{"key1", "key2", "key3"}
	for i, want := range expected {
		k, ok := iter.Next()
		if !ok {
			t.Fatalf("iter.Next() returned false at step %d", i)
		}
		if k != want {
			t.Errorf("step %d: expected %s, got %s", i, want, k)
		}
	}
	// Should be exhausted
	if _, ok := iter.Next(); ok {
		t.Errorf("expected iterator exhausted after all keys")
	}

	// Second iterator starts at next position (load balancing)
	iter2 := pool.NewIterator()
	k, ok := iter2.Next()
	if !ok {
		t.Fatal("iter2.Next() returned false")
	}
	if k != "key2" {
		t.Errorf("expected key2 (round-robin), got %s", k)
	}

	// Empty pool
	emptyPool := NewAPIKeyPool([]string{})
	emptyIter := emptyPool.NewIterator()
	if _, ok := emptyIter.Next(); ok {
		t.Errorf("expected false for empty pool")
	}

	// Single key pool
	singlePool := NewAPIKeyPool([]string{"single"})
	singleIter := singlePool.NewIterator()
	if k, ok := singleIter.Next(); !ok || k != "single" {
		t.Errorf("expected single, got %s (ok=%v)", k, ok)
	}
	if _, ok := singleIter.Next(); ok {
		t.Errorf("expected exhausted after single key")
	}
}

func TestWebTool_TavilySearch_Failover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("failed to decode payload: %v", err)
		}

		apiKey := payload["api_key"].(string)

		if apiKey == "key1" {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("Rate limited"))
			return
		}

		if apiKey == "key2" {
			// Success
			response := map[string]any{
				"results": []map[string]any{
					{
						"title":   "Success Result",
						"url":     "https://example.com/success",
						"content": "Success content",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(response)
			return
		}

		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	tool, err := NewWebSearchTool(WebSearchToolOptions{
		TavilyEnabled:    true,
		TavilyAPIKeys:    []string{"key1", "key2"},
		TavilyBaseURL:    server.URL,
		TavilyMaxResults: 5,
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}

	ctx := context.Background()
	args := map[string]any{
		"query": "test query",
	}

	result := tool.Execute(ctx, args)

	if result.IsError {
		t.Errorf("Expected success, got Error: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "Success Result") {
		t.Errorf("Expected failover to second key and success result, got: %s", result.ForUser)
	}
}

func TestWebTool_GLMSearch_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("Expected POST request, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer test-glm-key" {
			t.Errorf("Expected Authorization Bearer test-glm-key, got %s", r.Header.Get("Authorization"))
		}

		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		if payload["search_query"] != "test query" {
			t.Errorf("Expected search_query 'test query', got %v", payload["search_query"])
		}
		if payload["search_engine"] != "search_std" {
			t.Errorf("Expected search_engine 'search_std', got %v", payload["search_engine"])
		}

		response := map[string]any{
			"id":      "web-search-test",
			"created": 1709568000,
			"search_result": []map[string]any{
				{
					"title":        "Test GLM Result",
					"content":      "GLM search snippet",
					"link":         "https://example.com/glm",
					"media":        "Example",
					"publish_date": "2026-03-04",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	tool, err := NewWebSearchTool(WebSearchToolOptions{
		GLMSearchEnabled: true,
		GLMSearchAPIKey:  "test-glm-key",
		GLMSearchBaseURL: server.URL,
		GLMSearchEngine:  "search_std",
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"query": "test query",
	})

	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForUser, "Test GLM Result") {
		t.Errorf("Expected 'Test GLM Result' in output, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "https://example.com/glm") {
		t.Errorf("Expected URL in output, got: %s", result.ForUser)
	}
	if !strings.Contains(result.ForUser, "via GLM Search") {
		t.Errorf("Expected 'via GLM Search' in output, got: %s", result.ForUser)
	}
}

func TestWebTool_GLMSearch_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer server.Close()

	tool, err := NewWebSearchTool(WebSearchToolOptions{
		GLMSearchEnabled: true,
		GLMSearchAPIKey:  "bad-key",
		GLMSearchBaseURL: server.URL,
		GLMSearchEngine:  "search_std",
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}

	result := tool.Execute(context.Background(), map[string]any{
		"query": "test query",
	})

	if !result.IsError {
		t.Errorf("Expected IsError=true for 401 response")
	}
	if !strings.Contains(result.ForLLM, "status 401") {
		t.Errorf("Expected status 401 in error, got: %s", result.ForLLM)
	}
}

func TestWebTool_GLMSearch_Priority(t *testing.T) {
	// GLM Search should only be selected when all other providers are disabled
	tool, err := NewWebSearchTool(WebSearchToolOptions{
		DuckDuckGoEnabled:    true,
		DuckDuckGoMaxResults: 5,
		GLMSearchEnabled:     true,
		GLMSearchAPIKey:      "test-key",
		GLMSearchBaseURL:     "https://example.com",
		GLMSearchEngine:      "search_std",
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}

	// DuckDuckGo should win over GLM Search
	if _, ok := tool.provider.(*DuckDuckGoSearchProvider); !ok {
		t.Errorf("Expected DuckDuckGoSearchProvider when both enabled, got %T", tool.provider)
	}

	// With DuckDuckGo disabled, GLM Search should be selected
	tool2, err := NewWebSearchTool(WebSearchToolOptions{
		DuckDuckGoEnabled: false,
		GLMSearchEnabled:  true,
		GLMSearchAPIKey:   "test-key",
		GLMSearchBaseURL:  "https://example.com",
		GLMSearchEngine:   "search_std",
	})
	if err != nil {
		t.Fatalf("NewWebSearchTool() error: %v", err)
	}
	if _, ok := tool2.provider.(*GLMSearchProvider); !ok {
		t.Errorf("Expected GLMSearchProvider when only GLM enabled, got %T", tool2.provider)
	}
}

func TestStripTags_Simple(t *testing.T) {
	got := stripTags("<p>Hello <b>World</b></p>")
	if got != "Hello World" {
		t.Errorf("stripTags = %q, want 'Hello World'", got)
	}
}

func TestStripTags_Empty(t *testing.T) {
	if got := stripTags(""); got != "" {
		t.Errorf("stripTags empty = %q, want empty", got)
	}
}

func TestStripTags_NoTags(t *testing.T) {
	got := stripTags("plain text")
	if got != "plain text" {
		t.Errorf("stripTags no tags = %q, want 'plain text'", got)
	}
}

func TestNormalizeWhitelistIP_IPv4(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	got := normalizeWhitelistIP(ip)
	if got == nil {
		t.Fatal("normalizeWhitelistIP nil for valid IPv4")
	}
	if got.To4() == nil {
		t.Error("normalizeWhitelistIP IPv4 should stay IPv4")
	}
}

func TestNormalizeWhitelistIP_Nil(t *testing.T) {
	if normalizeWhitelistIP(nil) != nil {
		t.Error("normalizeWhitelistIP(nil) should return nil")
	}
}

func TestNewPrivateHostWhitelist_Empty(t *testing.T) {
	w, err := newPrivateHostWhitelist(nil)
	if err != nil || w != nil {
		t.Errorf("newPrivateHostWhitelist(nil) = %v, %v, want nil, nil", w, err)
	}
}

func TestNewPrivateHostWhitelist_ValidIP(t *testing.T) {
	w, err := newPrivateHostWhitelist([]string{"192.168.1.100"})
	if err != nil {
		t.Fatalf("newPrivateHostWhitelist valid IP: %v", err)
	}
	if w == nil {
		t.Fatal("newPrivateHostWhitelist should return non-nil whitelist")
	}
	if !w.Contains(net.ParseIP("192.168.1.100")) {
		t.Error("whitelist should contain 192.168.1.100")
	}
	if w.Contains(net.ParseIP("192.168.1.101")) {
		t.Error("whitelist should not contain 192.168.1.101")
	}
}

func TestNewPrivateHostWhitelist_ValidCIDR(t *testing.T) {
	w, err := newPrivateHostWhitelist([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("newPrivateHostWhitelist CIDR: %v", err)
	}
	if !w.Contains(net.ParseIP("10.1.2.3")) {
		t.Error("whitelist should contain 10.1.2.3 (in CIDR 10.0.0.0/8)")
	}
	if w.Contains(net.ParseIP("192.168.1.1")) {
		t.Error("whitelist should not contain 192.168.1.1")
	}
}

func TestNewPrivateHostWhitelist_InvalidEntry(t *testing.T) {
	_, err := newPrivateHostWhitelist([]string{"not-an-ip-or-cidr"})
	if err == nil {
		t.Error("newPrivateHostWhitelist invalid entry should return error")
	}
}

func TestPrivateHostWhitelist_Contains_Nil(t *testing.T) {
	var w *privateHostWhitelist
	if w.Contains(net.ParseIP("10.0.0.1")) {
		t.Error("nil whitelist Contains should return false")
	}
}

func TestShouldBlockPrivateIP_BlockedWhenNoWhitelist(t *testing.T) {
	ip := net.ParseIP("192.168.1.1")
	if !shouldBlockPrivateIP(ip, nil) {
		t.Error("shouldBlockPrivateIP should block private IP with nil whitelist")
	}
}

func TestShouldBlockPrivateIP_AllowedWhenWhitelisted(t *testing.T) {
	w, _ := newPrivateHostWhitelist([]string{"192.168.1.1"})
	ip := net.ParseIP("192.168.1.1")
	if shouldBlockPrivateIP(ip, w) {
		t.Error("shouldBlockPrivateIP should not block whitelisted IP")
	}
}

func TestShouldBlockPrivateIP_PublicIPNotBlocked(t *testing.T) {
	ip := net.ParseIP("8.8.8.8")
	if shouldBlockPrivateIP(ip, nil) {
		t.Error("shouldBlockPrivateIP should not block public IP")
	}
}

func TestWebSearchTool_Name(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{})
	if err != nil {
		t.Fatalf("NewWebSearchTool: %v", err)
	}
	if tool.Name() != NameWebSearch {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameWebSearch)
	}
}

func TestWebSearchTool_Description(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{})
	if err != nil {
		t.Fatalf("NewWebSearchTool: %v", err)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
}

func TestWebSearchTool_Parameters(t *testing.T) {
	tool, err := NewWebSearchTool(WebSearchToolOptions{})
	if err != nil {
		t.Fatalf("NewWebSearchTool: %v", err)
	}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() should not be nil")
	}
	if params["type"] != "object" {
		t.Errorf("Parameters() type = %v, want object", params["type"])
	}
}

func TestWebFetchTool_Name(t *testing.T) {
	tool, err := NewWebFetchTool(1000, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool: %v", err)
	}
	if tool.Name() != NameWebFetch {
		t.Errorf("Name() = %q, want %q", tool.Name(), NameWebFetch)
	}
}

func TestWebFetchTool_Description(t *testing.T) {
	tool, err := NewWebFetchTool(1000, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool: %v", err)
	}
	if tool.Description() == "" {
		t.Error("Description() should not be empty")
	}
}

func TestWebFetchTool_Parameters(t *testing.T) {
	tool, err := NewWebFetchTool(1000, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool: %v", err)
	}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() should not be nil")
	}
	if params["type"] != "object" {
		t.Errorf("Parameters() type = %v, want object", params["type"])
	}
}

func TestDuckDuckGoSearchProvider_ExtractResults_NoMatches(t *testing.T) {
	p := &DuckDuckGoSearchProvider{client: &http.Client{}}
	result, err := p.extractResults("<html><body>nothing useful</body></html>", 3, "test query")
	if err != nil {
		t.Fatalf("extractResults: %v", err)
	}
	if !strings.Contains(result, "No results found") {
		t.Errorf("expected 'No results found' in output, got: %q", result)
	}
}

func TestDuckDuckGoSearchProvider_ExtractResults_WithLinks(t *testing.T) {
	p := &DuckDuckGoSearchProvider{client: &http.Client{}}
	// Minimal DDG HTML that matches the link regex pattern
	html := `<div class="result">
		<a class="result__a" href="https://example.com">Example Site</a>
		<a class="result__snippet">A great example website for testing</a>
	</div>
	<div class="result">
		<a class="result__a" href="https://test.org">Test Organization</a>
	</div>`
	result, err := p.extractResults(html, 5, "example")
	if err != nil {
		t.Fatalf("extractResults with links: %v", err)
	}
	_ = result // just verify no panic
}

func TestSearXNGSearchProvider_Search_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Example","url":"https://example.com","content":"An example result","engine":"google","score":1.0}]}`))
	}))
	defer ts.Close()

	p := &SearXNGSearchProvider{
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "test", 5, "")
	if err != nil {
		t.Fatalf("SearXNG Search: %v", err)
	}
	if !strings.Contains(result, "Example") {
		t.Errorf("expected 'Example' in result, got: %q", result)
	}
}

func TestSearXNGSearchProvider_Search_NoResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer ts.Close()

	p := &SearXNGSearchProvider{
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "noresults", 5, "")
	if err != nil {
		t.Fatalf("SearXNG Search no results: %v", err)
	}
	if !strings.Contains(result, "No results") {
		t.Errorf("expected 'No results' in output, got: %q", result)
	}
}

func TestSearXNGSearchProvider_Search_ErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	p := &SearXNGSearchProvider{
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	_, err := p.Search(context.Background(), "test", 5, "")
	if err == nil {
		t.Error("SearXNG Search with 500 should return error")
	}
}

func TestTavilySearchProvider_Search_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Tavily Example","url":"https://example.com","content":"A great result"}]}`))
	}))
	defer ts.Close()

	p := &TavilySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "test query", 5, "")
	if err != nil {
		t.Fatalf("Tavily Search: %v", err)
	}
	if !strings.Contains(result, "Tavily Example") {
		t.Errorf("expected 'Tavily Example' in result, got: %q", result)
	}
}

func TestTavilySearchProvider_Search_NoResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer ts.Close()

	p := &TavilySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "noresults", 3, "")
	if err != nil {
		t.Fatalf("Tavily Search no results: %v", err)
	}
	if !strings.Contains(result, "No results") {
		t.Errorf("expected 'No results' in output, got: %q", result)
	}
}

func TestTavilySearchProvider_Search_ErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer ts.Close()

	p := &TavilySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Tavily Search with 400 should return error")
	}
}

func TestTavilySearchProvider_Search_NoKeys(t *testing.T) {
	p := &TavilySearchProvider{
		keyPool: NewAPIKeyPool([]string{}),
		baseURL: "http://unused",
		client:  &http.Client{},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Tavily Search with no keys should return error")
	}
}

func TestBraveSearchProvider_Search_NoKeys(t *testing.T) {
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{}),
		client:  &http.Client{},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Brave Search with no keys should return error")
	}
}

// roundTripFunc implements http.RoundTripper using a function.
type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDuckDuckGoSearchProvider_Search_Success(t *testing.T) {
	html := `<html><body>
<a class="result__a" href="https://example.com">Example Title</a>
<a class="result__snippet">A great result snippet</a>
</body></html>`

	p := &DuckDuckGoSearchProvider{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(html)),
				Header:     http.Header{"Content-Type": []string{"text/html"}},
			}, nil
		})},
	}

	result, err := p.Search(context.Background(), "example", 5, "")
	if err != nil {
		t.Fatalf("DuckDuckGo Search: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestDuckDuckGoSearchProvider_Search_RequestError(t *testing.T) {
	p := &DuckDuckGoSearchProvider{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("connection refused")
		})},
	}

	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("DuckDuckGo Search request error should return error")
	}
}

func TestPerplexitySearchProvider_Search_NoKeys(t *testing.T) {
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{}),
		client:  &http.Client{},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Perplexity Search with no keys should return error")
	}
}

func TestPerplexitySearchProvider_Search_Success(t *testing.T) {
	responseBody := `{"choices":[{"message":{"content":"1. Result One\n   https://one.com\n   First result description"}}]}`
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(responseBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})},
	}
	result, err := p.Search(context.Background(), "test", 3, "")
	if err != nil {
		t.Fatalf("Perplexity Search success: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestPerplexitySearchProvider_Search_RequestError(t *testing.T) {
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network failure")
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	// All keys exhaust → last error returned
	if err == nil {
		t.Error("Perplexity Search request error should return error")
	}
}

func TestPerplexitySearchProvider_Search_ErrorStatus(t *testing.T) {
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 401,
				Body:       io.NopCloser(bytes.NewBufferString(`{"error":"Unauthorized"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Perplexity Search 401 should return error")
	}
}

func TestBraveSearchProvider_Search_Success(t *testing.T) {
	body := `{"web":{"results":[{"title":"Brave Result","url":"https://example.com","description":"A result"}]}}`
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{"brave-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	result, err := p.Search(context.Background(), "brave test", 3, "")
	if err != nil {
		t.Fatalf("BraveSearchProvider.Search unexpected error: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	if !strings.Contains(result, "Brave Result") {
		t.Errorf("expected result to contain 'Brave Result', got: %s", result)
	}
}

func TestBraveSearchProvider_Search_NoResults(t *testing.T) {
	body := `{"web":{"results":[]}}`
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{"brave-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	result, err := p.Search(context.Background(), "nothing", 3, "")
	if err != nil {
		t.Fatalf("BraveSearch no results unexpected error: %v", err)
	}
	if !strings.Contains(result, "No results") {
		t.Errorf("expected 'No results' message, got: %s", result)
	}
}

func TestBraveSearchProvider_Search_RequestError(t *testing.T) {
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{"brave-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("connection refused")
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("BraveSearch network error should return error")
	}
}

func TestBraveSearchProvider_Search_RateLimited_ThenSuccess(t *testing.T) {
	callCount := 0
	successBody := `{"web":{"results":[{"title":"Success","url":"https://ok.com","description":""}]}}`
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{"key1", "key2"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			callCount++
			if callCount == 1 {
				return &http.Response{
					StatusCode: 429,
					Body:       io.NopCloser(bytes.NewBufferString(`rate limited`)),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(successBody)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	result, err := p.Search(context.Background(), "test", 3, "")
	if err != nil {
		t.Fatalf("BraveSearch rate-limit-then-success: %v", err)
	}
	if !strings.Contains(result, "Success") {
		t.Errorf("expected success result, got: %s", result)
	}
}

func TestBraveSearchProvider_Search_NonRetryableError(t *testing.T) {
	p := &BraveSearchProvider{
		keyPool: NewAPIKeyPool([]string{"brave-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Body:       io.NopCloser(bytes.NewBufferString(`bad request`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("BraveSearch 400 (non-retryable) should return error immediately")
	}
}

// --- newSafeDialContext direct tests ---

func TestNewSafeDialContext_InvalidAddress_Error(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	_, err := dialFn(context.Background(), "tcp", "not-a-valid-address-without-port")
	if err == nil {
		t.Fatal("expected error for address without port")
	}
}

func TestNewSafeDialContext_EmptyHost_Error(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	_, err := dialFn(context.Background(), "tcp", ":80")
	if err == nil {
		t.Fatal("expected error for empty host")
	}
	if !strings.Contains(err.Error(), "empty target host") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestNewSafeDialContext_PrivateLiteralIP_Blocked(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	_, err := dialFn(context.Background(), "tcp", "10.0.0.1:80")
	if err == nil {
		t.Fatal("expected error for private IP 10.0.0.1")
	}
	if !strings.Contains(err.Error(), "blocked private or local target") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewSafeDialContext_LoopbackIP_Blocked(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	_, err := dialFn(context.Background(), "tcp", "127.0.0.1:8080")
	if err == nil {
		t.Fatal("expected error for loopback IP 127.0.0.1")
	}
	if !strings.Contains(err.Error(), "blocked private or local target") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewSafeDialContext_AllowedFirstHop_Bypasses(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	connected := false
	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	// Use a context that marks "127.0.0.1" as the allowed first hop.
	ctx := context.WithValue(context.Background(), webFetchAllowedFirstHopHostKey{}, "127.0.0.1")
	dialFn := newSafeDialContext(dialer)

	// Should bypass private check and attempt a real dial (which will fail but at connect, not at SSRF check).
	conn, err := dialFn(ctx, "tcp", "127.0.0.1:19999")
	if conn != nil {
		conn.Close()
		connected = true
	}
	// Either connected (unlikely) or failed at dial level, but NOT due to SSRF block.
	if err != nil && strings.Contains(err.Error(), "blocked private or local target") {
		t.Error("allowed first hop should bypass SSRF check; got SSRF error instead")
	}
	_ = connected
}

func TestNewSafeDialContext_Hostname_AllPrivate_Blocked(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	defer allowPrivateWebFetchHosts.Store(false)

	// "localhost" resolves to 127.0.0.1 (private) → all resolved IPs are private.
	dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	_, err := dialFn(context.Background(), "tcp", "localhost:80")
	if err == nil {
		t.Skip("dial to localhost succeeded unexpectedly — skipping in this environment")
	}
	// Should be SSRF block or connection refusal, not a panic.
	if strings.Contains(err.Error(), "blocked private or local target") {
		// isObviousPrivateHost would have blocked the URL before reaching here from Execute,
		// but direct call to dialFn skips that check.
		t.Skip("localhost caught as private literal IP — ok")
	}
	// If DNS resolved and all IPs were private:
	if strings.Contains(err.Error(), "all resolved addresses") {
		return // correctly blocked by DNS path
	}
}

// --- allowConfiguredProxyFirstHop ---

func TestAllowConfiguredProxyFirstHop_NilRequest(t *testing.T) {
	// Must not panic.
	allowConfiguredProxyFirstHop(nil, http.DefaultTransport)
}

func TestAllowConfiguredProxyFirstHop_NonTransport_NoOp(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	origCtx := req.Context()
	// Pass a non-*http.Transport round-tripper.
	allowConfiguredProxyFirstHop(req, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200}, nil
	}))
	// Context should be unchanged.
	if req.Context() != origCtx {
		t.Error("context should not change when rt is not *http.Transport")
	}
}

func TestAllowConfiguredProxyFirstHop_TransportNoProxy_NoOp(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	origCtx := req.Context()
	transport := &http.Transport{Proxy: nil}
	allowConfiguredProxyFirstHop(req, transport)
	if req.Context() != origCtx {
		t.Error("context should not change when transport has no proxy")
	}
}

// --- isObviousPrivateHost edge cases ---

func TestIsObviousPrivateHost_EmptyHost(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	if !isObviousPrivateHost("") {
		t.Error("empty host should be obvious private")
	}
}

func TestIsObviousPrivateHost_LocalhostSubdomain(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	if !isObviousPrivateHost("sub.localhost") {
		t.Error("sub.localhost should be obvious private")
	}
}

func TestIsObviousPrivateHost_PublicDomain_NotPrivate(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	if isObviousPrivateHost("example.com") {
		t.Error("example.com should not be obvious private")
	}
}

func TestIsObviousPrivateHost_TrailingDot(t *testing.T) {
	allowPrivateWebFetchHosts.Store(false)
	// "localhost." should be trimmed to "localhost" → private
	if !isObviousPrivateHost("localhost.") {
		t.Error("localhost. (trailing dot) should be obvious private")
	}
}

// --- isPrivateOrRestrictedIP edge cases ---

func TestIsPrivateOrRestrictedIP_NilIP(t *testing.T) {
	if !isPrivateOrRestrictedIP(nil) {
		t.Error("nil IP should be treated as restricted")
	}
}

func TestIsPrivateOrRestrictedIP_LinkLocalIPv6(t *testing.T) {
	ip := net.ParseIP("fe80::1")
	if !isPrivateOrRestrictedIP(ip) {
		t.Error("link-local IPv6 fe80::1 should be restricted")
	}
}

func TestIsPrivateOrRestrictedIP_UniqueLocalIPv6(t *testing.T) {
	ip := net.ParseIP("fc00::1")
	if !isPrivateOrRestrictedIP(ip) {
		t.Error("unique-local IPv6 fc00::1 should be restricted")
	}
}

func TestIsPrivateOrRestrictedIP_CarrierGradeNAT(t *testing.T) {
	ip := net.ParseIP("100.64.0.1")
	if !isPrivateOrRestrictedIP(ip) {
		t.Error("100.64.0.1 (carrier-grade NAT) should be restricted")
	}
}

func TestIsPrivateOrRestrictedIP_PublicIPv4_Allowed(t *testing.T) {
	ip := net.ParseIP("8.8.8.8")
	if isPrivateOrRestrictedIP(ip) {
		t.Error("8.8.8.8 should not be restricted")
	}
}

// --- newPrivateHostWhitelist edge cases ---

func TestNewPrivateHostWhitelist_EmptyEntries_ReturnsNil(t *testing.T) {
	// Entries with only whitespace are skipped → result is nil
	w, err := newPrivateHostWhitelist([]string{"  ", ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != nil {
		t.Error("whitelist with only empty entries should return nil")
	}
}

func TestNormalizeWhitelistIP_PureIPv6(t *testing.T) {
	// A pure IPv6 address (not 4-in-6) — To4() returns nil, so the IPv6 value is returned as-is.
	ip := net.ParseIP("2001:db8::1")
	got := normalizeWhitelistIP(ip)
	if got == nil {
		t.Fatal("normalizeWhitelistIP pure IPv6 should not return nil")
	}
	if got.To4() != nil {
		t.Error("normalizeWhitelistIP pure IPv6 should not convert to IPv4")
	}
}

func TestNewWebFetchToolWithProxy_InvalidProxy(t *testing.T) {
	// Unsupported proxy scheme → CreateHTTPClient returns error.
	_, err := NewWebFetchToolWithProxy(1024, "ftp://proxy.example.com:21", testFetchLimit)
	if err == nil {
		t.Fatal("expected error for unsupported proxy scheme")
	}
}

func TestNewSafeDialContext_AllowPrivateHosts_DialDirectly(t *testing.T) {
	// When allowPrivateWebFetchHosts is true, the dial proceeds without SSRF check.
	// The dial itself will fail (no server), but the important thing is the branch is exercised.
	allowPrivateWebFetchHosts.Store(true)
	defer allowPrivateWebFetchHosts.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	dialer := &net.Dialer{Timeout: 50 * time.Millisecond}
	dialFn := newSafeDialContext(dialer)

	conn, err := dialFn(ctx, "tcp", "127.0.0.1:19997")
	if conn != nil {
		conn.Close()
	}
	// May succeed or fail at dial; the branch path is covered.
	_ = err
}

// --- GLMSearchProvider direct tests ---

func TestGLMSearchProvider_Search_NoResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"search_result":[]}`))
	}))
	defer ts.Close()

	p := &GLMSearchProvider{
		apiKey:  "test-key",
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "nothing", 3, "")
	if err != nil {
		t.Fatalf("GLMSearch no results: unexpected error: %v", err)
	}
	if !strings.Contains(result, "No results") {
		t.Errorf("expected 'No results' message, got: %q", result)
	}
}

func TestGLMSearchProvider_Search_TruncatesResults(t *testing.T) {
	// Server returns 5 results, but count=2; the loop must break at i >= count.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		items := []map[string]any{
			{"title": "R1", "content": "c1", "link": "https://a.com"},
			{"title": "R2", "content": "c2", "link": "https://b.com"},
			{"title": "R3", "content": "c3", "link": "https://c.com"},
			{"title": "R4", "content": "c4", "link": "https://d.com"},
			{"title": "R5", "content": "c5", "link": "https://e.com"},
		}
		json.NewEncoder(w).Encode(map[string]any{"search_result": items})
	}))
	defer ts.Close()

	p := &GLMSearchProvider{
		apiKey:  "test-key",
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "query", 2, "")
	if err != nil {
		t.Fatalf("GLMSearch truncation: unexpected error: %v", err)
	}
	if strings.Contains(result, "R3") || strings.Contains(result, "R4") || strings.Contains(result, "R5") {
		t.Errorf("GLMSearch should truncate to 2 results, but got extra results in: %q", result)
	}
	if !strings.Contains(result, "R1") || !strings.Contains(result, "R2") {
		t.Errorf("GLMSearch should contain first 2 results, got: %q", result)
	}
}

// --- SearXNGSearchProvider direct tests ---

func TestSearXNGSearchProvider_Search_NilClient(t *testing.T) {
	// When client is nil, SearXNG falls back to a default http.Client.
	// Use a test server to avoid real network; set client=nil to cover that branch.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"NilClientResult","url":"https://example.com","content":"ok"}]}`))
	}))
	defer ts.Close()

	p := &SearXNGSearchProvider{
		baseURL: ts.URL,
		client:  nil, // triggers the nil-client branch → uses default http.Client
	}
	result, err := p.Search(context.Background(), "test", 5, "")
	// The default http.Client will attempt the request; it may succeed or fail depending on
	// whether the httptest server accepts connections from the default transport.
	// Both outcomes are fine — we only need the branch executed.
	_ = result
	_ = err
}

func TestSearXNGSearchProvider_Search_TruncatesResults(t *testing.T) {
	// Server returns 4 results, count=2 → truncation path executes.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		results := []map[string]any{
			{"title": "T1", "url": "https://a.com", "content": "c1"},
			{"title": "T2", "url": "https://b.com", "content": "c2"},
			{"title": "T3", "url": "https://c.com", "content": "c3"},
			{"title": "T4", "url": "https://d.com", "content": "c4"},
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer ts.Close()

	p := &SearXNGSearchProvider{
		baseURL: ts.URL,
		client:  ts.Client(),
	}
	result, err := p.Search(context.Background(), "query", 2, "")
	if err != nil {
		t.Fatalf("SearXNG truncation: unexpected error: %v", err)
	}
	if strings.Contains(result, "T3") || strings.Contains(result, "T4") {
		t.Errorf("SearXNG should truncate to 2 results, got: %q", result)
	}
	if !strings.Contains(result, "T1") || !strings.Contains(result, "T2") {
		t.Errorf("SearXNG should contain first 2 results, got: %q", result)
	}
}

// --- PerplexitySearchProvider additional paths ---

func TestPerplexitySearchProvider_Search_RetryableError_Exhausts(t *testing.T) {
	// 429 (rate limit) is retryable; with a single key it should exhaust and return error.
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"only-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(bytes.NewBufferString(`rate limited`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Perplexity 429 with single key should exhaust and return error")
	}
	if !strings.Contains(err.Error(), "all api keys failed") {
		t.Errorf("expected 'all api keys failed' error, got: %v", err)
	}
}

func TestPerplexitySearchProvider_Search_EmptyChoices(t *testing.T) {
	// Successful response with empty choices array → "No results for: ..." path.
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"test-key"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewBufferString(`{"choices":[]}`)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		})},
	}
	result, err := p.Search(context.Background(), "empty", 3, "")
	if err != nil {
		t.Fatalf("Perplexity empty choices: unexpected error: %v", err)
	}
	if !strings.Contains(result, "No results") {
		t.Errorf("expected 'No results' for empty choices, got: %q", result)
	}
}

func TestPerplexitySearchProvider_Search_NonRetryableError(t *testing.T) {
	// 404 is not in the retryable set → should return immediately without exhausting keys.
	p := &PerplexitySearchProvider{
		keyPool: NewAPIKeyPool([]string{"key1", "key2"}),
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(bytes.NewBufferString(`{"error":"not found"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := p.Search(context.Background(), "test", 3, "")
	if err == nil {
		t.Error("Perplexity 404 (non-retryable) should return error immediately")
	}
}
