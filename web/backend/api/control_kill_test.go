package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// The launcher forwards POST /api/control/kill to the gateway with the management
// token (which the browser never sees) and returns the gateway's answer as it is;
// a cross-site request is refused before anything is forwarded.
func TestControlKill_ProxiesToTheGatewayWithTheToken(t *testing.T) {
	var gotAuth, gotMethod, gotPath, gotOrigin string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath, gotOrigin = r.Header.Get("Authorization"), r.Method, r.URL.Path, r.Header.Get("Origin")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"killed":true,"changed":true,"text":"Stopped."}`))
	}))
	defer gw.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(gw.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	cfg := config.DefaultConfig()
	cfg.Gateway.Host, cfg.Gateway.Port, cfg.Gateway.ManagementToken = "127.0.0.1", port, "t0ken"
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(path)
	mux := http.NewServeMux()
	h.registerControlRoutes(mux)

	cross := httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, cross)
	if rec.Code != http.StatusForbidden || gotMethod != "" {
		t.Errorf("cross-site: %d, forwarded=%q", rec.Code, gotMethod)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/control/kill", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Origin", "http://localhost:18800")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"text":"Stopped."`) {
		t.Fatalf("same-origin: %d %s", rec.Code, rec.Body.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/api/control/kill" || gotAuth != "Bearer t0ken" {
		t.Errorf("forwarded %s %s with auth %q", gotMethod, gotPath, gotAuth)
	}
	if gotOrigin != "" {
		t.Errorf("the browser's Origin header was forwarded (%q): the gateway would refuse it", gotOrigin)
	}
}
