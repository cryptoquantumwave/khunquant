package webull

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// listServer is a fake Webull whose open-order and history listings return the
// rows the test sets, and which records the query of every listing request and
// the client_order_id of every place request.
type listServer struct {
	mu      sync.Mutex
	open    []ComboOrder
	history []ComboOrder
	openQ   []map[string]string
	histQ   []map[string]string
	placed  []string
}

func (s *listServer) adapter(t *testing.T) *webullAdapter {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		query := func() map[string]string {
			q := map[string]string{}
			for k := range r.URL.Query() {
				q[k] = r.URL.Query().Get(k)
			}
			return q
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/openapi/auth/token/create":
			_ = json.NewEncoder(w).Encode(TokenResponse{Token: "t", Expires: 9999999999999, Status: "NORMAL"})
		case endpointOrderOpen:
			s.openQ = append(s.openQ, query())
			_ = json.NewEncoder(w).Encode(s.open)
		case endpointOrderHistory:
			s.histQ = append(s.histQ, query())
			_ = json.NewEncoder(w).Encode(s.history)
		case endpointOrderPlace:
			var req PlaceOrderRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.NewOrders) != 1 {
				t.Errorf("unexpected place body: %v", err)
				return
			}
			s.placed = append(s.placed, req.NewOrders[0].ClientOrderID)
			_ = json.NewEncoder(w).Encode(PlaceOrderResponse{ClientOrderID: req.NewOrders[0].ClientOrderID, OrderID: "1"})
		}
	}))
	t.Cleanup(server.Close)
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{APIKey: *config.NewSecureString("key"), Secret: *config.NewSecureString("secret")},
		AccountID:       "ACC123",
	}
	client, err := NewClient(cfg, WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &webullAdapter{client: client, cfg: cfg}
}

func (s *listServer) placedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.placed...)
}

// combos builds n single-order records of other people's orders, so none of them
// matches the order being reconciled.
func combos(n int, status string) []ComboOrder {
	out := make([]ComboOrder, n)
	for i := range out {
		out[i] = ComboOrder{ComboType: "NORMAL", Orders: []OrderItem{{
			ClientOrderID: fmt.Sprintf("other%03d", i), Symbol: "TSLA", Side: "BUY", Status: status, OrderType: "MARKET",
		}}}
	}
	return out
}

// pairs builds n multi-order records (an OTO pair each): 2n order items in n combos.
func pairs(n int, status string) []ComboOrder {
	out := combos(n, status)
	for i := range out {
		second := out[i].Orders[0]
		second.ClientOrderID += "b"
		out[i].Orders = append(out[i].Orders, second)
	}
	return out
}

// Both listings ask for the documented maximum page; the default is 10, so a
// just-placed order behind ten newer ones would not be in the answer.
func TestListings_AskForAFullPage(t *testing.T) {
	s := &listServer{}
	a := s.adapter(t)
	if _, err := a.FetchOpenOrders(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FetchClosedOrders(context.Background(), "", nil, 0); err != nil {
		t.Fatal(err)
	}
	for name, qs := range map[string][]map[string]string{"open": s.openQ, "history": s.histQ} {
		if len(qs) != 1 || qs[0]["page_size"] != "100" || qs[0]["account_id"] != "ACC123" {
			t.Errorf("%s listing queries = %v, want one with page_size=100", name, qs)
		}
	}
}

func TestListings_FullPageIsTruncatedFromTheRawRecords(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		open, history []ComboOrder
		symbol        string
		wantOpenCut   bool
		wantClosedCut bool
	}{
		{"empty", nil, nil, "", false, false},
		{"one short of a page", combos(99, "SUBMITTED"), combos(99, "FILLED"), "", false, false},
		{"exactly a page", combos(100, "SUBMITTED"), combos(100, "FILLED"), "", true, true},
		// The spec does not say whether page_size counts combos or the orders inside
		// them: 50 combos of 2 orders are 100 order items, so they count as full.
		{"50 combos holding 100 order items", pairs(50, "SUBMITTED"), pairs(50, "FILLED"), "", true, true},
		{"49 combos holding 98 order items", pairs(49, "SUBMITTED"), pairs(49, "FILLED"), "", false, false},
		// The status and symbol filters run after the page was cut: 100 raw records
		// that all drop out are still a full page.
		{"a full page the filters empty out", combos(100, "SUBMITTED"), combos(100, "FAILED"), "AAPL/USD", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &listServer{open: tc.open, history: tc.history}
			a := s.adapter(t)
			_, openCut, err := a.FetchOpenOrdersPage(ctx, tc.symbol)
			if err != nil {
				t.Fatal(err)
			}
			_, closedCut, err := a.FetchClosedOrdersPage(ctx, tc.symbol, nil, 100)
			if err != nil {
				t.Fatal(err)
			}
			if openCut != tc.wantOpenCut || closedCut != tc.wantClosedCut {
				t.Errorf("truncated open=%v closed=%v, want %v %v", openCut, closedCut, tc.wantOpenCut, tc.wantClosedCut)
			}
		})
	}
}

// The reconcile of a Webull order, end to end through PlaceOrderIdempotent: a
// listing that comes back exactly full cannot show that the order is absent, so a
// retry places nothing and says it cannot establish whether the order exists.
func TestReconcile_FullPageNeverReplacesTheOrder(t *testing.T) {
	const key = "wf-1:place"
	cid := broker.ClientOrderID(key, maxClientOrderIDLen)
	first := time.Now().Add(-time.Hour) // far past the settle delay
	req := broker.OrderRequest{Market: broker.OrderMarketSpot, Symbol: "AAPL/USD", Type: "market", Side: "buy", Amount: 10}
	other := func(n int) []ComboOrder { return combos(n, "SUBMITTED") }
	mine := ComboOrder{ComboType: "NORMAL", Orders: []OrderItem{{
		ClientOrderID: cid, Symbol: "AAPL", Side: "BUY", Status: "SUBMITTED", OrderType: "MARKET", TotalQuantity: "10",
	}}}

	for _, tc := range []struct {
		name          string
		open, history []ComboOrder
		wantPlaced    int
		wantErr       error // nil: the retry succeeds
	}{
		{"a full open page without the order", other(100), nil, 0, broker.ErrReconcileUnavailable},
		{"a full history page without the order", nil, combos(100, "FAILED"), 0, broker.ErrReconcileUnavailable},
		{"a page of 100 order items in 50 combos", pairs(50, "SUBMITTED"), nil, 0, broker.ErrReconcileUnavailable},
		{"a page short of full without the order: absence is believed", other(99), combos(99, "FILLED"), 1, nil},
		{"an empty listing: absence is believed", nil, nil, 1, nil},
		{"the order is in a full page: found, not placed again", append(other(99), mine), nil, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &listServer{open: tc.open, history: tc.history}
			a := s.adapter(t)
			o, err := broker.PlaceOrderIdempotent(context.Background(), a, req, key, 2, first)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			case o.Id == nil || *o.Id != cid:
				t.Errorf("order id = %v, want %s", o.Id, cid)
			}
			if got := s.placedIDs(); len(got) != tc.wantPlaced {
				t.Errorf("place requests = %v, want %d", got, tc.wantPlaced)
			} else if tc.wantPlaced == 1 && got[0] != cid {
				t.Errorf("re-placed under %q, want the same client_order_id %q", got[0], cid)
			}
			// Both listings were asked for a full page.
			for _, q := range append(append([]map[string]string{}, s.openQ...), s.histQ...) {
				if q["page_size"] != "100" {
					t.Errorf("a listing was requested with query %v", q)
				}
			}
		})
	}
}
