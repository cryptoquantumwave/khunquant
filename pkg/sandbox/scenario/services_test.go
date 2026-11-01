package scenario

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

// call_tool answers "no generation of services is running" until the instance has
// published its first generation; a call made in that window waits for it rather than
// failing the run, and any other answer is returned at once.
func TestCall_WaitsForTheFirstGenerationOfServices(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/sandbox/lab/call_tool":
			if n.Add(1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"no generation of services is running (a reload is in progress?)","code":"unavailable"}`))
				return
			}
			_, _ = w.Write([]byte(`{"for_llm":"ok","is_error":false}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad","code":"bad_args"}`))
		}
	}))
	defer srv.Close()
	c, err := labclient.New(srv.URL, "Zq9-token-Zq9")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := call(context.Background(), c, "call_tool", "", map[string]any{"name": "update_profile"}); err != nil || n.Load() != 3 {
		t.Fatalf("call_tool: %v after %d tries, want success on the third", err, n.Load())
	}
	if _, err := call(context.Background(), c, "clock", "get", nil); err == nil || noServices(err) {
		t.Fatalf("another error must come back at once, got %v", err)
	}
	if noServices(nil) {
		t.Error("no error is not 'no services'")
	}
}
