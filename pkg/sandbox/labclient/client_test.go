package labclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

const tok = "tok-0123456789abcdef0123456789abcdef"

func TestLabClient_LoopbackOnlyAndErrors(t *testing.T) {
	for _, bad := range []string{"http://example.com", "http://10.0.0.5:80", "https://127.0.0.1:1", "127.0.0.1:80", "http://", "ftp://127.0.0.1"} {
		if _, err := New(bad, tok); err == nil {
			t.Errorf("New(%q) accepted a base URL that is not plain-http loopback", bad)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:1", "http://[::1]:9", "http://127.0.0.1:8080/"} {
		if _, err := New(ok, tok); err != nil {
			t.Errorf("New(%q): %v", ok, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/sandbox/lab/nope":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"no lab group \"nope\"","code":"unknown_group"}`)
		case "/api/sandbox/lab/echo": // a proxy page that echoes the credential must not leak it
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "denied for "+r.Header.Get("Authorization"))
		case "/redirect":
			http.Redirect(w, r, "http://example.com/steal", http.StatusFound)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, tok)
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Call(context.Background(), "nope", json.RawMessage(`{"op":"x"}`))
	var le *Error
	if !errors.As(err, &le) || le.Status != 404 || le.Code != "unknown_group" || !strings.Contains(le.Msg, "nope") {
		t.Fatalf("err = %#v, want the lab.Error decoded", err)
	}
	if _, err = c.Call(context.Background(), "echo", nil); err == nil || strings.Contains(err.Error(), tok) {
		t.Errorf("err = %v, want a failure without the token", err)
	}
	if _, err := c.do(context.Background(), http.MethodGet, "/redirect", nil); err == nil {
		t.Error("a redirect was followed or treated as success")
	}
	if _, err := c.Status(context.Background()); err == nil {
		t.Error("a 401 must be an error")
	}
}

func TestLabClient_Calls(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch r.URL.Path {
		case "/health":
		case "/api/sandbox/status":
			_, _ = io.WriteString(w, `{"enabled":true,"locked":true,"running":true}`)
		case "/api/sandbox/lab":
			_, _ = io.WriteString(w, `{"groups":[{"name":"g","description":"d","ops":[{"name":"o","description":"x","args":{"a":{"type":"string"}},"required":["a"]}]}]}`)
		case "/api/sandbox/lab/inspect":
			_, _ = io.WriteString(w, `{"matched":true,"count":2,"entries":[],"next_seq":7}`)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, tok)
	ctx := context.Background()

	if err := c.Health(ctx); err != nil {
		t.Error(err)
	}
	if locked, err := c.Status(ctx); err != nil || !locked || gotAuth != "Bearer "+tok {
		t.Errorf("Status = %v, %v (auth %q)", locked, err, gotAuth)
	}
	gs, err := c.Groups(ctx)
	if err != nil || len(gs) != 1 || gs[0].Ops[0].Name != "o" || string(gs[0].Ops[0].Args) != `{"a":{"type":"string"}}` || gs[0].Ops[0].Required[0] != "a" {
		t.Errorf("Groups = %+v, %v", gs, err)
	}
	res, err := c.WaitFor(ctx, labjournal.Match{Kind: "chat.out", Count: ">= 1"}, 3, 5*time.Second)
	if err != nil || !res.Matched || res.NextSeq != 7 {
		t.Errorf("WaitFor = %+v, %v", res, err)
	}
	for _, want := range []string{`"op":"wait_for"`, `"kind":"chat.out"`, `"after_seq":3`, `"timeout_wall":"5s"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("wait_for body %s lacks %s", gotBody, want)
		}
	}
}
