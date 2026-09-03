package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// web_fetch presents a browser User-Agent by default. When a WAF answers with a
// bot challenge, the response is to say honestly what this is rather than to
// escalate the disguise: some operators allow-list AI assistants that identify
// themselves, and a challenge is a request for identification.

func fetchToolFor(t *testing.T) *WebFetchTool {
	t.Helper()
	// The SSRF pre-flight refuses loopback, which is where httptest lives.
	withPrivateWebFetchHostsAllowed(t)
	tool, err := NewWebFetchTool(defaultMaxChars, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool() error = %v", err)
	}
	return tool
}

// TestWebFetch_RetriesChallengeWithHonestUA is the behaviour itself.
func TestWebFetch_RetriesChallengeWithHonestUA(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		seen = append(seen, ua)
		if len(seen) == 1 {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("blocked"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("the real content"))
	}))
	defer srv.Close()

	res := fetchToolFor(t).Execute(t.Context(), map[string]any{"url": srv.URL})
	if res == nil || res.IsError {
		t.Fatalf("fetch failed: %v", res)
	}
	if !strings.Contains(res.ForLLM, "the real content") {
		t.Errorf("did not return the retried content: %q", res.ForLLM)
	}

	if len(seen) != 2 {
		t.Fatalf("made %d requests, want 2 (challenge then honest retry): %v", len(seen), seen)
	}
	if seen[0] != userAgent {
		t.Errorf("first request UA = %q, want the browser default", seen[0])
	}
	if !strings.HasPrefix(seen[1], "khunquant/") {
		t.Errorf("retry UA = %q, want it to identify this agent", seen[1])
	}
	if !strings.Contains(seen[1], "AI assistant bot") {
		t.Errorf("retry UA = %q, want it to say what it is", seen[1])
	}
	if strings.Contains(seen[1], "Mozilla") {
		t.Errorf("retry UA = %q still claims to be a browser", seen[1])
	}
}

// A plain 403 with no challenge header must not trigger a second request:
// retrying every rejection would double traffic to sites that already said no.
func TestWebFetch_DoesNotRetryPlainForbidden(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("no"))
	}))
	defer srv.Close()

	_ = fetchToolFor(t).Execute(t.Context(), map[string]any{"url": srv.URL})
	if count != 1 {
		t.Errorf("made %d requests to a plain 403, want 1", count)
	}
}

// A successful first request must not retry either.
func TestWebFetch_DoesNotRetryOnSuccess(t *testing.T) {
	withPrivateWebFetchHostsAllowed(t)
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count++
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("fine"))
	}))
	defer srv.Close()

	res := fetchToolFor(t).Execute(t.Context(), map[string]any{"url": srv.URL})
	if res == nil || res.IsError {
		t.Fatalf("fetch failed: %v", res)
	}
	if count != 1 {
		t.Errorf("made %d requests to a 200, want 1", count)
	}
}

// The honest UA must be a truthful identifier, not another disguise.
func TestHonestUserAgent_IdentifiesTruthfully(t *testing.T) {
	ua := honestUserAgent()
	for _, want := range []string{"khunquant/", "AI assistant bot", "https://github.com/"} {
		if !strings.Contains(ua, want) {
			t.Errorf("honest UA %q is missing %q", ua, want)
		}
	}
	if strings.Contains(ua, "Mozilla") || strings.Contains(ua, "Chrome") {
		t.Errorf("honest UA %q impersonates a browser", ua)
	}
}
