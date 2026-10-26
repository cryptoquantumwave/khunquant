package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func listenLoopback(t *testing.T, network, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln
}

func portOf(t *testing.T, ln net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestEgress_DialPolicy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v4 := listenLoopback(t, "tcp", "127.0.0.1:0")
	dial := DialContext(Policy{})

	t.Run("loopback IPv4 connects", func(t *testing.T) {
		c, err := dial(ctx, "tcp", net.JoinHostPort("127.0.0.1", portOf(t, v4)))
		if err != nil {
			t.Fatalf("dial 127.0.0.1: %v", err)
		}
		_ = c.Close()
	})

	t.Run("loopback IPv6 connects", func(t *testing.T) {
		v6 := listenLoopback(t, "tcp6", "[::1]:0")
		c, err := dial(ctx, "tcp", net.JoinHostPort("::1", portOf(t, v6)))
		if err != nil {
			t.Fatalf("dial [::1]: %v", err)
		}
		_ = c.Close()
	})

	t.Run("localhost connects over loopback only", func(t *testing.T) {
		c, err := dial(ctx, "tcp", net.JoinHostPort("localhost", portOf(t, v4)))
		if err != nil {
			t.Skipf("localhost did not resolve to a reachable loopback address here: %v", err)
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			t.Errorf("localhost connected to non-loopback %s", host)
		}
		_ = c.Close()
	})

	t.Run("blocked without a DNS lookup", func(t *testing.T) {
		// A resolver that fails the test when asked proves no lookup happened: the
		// block comes first. net.DefaultResolver is swapped for the call.
		old := net.DefaultResolver
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Error("DNS was consulted for a blocked host")
			return nil, errors.New("no dns in this test")
		}}
		defer func() { net.DefaultResolver = old }()

		for _, addr := range []string{"example.com:443", "203.0.113.1:80", "[2001:db8::1]:80", "2130706433:80", "localhost.example.com:80"} {
			_, err := dial(ctx, "tcp", addr)
			if !errors.Is(err, ErrBlocked) {
				t.Errorf("dial %s: err = %v, want ErrBlocked", addr, err)
			}
		}
	})

	t.Run("non-tcp networks are blocked", func(t *testing.T) {
		if _, err := dial(ctx, "unix", "/tmp/x.sock"); !errors.Is(err, ErrBlocked) {
			t.Errorf("unix dial: err = %v, want ErrBlocked", err)
		}
		if _, err := dial(ctx, "tcp", "no-port"); !errors.Is(err, ErrBlocked) {
			t.Errorf("malformed addr: err = %v, want ErrBlocked", err)
		}
	})

	t.Run("an AllowHosts name passes the policy", func(t *testing.T) {
		// The base dialer records what the policy lets through, so nothing here
		// touches the network or DNS.
		var dialed []string
		rec := func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return nil, errors.New("recorded")
		}
		allowDial := guard(Policy{AllowHosts: []string{"LLM.Allowed.Test.", "203.0.113.9"}}, rec)

		for _, addr := range []string{"203.0.113.9:80", "llm.allowed.test:443", "LLM.ALLOWED.TEST:443"} {
			if _, err := allowDial(ctx, "tcp", addr); errors.Is(err, ErrBlocked) {
				t.Errorf("allowed %s was blocked: %v", addr, err)
			}
		}
		if len(dialed) != 3 {
			t.Errorf("base dialer saw %v, want the 3 allowed addresses", dialed)
		}
		for _, addr := range []string{"other.test:443", "203.0.113.10:80", "allowed.test:443"} {
			if _, err := allowDial(ctx, "tcp", addr); !errors.Is(err, ErrBlocked) {
				t.Errorf("unlisted %s: err = %v, want ErrBlocked", addr, err)
			}
		}
		if len(dialed) != 3 {
			t.Errorf("a blocked address reached the base dialer: %v", dialed)
		}
	})
}

func TestEgress_TransportHasNoProxyOrBypassDialers(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	tr := Transport(Policy{})
	if tr.Proxy != nil {
		t.Error("guarded transport must not use a proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("guarded transport has no DialContext")
	}
	if tr.Dial != nil || tr.DialTLS != nil || tr.DialTLSContext != nil { //nolint:staticcheck // asserting the deprecated hooks are cleared
		t.Error("guarded transport must not carry other dial hooks")
	}
}

func TestEgress_InstallReplacesDefault(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)

	if Installed() {
		t.Fatal("Installed() before Install")
	}
	if err := Install(Policy{}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !Installed() {
		t.Fatal("Installed() is false after Install")
	}
	if err := Install(Policy{}); err == nil {
		t.Fatal("a second Install must error")
	}

	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737): never routable, and the guard
	// refuses it before any packet leaves.
	_, err := http.Get("http://203.0.113.1/") //nolint:noctx // blocked before any I/O
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("http.Get to a public address: err = %v, want ErrBlocked", err)
	}
	if err == nil || !strings.Contains(err.Error(), "egress: blocked") {
		t.Fatalf("error text %q does not say egress: blocked", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL) //nolint:noctx // loopback test server
	if err != nil {
		t.Fatalf("http.Get to loopback: %v", err)
	}
	_ = resp.Body.Close()
}

func TestResetForTest_Restores(t *testing.T) {
	ResetForTest()
	before := http.DefaultTransport
	if err := Install(Policy{}); err != nil {
		t.Fatal(err)
	}
	if http.DefaultTransport == before {
		t.Fatal("Install did not replace http.DefaultTransport")
	}
	ResetForTest()
	if http.DefaultTransport != before {
		t.Fatal("ResetForTest did not restore http.DefaultTransport")
	}
	if Installed() {
		t.Fatal("still installed after reset")
	}
}
