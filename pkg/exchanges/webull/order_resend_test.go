package webull

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// resendHarness serves the token and account endpoints and counts the requests
// that reach an order endpoint. onOrder decides what an order request gets.
type resendHarness struct {
	posts, gets atomic.Int32
	client      *Client
}

func newResendHarness(t *testing.T, onOrder func(h *resendHarness, w http.ResponseWriter, r *http.Request), opts ...Option) *resendHarness {
	t.Helper()
	old := retryDelayFn
	retryDelayFn = func(*http.Response, int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelayFn = old })

	h := &resendHarness{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi/auth/token/create":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TokenResponse{Token: "t", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderPlace, endpointOrderCancel:
			_, _ = io.ReadAll(r.Body)
			h.posts.Add(1)
			onOrder(h, w, r)
		case endpointOrderOpen:
			h.gets.Add(1)
			onOrder(h, w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{APIKey: *config.NewSecureString("key"), Secret: *config.NewSecureString("secret")},
		AccountID:       "ACC123",
	}
	all := append([]Option{WithBaseURL(server.URL), WithHTTPClient(server.Client())}, opts...)
	client, err := NewClient(cfg, all...)
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	return h
}

func (h *resendHarness) place() error {
	_, err := h.client.PlaceOrder(context.Background(), PlaceOrderRequest{AccountID: "ACC123", NewOrders: []NewOrder{{ClientOrderID: "kqabc"}}})
	return err
}

func (h *resendHarness) cancel() error {
	_, err := h.client.CancelOrder(context.Background(), CancelOrderRequest{AccountID: "ACC123", ClientOrderID: "kqabc"})
	return err
}

func fail5xx(h *resendHarness, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"message":"upstream unavailable","error_code":"UNAVAILABLE"}`))
}

// A 5xx on a request that places or cancels an order may mean Webull accepted it:
// exactly one request is sent and the error says the outcome is unknown.
func TestOrderMutations_5xxIsNeverResent(t *testing.T) {
	for _, call := range []struct {
		name string
		do   func(h *resendHarness) error
	}{
		{"place", (*resendHarness).place},
		{"cancel", (*resendHarness).cancel},
	} {
		t.Run(call.name, func(t *testing.T) {
			h := newResendHarness(t, fail5xx)
			err := call.do(h)
			if err == nil || !errors.Is(err, broker.ErrOutcomeUnknown) || !broker.Retryable(err) {
				t.Fatalf("err = %v, want one that reads as ErrOutcomeUnknown", err)
			}
			if n := h.posts.Load(); n != 1 {
				t.Fatalf("Webull saw %d order POSTs, want exactly 1", n)
			}
		})
	}
}

// The server reads the whole request and then drops the connection: the order may
// have been accepted. Exactly one POST.
func TestOrderMutations_ConnectionDroppedAfterTheRequestWasSentIsNeverResent(t *testing.T) {
	h := newResendHarness(t, func(h *resendHarness, w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	err := h.place()
	if err == nil || !errors.Is(err, broker.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
	if n := h.posts.Load(); n != 1 {
		t.Fatalf("Webull saw %d order POSTs, want exactly 1", n)
	}
}

// A 429 is a refusal: nothing was done, so the order request is repeated.
func TestOrderMutations_429IsStillRetried(t *testing.T) {
	h := newResendHarness(t, func(h *resendHarness, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.posts.Load() == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: "kqabc", OrderID: "1"})
	})
	if err := h.place(); err != nil {
		t.Fatal(err)
	}
	if n := h.posts.Load(); n != 2 {
		t.Fatalf("POSTs = %d, want 2 (the 429, then the order)", n)
	}
}

// failFirstDial fails the first round trip before any byte is written, as a
// refused connection does.
type failFirstDial struct {
	inner http.RoundTripper
	calls atomic.Int32
}

func (f *failFirstDial) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == endpointOrderPlace && f.calls.Add(1) == 1 {
		return nil, errors.New("dial tcp: connection refused")
	}
	return f.inner.RoundTrip(r)
}

// A transport error that provably happened before the request was written may be
// retried: nothing was sent.
func TestOrderMutations_ErrorBeforeTheRequestWasWrittenIsRetried(t *testing.T) {
	var rt *failFirstDial
	h := newResendHarness(t, func(h *resendHarness, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: "kqabc", OrderID: "1"})
	}, func(c *Client) error {
		rt = &failFirstDial{inner: c.httpClient.Transport}
		if rt.inner == nil {
			rt.inner = http.DefaultTransport
		}
		c.httpClient = &http.Client{Transport: rt}
		return nil
	})
	if err := h.place(); err != nil {
		t.Fatal(err)
	}
	if n := h.posts.Load(); n != 1 || rt.calls.Load() != 2 {
		t.Fatalf("server POSTs = %d, round trips = %d, want 1 and 2", n, rt.calls.Load())
	}
}

// Reads keep their retry policy: a GET that gets a 5xx is retried.
func TestReads_5xxIsStillRetried(t *testing.T) {
	h := newResendHarness(t, func(h *resendHarness, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.gets.Load() == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := h.client.FetchOpenOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := h.gets.Load(); n != 2 {
		t.Fatalf("GETs = %d, want 2", n)
	}
}

func TestMutatesOrders(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, endpointOrderPlace, true},
		{http.MethodPost, endpointOrderCancel, true},
		{http.MethodPost, endpointOrderReplace, true},
		{http.MethodPost, endpointTokenCreate, false},
		{http.MethodPost, endpointTokenCheck, false},
		{http.MethodGet, endpointOrderOpen, false},
		{http.MethodGet, endpointOrderPlace, false},
	} {
		if got := mutatesOrders(c.method, c.path); got != c.want {
			t.Errorf("mutatesOrders(%s %s) = %v", c.method, c.path, got)
		}
	}
}
