package settrade

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// fakeSettrade serves the quote and order endpoints and records the requests it saw.
type fakeSettrade struct {
	mu          sync.Mutex
	quoteBody   string
	quoteStatus int
	quoteCalls  int
	orders      []createOrderRequest
}

func (f *fakeSettrade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(r.URL.Path, "/quote/"):
		f.quoteCalls++
		w.WriteHeader(f.quoteStatus)
		_, _ = io.WriteString(w, f.quoteBody)
	case strings.HasSuffix(r.URL.Path, "/orders") && r.Method == http.MethodPost:
		var req createOrderRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.orders = append(f.orders, req)
		_, _ = io.WriteString(w, `{"data":{"orderNo":"1","symbol":"PTT","side":"Buy","priceType":"`+req.PriceType+`","status":"A"}}`)
	default:
		http.NotFound(w, r)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newFakeAdapter returns an adapter whose HTTP traffic is redirected to a local
// fake server, with a valid token preloaded so no login is attempted.
func newFakeAdapter(t *testing.T, f *fakeSettrade) *SettradeFullAdapter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.SettradeExchangeAccount{BrokerID: "BRK", AppCode: "APP", AccountNo: "ACC"}
	cfg.PIN.Set("0000")
	client := &SettradeClient{
		cfg: cfg,
		httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
			return http.DefaultTransport.RoundTrip(r)
		})},
		accessToken: "token",
		tokenExpiry: time.Now().Add(time.Hour),
	}
	return &SettradeFullAdapter{client: client, cfg: cfg}
}

func TestCreateOrder_OddLotMarketNilPriceUsesQuote(t *testing.T) {
	f := &fakeSettrade{quoteStatus: 200, quoteBody: `{"symbol":"PTT","last":34.5}`}
	a := newFakeAdapter(t, f)

	if _, err := a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 10, nil, nil); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if f.quoteCalls != 1 {
		t.Errorf("quote calls: want 1, got %d", f.quoteCalls)
	}
	if len(f.orders) != 1 {
		t.Fatalf("orders sent: want 1, got %d", len(f.orders))
	}
	got := f.orders[0]
	if got.PriceType != "Limit" || got.Price != 34.5 || got.Volume != 10 || got.Side != "Buy" || got.Symbol != "PTT" {
		t.Errorf("unexpected order request: %+v", got)
	}
}

func TestCreateOrder_OddLotMarketNilPriceRefusedWithoutUsableQuote(t *testing.T) {
	cases := map[string]*fakeSettrade{
		"fetch fails":   {quoteStatus: 500, quoteBody: `{"code":"X","message":"boom"}`},
		"last is zero":  {quoteStatus: 200, quoteBody: `{"symbol":"PTT","last":0}`},
		"last missing":  {quoteStatus: 200, quoteBody: `{"symbol":"PTT"}`},
		"last negative": {quoteStatus: 200, quoteBody: `{"symbol":"PTT","last":-1}`},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			a := newFakeAdapter(t, f)
			_, err := a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 10, nil, nil)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if len(f.orders) != 0 {
				t.Errorf("no order request should be sent, got %+v", f.orders)
			}
		})
	}
}

func TestCreateOrder_OddLotMarketExplicitPriceFetchesNothing(t *testing.T) {
	f := &fakeSettrade{quoteStatus: 200, quoteBody: `{"symbol":"PTT","last":99}`}
	a := newFakeAdapter(t, f)

	price := 30.0
	if _, err := a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 10, &price, nil); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if f.quoteCalls != 0 {
		t.Errorf("quote calls: want 0, got %d", f.quoteCalls)
	}
	if len(f.orders) != 1 || f.orders[0].PriceType != "Limit" || f.orders[0].Price != 30 {
		t.Errorf("unexpected orders: %+v", f.orders)
	}
}

func TestCreateOrder_BoardLotMarketStillATO(t *testing.T) {
	f := &fakeSettrade{quoteStatus: 200, quoteBody: `{"symbol":"PTT","last":34.5}`}
	a := newFakeAdapter(t, f)

	if _, err := a.CreateOrder(context.Background(), "PTT/THB", "market", "buy", 100, nil, nil); err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if f.quoteCalls != 0 {
		t.Errorf("quote calls: want 0, got %d", f.quoteCalls)
	}
	if len(f.orders) != 1 || f.orders[0].PriceType != "ATO" || f.orders[0].Price != 0 {
		t.Errorf("unexpected orders: %+v", f.orders)
	}
}
