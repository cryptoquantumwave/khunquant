package sandbox

import (
	"context"
	"net/http"
	"testing"
	"time"
)

type gateResponder struct {
	entered chan struct{}
	gate    chan struct{}
}

func (g gateResponder) Respond(string, string, string, *http.Request) (*Response, bool) {
	close(g.entered)
	<-g.gate
	return nil, false
}

// The server is busy while a request is being served (here held inside the responder)
// and idle before and after it.
func TestServer_IdleCoversARequest(t *testing.T) {
	srv := NewServer(NewStore())
	g := gateResponder{entered: make(chan struct{}), gate: make(chan struct{})}
	srv.SetResponder(g)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if !srv.Idle() {
		t.Fatal("a server with no request is idle")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(srv.BaseURL() + "/__sbx__/binance/api.binance.com/api/v3/ping")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the responder")
	}
	if srv.Idle() || srv.Idle() {
		t.Fatal("idle while a request is in progress")
	}
	close(g.gate)
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for !srv.Idle() {
		if time.Now().After(deadline) {
			t.Fatal("busy after the request finished")
		}
		time.Sleep(time.Millisecond)
	}
}
