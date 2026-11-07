package simweb

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A web request in flight keeps the sandbox server busy, so the lab's "exchange_sim"
// quiescer (Server.Idle) covers SimWeb: a settle waits for it. The stage is held here the way
// a fault latency or a slow answer would hold it.
func TestSimWeb_ServerIdleCoversAWebRequest(t *testing.T) {
	s, _ := Open(Options{})
	if _, err := s.PutPage(PageRule{URLPattern: "slow.example/*", Body: "ok"}); err != nil {
		t.Fatal(err)
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	chain := sandbox.NewChain(sandbox.NewStore())
	chain.Set(sandbox.StageWeb, func(next sandbox.Responder) sandbox.Responder {
		inner := s.Middleware()(next)
		return sandbox.ResponderFunc(func(v, m, p string, r *http.Request) (*sandbox.Response, bool) {
			close(entered)
			<-gate
			return inner.Respond(v, m, p, r)
		})
	})
	srv := sandbox.NewServer(sandbox.NewStore())
	srv.SetResponder(chain)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if !srv.Idle() {
		t.Fatal("a server with no request is idle")
	}
	done := make(chan string, 1)
	go func() {
		resp, err := http.Get(srv.BaseURL() + "/__sbx__/web/slow.example/p")
		if err != nil {
			done <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the web stage")
	}
	if srv.Idle() {
		t.Fatal("idle while a web request is held in the stage")
	}
	close(gate)
	if got := <-done; got != "ok" {
		t.Fatalf("answer = %q", got)
	}
	// The handler's counter drops just after the answer is written: poll the state itself,
	// bounded, rather than sleeping.
	deadline := time.Now().Add(5 * time.Second)
	for !srv.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("busy after the request finished")
		}
		runtime.Gosched()
	}
}
