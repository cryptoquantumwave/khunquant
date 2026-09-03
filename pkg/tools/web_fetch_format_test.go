package tools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const structuredHTML = `<html><body>
<h1>Title</h1>
<p>Intro paragraph.</p>
<ul><li>first</li><li>second</li></ul>
<a href="https://example.com/doc">the link</a>
</body></html>`

func fetchToolServing(t *testing.T, body string) (*WebFetchTool, string) {
	t.Helper()
	withPrivateWebFetchHostsAllowed(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	tool, err := NewWebFetchTool(defaultMaxChars, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool() error = %v", err)
	}
	return tool, srv.URL
}

// Plaintext remains the default, so nothing changes for callers that do not ask
// for markdown.
func TestWebFetch_DefaultsToPlaintext(t *testing.T) {
	tool, url := fetchToolServing(t, structuredHTML)

	res := tool.Execute(t.Context(), map[string]any{"url": url})
	if res == nil || res.IsError {
		t.Fatalf("fetch failed: %v", res)
	}
	if !strings.Contains(res.ForLLM, `"extractor": "text"`) {
		t.Errorf("extractor is not text by default: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "# Title") {
		t.Error("plaintext output contains markdown headings")
	}
}

// The per-call argument selects markdown, and the structure a model needs
// survives: headings marked as headings, links carrying their target.
func TestWebFetch_MarkdownPreservesStructure(t *testing.T) {
	tool, url := fetchToolServing(t, structuredHTML)

	res := tool.Execute(t.Context(), map[string]any{"url": url, "format": "markdown"})
	if res == nil || res.IsError {
		t.Fatalf("fetch failed: %v", res)
	}
	if !strings.Contains(res.ForLLM, `"extractor": "markdown"`) {
		t.Fatalf("extractor is not markdown: %s", res.ForLLM)
	}
	for _, want := range []string{"# Title", "https://example.com/doc"} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("markdown output is missing %q: %s", want, res.ForLLM)
		}
	}
}

// The configured default applies when no per-call argument is given, and the
// per-call argument overrides it in both directions.
func TestWebFetch_FormatResolution(t *testing.T) {
	tool, err := NewWebFetchTool(defaultMaxChars, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool() error = %v", err)
	}

	if got := tool.resolveFetchFormat(nil); got != "plaintext" {
		t.Errorf("unset default = %q, want plaintext", got)
	}

	tool.SetFormat("markdown")
	if got := tool.resolveFetchFormat(nil); got != "markdown" {
		t.Errorf("configured default = %q, want markdown", got)
	}
	if got := tool.resolveFetchFormat(map[string]any{"format": "plaintext"}); got != "plaintext" {
		t.Errorf("per-call override = %q, want plaintext", got)
	}

	// An unrecognised value falls back rather than erroring: returning readable
	// text beats refusing over a formatting preference.
	if got := tool.resolveFetchFormat(map[string]any{"format": "yaml"}); got != "markdown" {
		t.Errorf("unknown format = %q, want the configured default", got)
	}
	tool.SetFormat("")
	if got := tool.resolveFetchFormat(map[string]any{"format": "yaml"}); got != "plaintext" {
		t.Errorf("unknown format with no default = %q, want plaintext", got)
	}
}

// The schema must advertise the parameter or no model will send it.
func TestWebFetch_ParametersDeclareFormat(t *testing.T) {
	tool, err := NewWebFetchTool(defaultMaxChars, 1<<20)
	if err != nil {
		t.Fatalf("NewWebFetchTool() error = %v", err)
	}
	props, ok := tool.Parameters()["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters() has no properties map")
	}
	spec, ok := props["format"].(map[string]any)
	if !ok {
		t.Fatal("Parameters() does not declare a format property")
	}
	enum, ok := spec["enum"].([]string)
	if !ok || len(enum) != 2 {
		t.Errorf("format enum = %v, want plaintext and markdown", spec["enum"])
	}
}

// A page that will not convert must still be readable, not an error.
func TestWebFetch_MarkdownFallsBackOnUnconvertibleInput(t *testing.T) {
	tool, url := fetchToolServing(t, "<html><body>plain words</body></html>")

	res := tool.Execute(t.Context(), map[string]any{"url": url, "format": "markdown"})
	if res == nil || res.IsError {
		t.Fatalf("fetch failed: %v", res)
	}
	if !strings.Contains(res.ForLLM, "plain words") {
		t.Errorf("content was lost: %s", res.ForLLM)
	}
}
