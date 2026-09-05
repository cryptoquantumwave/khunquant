package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestMaskAPIKeyValue(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "empty key",
			key:  "",
			want: "",
		},
		{
			name: "whitespace only",
			key:  "   ",
			want: "",
		},
		{
			name: "short key fully masked",
			key:  "abcd",
			want: "****",
		},
		{
			name: "length 8 boundary fully masked",
			key:  "12345678",
			want: "****",
		},
		{
			name: "length 9 boundary shows last 2",
			key:  "123456789",
			want: "123****89",
		},
		{
			name: "length 10 shows last 2",
			key:  "1234567890",
			want: "123****90",
		},
		{
			name: "length 12 boundary shows last 2",
			key:  "abcdefghijkl",
			want: "abc****kl",
		},
		{
			name: "length 13 boundary shows last 4",
			key:  "abcdefghijklm",
			want: "abc****jklm",
		},
		{
			name: "typical api key",
			key:  "sk-1234567890abcd",
			want: "sk-****abcd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := maskAPIKeyValue(tc.key)
			if got != tc.want {
				t.Fatalf("maskAPIKeyValue(%q) = %q, want %q", tc.key, got, tc.want)
			}

			if tc.key != "" {
				displayed := strings.Replace(got, "****", "", 1)
				if len(strings.TrimSpace(tc.key)) <= 8 {
					if displayed != "" {
						t.Fatalf("maskAPIKeyValue(%q) displayed part = %q, want empty", tc.key, displayed)
					}
				} else {
					if len(displayed)*10 > len(strings.TrimSpace(tc.key))*6 {
						t.Fatalf(
							"maskAPIKeyValue(%q) displayed length = %d, want at most 60%% of %d",
							tc.key,
							len(displayed),
							len(strings.TrimSpace(tc.key)),
						)
					}
				}
			}
		})
	}
}

// TestHandleDeleteCatalog_RequiresCSRF covers T6-3: handleDeleteCatalog is a
// state-changing (mutating) handler that previously never called
// allowStateChange, so a cross-site request could delete a saved catalog
// entry.
func TestHandleDeleteCatalog_RequiresCSRF(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("KHUNQUANT_HOME", filepath.Join(tmp, ".khunquant"))

	if err := SaveCatalog("openai", "https://api.openai.com/v1", "sk-x", []CatalogModel{{ID: "gpt-4o"}}); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
	store, err := loadCatalogs()
	if err != nil || len(store.Entries) != 1 {
		t.Fatalf("expected 1 saved catalog entry before the test, err=%v store=%+v", err, store)
	}
	var id string
	for k := range store.Entries {
		id = k
	}

	configPath := filepath.Join(tmp, "config.json")
	if err := config.SaveConfig(configPath, config.DefaultConfig()); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	h := NewHandler(configPath)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// No Sec-Fetch-Site/Origin/Referer: rejected, entry survives.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/models/catalog/"+url.PathEscape(id), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if store, err := loadCatalogs(); err != nil || len(store.Entries) != 1 {
		t.Fatalf("catalog entry should survive a CSRF-rejected delete: err=%v store=%+v", err, store)
	}

	// With Sec-Fetch-Site: same-origin, the delete succeeds.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/models/catalog/"+url.PathEscape(id), nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if store, err := loadCatalogs(); err != nil || len(store.Entries) != 0 {
		t.Fatalf("catalog entry should be removed after a CSRF-passing delete: err=%v store=%+v", err, store)
	}
}
