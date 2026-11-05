package fault

import (
	"io"
	"net/http"
	"testing"
	"time"
)

// A dropped GET on a reused connection reaches the caller (177-1). Go's client resends an
// idempotent request that fails on a kept-alive connection, so without a fresh connection
// per request the fault fired and was journaled, and the caller saw a success.
func TestDropConnection_ReachesTheCallerOnAReusedConnection(t *testing.T) {
	r := newHTTPRig(t, time.Second)
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	url := r.srv.BaseURL() + "/__sbx__/okx/www.okx.com/api/v5/public/time"
	get := func() error {
		resp, err := client.Get(url) //nolint:noctx // loopback test server
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}
	if err := get(); err != nil {
		t.Fatalf("the first GET: %v", err)
	}
	r.add(t, Rule{Match: Match{Venue: "okx", PathPrefix: "/api/v5/public/time"}, Effect: Effect{DropConnection: true}, When: When{Times: 1}})
	if err := get(); err == nil {
		t.Fatal("the dropped GET reached the caller as a success: the client resent it on a fresh connection")
	}
}
