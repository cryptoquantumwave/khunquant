package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/web/backend/launcherconfig"
)

func TestGetLauncherConfigUsesRuntimeFallback(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)
	h.SetServerOptions(19999, true, false, []string{"192.168.1.0/24"})
	h.SetServerAccessOptions(false, []string{"10.0.0.0/8"})

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/system/launcher-config", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var got launcherConfigPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Port != 19999 || !got.Public {
		t.Fatalf("response = %+v, want port=19999 public=true", got)
	}
	if len(got.AllowedCIDRs) != 1 || got.AllowedCIDRs[0] != "192.168.1.0/24" {
		t.Fatalf("response allowed_cidrs = %v, want [192.168.1.0/24]", got.AllowedCIDRs)
	}
	if got.AllowLocalhostBypass {
		t.Fatalf("response allow_localhost_bypass = true, want false")
	}
	if len(got.TrustedProxyCIDRs) != 1 || got.TrustedProxyCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("response trusted_proxy_cidrs = %v, want [10.0.0.0/8]", got.TrustedProxyCIDRs)
	}
}

func TestPutLauncherConfigPersists(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	path := launcherconfig.PathForAppConfig(configPath)
	if err := launcherconfig.Save(
		path,
		launcherconfig.Config{
			Port:   18800,
			Public: false,
		},
	); err != nil {
		t.Fatalf("launcherconfig.Save() error = %v", err)
	}
	h := NewHandler(configPath)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/system/launcher-config",
		strings.NewReader(`{"port":18080,"public":true,"allowed_cidrs":["192.168.1.0/24"],"allow_localhost_bypass":false}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfg, err := launcherconfig.Load(path, launcherconfig.Default())
	if err != nil {
		t.Fatalf("launcherconfig.Load() error = %v", err)
	}
	if cfg.Port != 18080 || !cfg.Public {
		t.Fatalf("saved config = %+v, want port=18080 public=true", cfg)
	}
	if len(cfg.AllowedCIDRs) != 1 || cfg.AllowedCIDRs[0] != "192.168.1.0/24" {
		t.Fatalf("saved config allowed_cidrs = %v, want [192.168.1.0/24]", cfg.AllowedCIDRs)
	}
	if cfg.AllowLocalhostBypass {
		t.Fatalf("saved config allow_localhost_bypass = true, want false")
	}
}

func TestPutLauncherConfigUsesDirectAccessFields(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/system/launcher-config",
		strings.NewReader(`{"port":18080,"public":false,"allowed_cidrs":["192.168.1.0/24"],"allow_localhost_bypass":true}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	path := launcherconfig.PathForAppConfig(configPath)
	cfg, err := launcherconfig.Load(path, launcherconfig.Default())
	if err != nil {
		t.Fatalf("launcherconfig.Load() error = %v", err)
	}
	if !cfg.AllowLocalhostBypass {
		t.Fatal("saved config allow_localhost_bypass = false, want true")
	}
}

func TestPutLauncherConfigKeepsLocalhostBypassWhenOmitted(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	path := launcherconfig.PathForAppConfig(configPath)
	if err := launcherconfig.Save(
		path,
		launcherconfig.Config{
			Port:                 18800,
			Public:               false,
			AllowLocalhostBypass: true,
			TrustedProxyCIDRs:    []string{"10.0.0.0/8"},
		},
	); err != nil {
		t.Fatalf("launcherconfig.Save() error = %v", err)
	}
	h := NewHandler(configPath)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/system/launcher-config",
		strings.NewReader(`{"port":18080,"public":true,"allowed_cidrs":["192.168.1.0/24"]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	cfg, err := launcherconfig.Load(path, launcherconfig.Default())
	if err != nil {
		t.Fatalf("launcherconfig.Load() error = %v", err)
	}
	if !cfg.AllowLocalhostBypass {
		t.Fatal("saved config allow_localhost_bypass = false, want true")
	}
}

func TestPutLauncherConfigRejectsInvalidPort(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/system/launcher-config",
		strings.NewReader(`{"port":70000,"public":false}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestPutLauncherConfigRejectsInvalidCIDR(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	h := NewHandler(configPath)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/system/launcher-config",
		strings.NewReader(`{"port":18080,"public":false,"allowed_cidrs":["bad-cidr"]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// putTrustedProxyCase saves a launcher config trusting 10.0.0.0/8, sends body,
// and returns the status and the trusted proxies saved afterwards.
func putTrustedProxyCase(t *testing.T, body string) (int, []string) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	path := launcherconfig.PathForAppConfig(configPath)
	if err := launcherconfig.Save(path, launcherconfig.Config{
		Port: 18800, TrustedProxyCIDRs: []string{"10.0.0.0/8"},
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(configPath).RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/system/launcher-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	mux.ServeHTTP(rec, req)

	cfg, err := launcherconfig.Load(path, launcherconfig.Default())
	if err != nil {
		t.Fatal(err)
	}
	return rec.Code, cfg.TrustedProxyCIDRs
}

// M-15: a dashboard session cannot widen whose X-Forwarded-For is believed.
func TestPutLauncherConfigRefusesTrustedProxyChange(t *testing.T) {
	code, saved := putTrustedProxyCase(t,
		`{"port":18800,"allowed_cidrs":[],"trusted_proxy_cidrs":["0.0.0.0/1","128.0.0.0/1"]}`)
	if code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", code)
	}
	if len(saved) != 1 || saved[0] != "10.0.0.0/8" {
		t.Fatalf("trusted proxies changed to %v", saved)
	}
}

// The dashboard never sends the field; saving must not wipe the file's value,
// which the handler used to overwrite with nil.
func TestPutLauncherConfigKeepsTrustedProxiesWhenOmitted(t *testing.T) {
	code, saved := putTrustedProxyCase(t, `{"port":18801,"allowed_cidrs":[]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(saved) != 1 || saved[0] != "10.0.0.0/8" {
		t.Fatalf("trusted proxies = %v after a save that omitted them", saved)
	}
}

// Echoing the current value back (a client that round-trips the GET) is fine.
func TestPutLauncherConfigAcceptsUnchangedTrustedProxies(t *testing.T) {
	code, _ := putTrustedProxyCase(t,
		`{"port":18801,"allowed_cidrs":[],"trusted_proxy_cidrs":[" 10.0.0.0/8 "]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
}
