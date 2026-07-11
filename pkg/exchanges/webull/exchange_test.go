package webull

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestExchange builds a WebullExchange wrapping an adapter pointed at
// server, matching the newOptionTestAdapter / newTestClient patterns used
// elsewhere in this package's tests.
func newTestExchange(t *testing.T, server *httptest.Server) *WebullExchange {
	t.Helper()
	return &WebullExchange{adapter: newOptionTestAdapter(t, server)}
}

func TestWebullExchange_Name(t *testing.T) {
	ex := &WebullExchange{adapter: &webullAdapter{}}
	if ex.Name() != Name {
		t.Errorf("Name() = %q, want %q", ex.Name(), Name)
	}
}

func TestWebullExchange_SupportedWalletTypes(t *testing.T) {
	ex := &WebullExchange{adapter: &webullAdapter{}}
	types := ex.SupportedWalletTypes()
	if len(types) == 0 {
		t.Error("expected non-empty supported wallet types")
	}
}

func TestWebullExchange_SupportedQuotes(t *testing.T) {
	ex := &WebullExchange{adapter: &webullAdapter{}}
	quotes := ex.SupportedQuotes()
	if len(quotes) != 1 || quotes[0] != "USD" {
		t.Errorf("expected [\"USD\"], got %v", quotes)
	}
}

func TestWebullExchange_GetBalances_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointBalance:
			json.NewEncoder(w).Encode(BalanceResponse{
				AccountCurrencyAssets: []CurrencyAsset{
					{Currency: "USD", CashBalance: "1000.50"},
				},
			})
		}
	}))
	defer server.Close()

	ex := newTestExchange(t, server)
	balances, err := ex.GetBalances(context.Background())
	if err != nil {
		t.Fatalf("GetBalances failed: %v", err)
	}
	if len(balances) != 1 || balances[0].Asset != "USD" || balances[0].Free != 1000.50 {
		t.Errorf("unexpected balances: %+v", balances)
	}
}

func TestWebullExchange_GetBalances_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointBalance:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"msg": "internal error"})
		}
	}))
	defer server.Close()

	ex := newTestExchange(t, server)
	fastRetries(t)
	if _, err := ex.GetBalances(context.Background()); err == nil {
		t.Fatal("expected error propagated from adapter.GetBalances")
	}
}

func TestWebullExchange_GetWalletBalances_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch r.URL.Path {
		case endpointTokenCreate:
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointBalance:
			json.NewEncoder(w).Encode(BalanceResponse{
				AccountCurrencyAssets: []CurrencyAsset{
					{Currency: "USD", CashBalance: "500.00", BuyingPower: "1000.00"},
				},
			})
		}
	}))
	defer server.Close()

	ex := newTestExchange(t, server)
	balances, err := ex.GetWalletBalances(context.Background(), "cash")
	if err != nil {
		t.Fatalf("GetWalletBalances failed: %v", err)
	}
	if len(balances) != 1 || balances[0].WalletType != "cash" || balances[0].Extra["buying_power"] != "1000.00" {
		t.Errorf("unexpected wallet balances: %+v", balances)
	}
}

func TestWebullExchange_GetWalletBalances_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case endpointTokenCreate:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		case endpointBalance:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"msg": "internal error"})
		}
	}))
	defer server.Close()

	ex := newTestExchange(t, server)
	fastRetries(t)
	if _, err := ex.GetWalletBalances(context.Background(), "cash"); err == nil {
		t.Fatal("expected error propagated from adapter.GetWalletBalances")
	}
}

func TestWebullExchange_FetchPrice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == endpointTokenCreate {
			json.NewEncoder(w).Encode(TokenResponse{Token: "test-token", Expires: 9999999999999, Status: "NORMAL"})
		}
	}))
	defer server.Close()

	ex := newTestExchange(t, server)
	// Unsupported quote currency is rejected without any further HTTP calls,
	// exercising the delegation + error-propagation path end-to-end.
	if _, err := ex.FetchPrice(context.Background(), "AAPL", "EUR"); err == nil {
		t.Fatal("expected error for unsupported quote currency")
	}
}
