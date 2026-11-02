package sandbox

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func startWrapServer(t *testing.T) *Server {
	t.Helper()
	srv := NewServer(NewStore())
	srv.SetResponder(ResponderFunc(func(string, string, string, *http.Request) (*Response, bool) {
		return &Response{Status: 200, BodyText: "routed"}, true
	}))
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv
}

func getBody(t *testing.T, srv *Server) string {
	t.Helper()
	resp, err := http.Get(srv.BaseURL() + "/__sbx__/okx/www.okx.com/api/v5/public/time")
	if err != nil {
		t.Error(err) // not Fatal: this also runs on a goroutine
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// With no wrap a request is served by the router alone; a wrap set after Start is used
// from the next request on, and nil removes it again.
func TestSetHandlerWrap_BeforeOrAfterStart(t *testing.T) {
	srv := startWrapServer(t)
	if srv.HasHandlerWrap() || getBody(t, srv) != "routed" {
		t.Fatal("a server with no wrap must serve the router's answer")
	}
	srv.SetHandlerWrap(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("wrapped"))
		})
	})
	if !srv.HasHandlerWrap() || getBody(t, srv) != "wrapped" {
		t.Fatal("a wrap set after Start did not take effect")
	}
	srv.SetHandlerWrap(nil)
	if srv.HasHandlerWrap() || getBody(t, srv) != "routed" {
		t.Fatal("clearing the wrap did not restore the router")
	}
}

// The in-flight count is outside the wrap, so a request the wrap holds keeps the server busy.
func TestSetHandlerWrap_IdleCoversAHeldRequest(t *testing.T) {
	srv := startWrapServer(t)
	entered, release := make(chan struct{}), make(chan struct{})
	srv.SetHandlerWrap(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
			next.ServeHTTP(w, r)
		})
	})
	done := make(chan string, 1)
	go func() { done <- getBody(t, srv) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the wrap")
	}
	if srv.Idle() {
		t.Fatal("idle while the wrap holds a request")
	}
	close(release)
	if got := <-done; got != "routed" {
		t.Fatalf("held request answered %q after release", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !srv.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("busy after the held request finished")
		}
		time.Sleep(time.Millisecond)
	}
}
