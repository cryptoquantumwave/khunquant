package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeSearchRange(t *testing.T) {
	valid := map[string]string{
		"d": "d", "w": "w", "m": "m", "y": "y",
		"D": "d", " W ": "w", // case and surrounding space are the model's, not an error
		"": "", // omitted means no filter
	}
	for in, want := range valid {
		got, err := normalizeSearchRange(in)
		if err != nil {
			t.Errorf("normalizeSearchRange(%q) error = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeSearchRange(%q) = %q, want %q", in, got, want)
		}
	}

	for _, bad := range []string{"day", "week", "1d", "z", "dd"} {
		if _, err := normalizeSearchRange(bad); err == nil {
			t.Errorf("normalizeSearchRange(%q) accepted an invalid range", bad)
		}
	}
}

// Each provider spells recency differently; the mapping is the whole feature,
// so it is asserted per provider rather than assumed.
func TestRangeMappers_TranslatePerProvider(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) string
		want map[string]string
	}{
		{"brave", mapBraveFreshness, map[string]string{"d": "pd", "w": "pw", "m": "pm", "y": "py"}},
		{"tavily", mapTavilyTimeRange, map[string]string{"d": "day", "w": "week", "m": "month", "y": "year"}},
		{"searxng", mapSearXNGTimeRange, map[string]string{"d": "day", "w": "week", "m": "month", "y": "year"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for in, want := range tc.want {
				if got := tc.fn(in); got != want {
					t.Errorf("%s(%q) = %q, want %q", tc.name, in, got, want)
				}
			}
			// An empty or unknown code must map to empty, which every provider
			// treats as "no filter" — so an unsupported combination degrades to
			// an unfiltered search rather than sending a bogus value.
			if got := tc.fn(""); got != "" {
				t.Errorf("%s(\"\") = %q, want empty", tc.name, got)
			}
			if got := tc.fn("nonsense"); got != "" {
				t.Errorf("%s(\"nonsense\") = %q, want empty", tc.name, got)
			}
		})
	}
}

// The range must actually reach the wire, not merely be accepted by the tool.
func TestBraveSearch_SendsFreshnessParameter(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"t","url":"u","description":"d"}]}}`))
	}))
	defer srv.Close()

	p := &BraveSearchProvider{keyPool: NewAPIKeyPool([]string{"k"}), client: srv.Client()}
	// Point the provider at the test server by overriding its transport target.
	p.client = &http.Client{Transport: rewriteHost{srv.URL, srv.Client().Transport}}

	if _, err := p.Search(context.Background(), "q", 3, "w"); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if !strings.Contains(gotQuery, "freshness=pw") {
		t.Errorf("query %q does not carry freshness=pw", gotQuery)
	}

	// And with no range, no freshness parameter at all.
	if _, err := p.Search(context.Background(), "q", 3, ""); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if strings.Contains(gotQuery, "freshness") {
		t.Errorf("query %q carries a freshness parameter for an empty range", gotQuery)
	}
}

// rewriteHost sends every request to a test server regardless of its URL, so a
// provider with a hard-coded endpoint can be exercised.
type rewriteHost struct {
	target string
	base   http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	target := strings.TrimPrefix(r.target, "http://")
	u.Scheme = "http"
	u.Host = target
	clone := req.Clone(req.Context())
	clone.URL = &u
	clone.Host = target
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// An invalid range must be refused rather than silently searched unfiltered: a
// model that asked for recent results and got all-time ones would treat stale
// hits as current.
func TestWebSearch_RejectsInvalidRange(t *testing.T) {
	tool := &WebSearchTool{provider: nil, maxResults: 5}

	res := tool.Execute(context.Background(), map[string]any{
		"query": "anything",
		"range": "last-week",
	})
	if res == nil || !res.IsError {
		t.Fatal("an invalid range was accepted")
	}
	if !strings.Contains(res.ForLLM, "range must be one of") {
		t.Errorf("error %q does not explain the accepted values", res.ForLLM)
	}
}

// The schema must advertise the parameter, or no model will ever send it.
func TestWebSearch_ParametersDeclareRange(t *testing.T) {
	tool := &WebSearchTool{}
	props, ok := tool.Parameters()["properties"].(map[string]any)
	if !ok {
		t.Fatal("Parameters() has no properties map")
	}
	spec, ok := props["range"].(map[string]any)
	if !ok {
		t.Fatal("Parameters() does not declare a range property")
	}
	enum, ok := spec["enum"].([]string)
	if !ok {
		t.Fatalf("range enum has type %T, want []string", spec["enum"])
	}
	if len(enum) != 4 {
		t.Errorf("range enum = %v, want the four codes", enum)
	}
}
