package tools

import (
	"context"
	"strings"
	"testing"
)

// Before this guard existed, a provider with no configured credential ran its
// retry loop zero times and fell through to the loop's exit error, reporting
//
//	all api keys failed, last error: %!w(<nil>)
//
// That names a failure that never happened — no request was ever attempted —
// and the mangled %w verb betrays that lastErr was nil. The operator's actual
// problem is that they never configured a key, which the message did not say.
func TestSearchProviders_ReportMissingCredentialPlainly(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		search  func() (string, error)
		wantErr string
	}{
		{
			name:    "brave with nil pool",
			search:  func() (string, error) { return (&BraveSearchProvider{}).Search(ctx, "q", 3) },
			wantErr: "no API key provided",
		},
		{
			name: "brave with empty pool",
			search: func() (string, error) {
				return (&BraveSearchProvider{keyPool: NewAPIKeyPool(nil)}).Search(ctx, "q", 3)
			},
			wantErr: "no API key provided",
		},
		{
			name:    "tavily with nil pool",
			search:  func() (string, error) { return (&TavilySearchProvider{}).Search(ctx, "q", 3) },
			wantErr: "no API key provided",
		},
		{
			name:    "perplexity with nil pool",
			search:  func() (string, error) { return (&PerplexitySearchProvider{}).Search(ctx, "q", 3) },
			wantErr: "no API key provided",
		},
		{
			name:    "searxng with no base URL",
			search:  func() (string, error) { return (&SearXNGSearchProvider{}).Search(ctx, "q", 3) },
			wantErr: "no SearXNG URL provided",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A nil keyPool previously dereferenced inside NewIterator, so this
			// also pins that the guard runs before any use of the pool.
			out, err := tc.search()
			if err == nil {
				t.Fatalf("expected an error, got output %q", out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
			if strings.Contains(err.Error(), "all api keys failed") {
				t.Errorf("error still blames key failure when none was attempted: %q", err.Error())
			}
			if strings.Contains(err.Error(), "%!w") {
				t.Errorf("error contains a mangled format verb: %q", err.Error())
			}
		})
	}
}
