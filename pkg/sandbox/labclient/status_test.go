package labclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ready is its own answer, never folded into locked: a locked instance that is not ready
// yet is one to poll, and a status without the field is not ready.
func TestLabClient_StatusReadyIsSeparateFromLocked(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		locked, ready bool
	}{
		{"ready", `{"enabled":true,"locked":true,"ready":true}`, true, true},
		{"locked, not ready", `{"enabled":true,"locked":true,"ready":false}`, true, false},
		{"locked, no ready field", `{"enabled":true,"locked":true}`, true, false},
		{"unlocked", `{"enabled":true,"locked":false}`, false, false},
		{"disabled", `{"enabled":false,"locked":true,"ready":true}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tc.body) }))
			defer srv.Close()
			c, _ := New(srv.URL, tok)
			locked, ready, err := c.Status(context.Background())
			if err != nil || locked != tc.locked || ready != tc.ready {
				t.Errorf("Status(%s) = locked %v ready %v, %v; want locked %v ready %v", tc.body, locked, ready, err, tc.locked, tc.ready)
			}
		})
	}
}
