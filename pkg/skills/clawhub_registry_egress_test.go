package skills

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

// Under the sandbox lock the ClawHub registry's own transport dials through the egress
// guard (131-1): a locked instance cannot reach the public registry. The request goes
// through the registry's client directly: Search retries a failed request with a delay,
// which is not what is under test.
func TestClawHubRegistry_UnderTheSandboxLockIsBlocked(t *testing.T) {
	egress.ResetForTest()
	t.Cleanup(egress.ResetForTest)
	if err := egress.Install(egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	reg := NewClawHubRegistry(ClawHubConfig{BaseURL: "https://203.0.113.1"}) // TEST-NET-3: never routable
	// The deadline only bounds a regression (an unguarded transport waits on the connect);
	// with the guard the refusal is immediate and before any I/O.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg.baseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := reg.client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, egress.ErrBlocked) {
		t.Fatalf("GET %s under the lock: err = %v, want egress.ErrBlocked", reg.baseURL, err)
	}
}
