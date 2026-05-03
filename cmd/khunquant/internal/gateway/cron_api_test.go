package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoopbackOnly_AllowsLoopback(t *testing.T) {
	h := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoopbackOnly_AllowsLocalInterfaceAddress(t *testing.T) {
	oldInterfaceAddrs := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.9"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() {
		interfaceAddrs = oldInterfaceAddrs
	})

	h := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "192.168.1.9:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestLoopbackOnly_DeniesRemoteAddress(t *testing.T) {
	oldInterfaceAddrs := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.9"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	t.Cleanup(func() {
		interfaceAddrs = oldInterfaceAddrs
	})

	h := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/cron/jobs", nil)
	req.RemoteAddr = "192.168.1.88:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}
