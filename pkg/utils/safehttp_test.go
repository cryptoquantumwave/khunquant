package utils

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsPrivateOrRestrictedIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"public v4", "8.8.8.8", false},
		{"loopback v4", "127.0.0.1", true},
		{"rfc1918 10", "10.1.2.3", true},
		{"rfc1918 172", "172.16.0.1", true},
		{"rfc1918 192", "192.168.1.10", true},
		{"link-local v4 (metadata)", "169.254.169.254", true},
		{"cgnat", "100.64.0.1", true},
		{"public v6", "2606:4700:4700::1111", false},
		{"loopback v6", "::1", true},
		{"link-local v6", "fe80::1", true},
		{"ula v6", "fc00::1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("invalid test IP %q", tc.ip)
			}
			if got := IsPrivateOrRestrictedIP(ip); got != tc.blocked {
				t.Errorf("IsPrivateOrRestrictedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}

	if !IsPrivateOrRestrictedIP(nil) {
		t.Error("IsPrivateOrRestrictedIP(nil) = false, want true")
	}
}

func TestIsLinkLocalOrMetadataIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		{"metadata", "169.254.169.254", true},
		{"link-local v4", "169.254.1.1", true},
		{"link-local v6", "fe80::1", true},
		{"private but not link-local", "192.168.1.10", false},
		{"loopback", "127.0.0.1", false},
		{"public", "8.8.8.8", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if got := IsLinkLocalOrMetadataIP(ip); got != tc.want {
				t.Errorf("IsLinkLocalOrMetadataIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
	if IsLinkLocalOrMetadataIP(nil) {
		t.Error("IsLinkLocalOrMetadataIP(nil) = true, want false")
	}
}

func TestNewSafeHTTPClient_BlocksLoopback(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	client := NewSafeHTTPClient(2 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected an error dialing a loopback target, got nil")
	}
	if !errors.Is(err, ErrRestrictedTarget) {
		t.Errorf("error = %v, want it to wrap ErrRestrictedTarget", err)
	}
	if hit {
		t.Error("upstream received a request that should have been blocked before connecting")
	}
}

func TestNewSafeHTTPClient_AllowsPublic(t *testing.T) {
	client := NewSafeHTTPClient(5 * time.Second)
	// 198.51.100.1 (TEST-NET-2, RFC 5737) is a documentation-only address:
	// publicly-routable but never assigned, so the connection attempt itself
	// times out rather than the dial policy rejecting it up front. We only
	// assert that the failure is a network-level timeout, not our own
	// ErrRestrictedTarget guard.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://198.51.100.1:81/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected a network-level error (unreachable TEST-NET-2 address), got nil")
	}
	if errors.Is(err, ErrRestrictedTarget) {
		t.Errorf("a public IP was rejected as restricted: %v", err)
	}
}

func TestNewPolicyHTTPClient_AllowHostPermitsSpecificPrivateHost(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, _, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host: %v", err)
	}

	client := NewPolicyHTTPClient(2*time.Second, SafeDialPolicy{
		AllowHost: func(h string) bool { return h == host },
	})

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-listed host should be reachable: %v", err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
}

func TestNewPolicyHTTPClient_NeverAllowsLinkLocalOrMetadata(t *testing.T) {
	client := NewPolicyHTTPClient(2*time.Second, SafeDialPolicy{
		AllowHost: func(h string) bool { return true }, // wide-open allowlist
	})

	resp, err := client.Get("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the cloud metadata address to be rejected even with AllowHost permitting everything")
	}
	if !errors.Is(err, ErrRestrictedTarget) {
		t.Errorf("error = %v, want it to wrap ErrRestrictedTarget", err)
	}
}
