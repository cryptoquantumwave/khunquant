package utils

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

func TestCreateHTTPClient_UsesEgressWhenInstalled(t *testing.T) {
	// A proxy from the environment must not survive either.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")

	// Not installed: the proxy settings are honoured as before.
	egress.ResetForTest()
	t.Cleanup(egress.ResetForTest)
	plain, err := CreateHTTPClient("socks5://127.0.0.1:1080", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("unlocked client lost its proxy")
	}

	if err := egress.Install(egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, proxy := range []string{"", "socks5://127.0.0.1:1080", "http://127.0.0.1:3128"} {
		client, err := CreateHTTPClient(proxy, 2*time.Second)
		if err != nil {
			t.Fatalf("CreateHTTPClient(%q): %v", proxy, err)
		}
		tr := client.Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Errorf("proxy %q: the guarded client kept a proxy", proxy)
		}
		if _, err := client.Get("http://203.0.113.1/"); !errors.Is(err, egress.ErrBlocked) { //nolint:noctx // blocked before any I/O
			t.Errorf("proxy %q: non-loopback GET: err = %v, want egress.ErrBlocked", proxy, err)
		}
	}

	// An invalid proxy URL is still a caller error.
	if _, err := CreateHTTPClient("ftp://x", time.Second); err == nil {
		t.Error("an unsupported proxy scheme must still be rejected")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	client, _ := CreateHTTPClient("socks5://127.0.0.1:1080", 2*time.Second)
	resp, err := client.Get(srv.URL) //nolint:noctx // loopback test server
	if err != nil {
		t.Fatalf("loopback GET through the guarded client: %v", err)
	}
	_ = resp.Body.Close()
}
