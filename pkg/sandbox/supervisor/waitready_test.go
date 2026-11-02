package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
)

// waitReadyOver runs waitReady against a /status that answers bodies in turn (the last
// repeats) and reports the error and how many times it was asked.
func waitReadyOver(t *testing.T, timeout time.Duration, bodies ...string) (int32, error) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := min(int(n.Add(1))-1, len(bodies)-1)
		_, _ = io.WriteString(w, bodies[i])
	}))
	defer srv.Close()
	c, err := labclient.New(srv.URL, "tok-waitready-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	err = waitReady(context.Background(), c, make(chan struct{}), timeout)
	return n.Load(), err
}

// waitReady keeps polling while the instance says ready:false, and while the field is
// missing (an answer that cannot say it is ready is not ready), and returns at the first
// ready:true.
func TestWaitReady_PollsThroughNotReady(t *testing.T) {
	const notReady = `{"enabled":true,"locked":true,"ready":false}`
	const noField = `{"enabled":true,"locked":true}`
	const ready = `{"enabled":true,"locked":true,"ready":true}`
	n, err := waitReadyOver(t, 10*time.Second, notReady, notReady, noField, ready)
	if err != nil || n != 4 {
		t.Fatalf("waitReady = %v after %d answers, want nil on the fourth", err, n)
	}
}

func TestWaitReady_NeverReadyTimesOut(t *testing.T) {
	for _, body := range []string{`{"enabled":true,"locked":true,"ready":false}`, `{"enabled":true,"locked":true}`} {
		n, err := waitReadyOver(t, 600*time.Millisecond, body)
		if err == nil || errors.Is(err, ErrNotLocked) || !strings.Contains(err.Error(), "not ready after") || n < 2 {
			t.Errorf("%s: waitReady = %v after %d answers, want a not-ready timeout after polling", body, err, n)
		}
	}
}

// An instance that is not locked is refused at its first answer, ready or not.
func TestWaitReady_UnlockedIsRefusedAtOnce(t *testing.T) {
	for _, body := range []string{`{"enabled":true,"locked":false}`, `{"enabled":true,"locked":false,"ready":true}`, `{"enabled":false,"locked":true,"ready":true}`} {
		n, err := waitReadyOver(t, 10*time.Second, body)
		if !errors.Is(err, ErrNotLocked) || n != 1 {
			t.Errorf("%s: waitReady = %v after %d answers, want ErrNotLocked at the first", body, err, n)
		}
	}
}
