package settrade

import (
	"encoding/json"
	"errors"
	"testing"
)

// --- roundToTickSize ---

func TestRoundToTickSize_TierBelow2(t *testing.T) {
	// tick = 0.01; 1.234 → floor to 1.23
	got := roundToTickSize(1.234)
	if got != 1.23 {
		t.Errorf("price 1.234: want 1.23, got %v", got)
	}
}

func TestRoundToTickSize_TierBelow5(t *testing.T) {
	// tick = 0.02; 3.05 → floor to 3.04
	got := roundToTickSize(3.05)
	if got != 3.04 {
		t.Errorf("price 3.05: want 3.04, got %v", got)
	}
}

func TestRoundToTickSize_TierBelow10(t *testing.T) {
	// tick = 0.05; 7.00 is already a multiple of 0.05
	got := roundToTickSize(7.00)
	if got != 7.00 {
		t.Errorf("price 7.00: want 7.00, got %v", got)
	}
}

func TestRoundToTickSize_TierBelow25(t *testing.T) {
	// tick = 0.10; 12.39 → floor to 12.30
	got := roundToTickSize(12.39)
	if got != 12.30 {
		t.Errorf("price 12.39: want 12.30, got %v", got)
	}
}

func TestRoundToTickSize_TierBelow50(t *testing.T) {
	// tick = 0.25; 30.60 → floor to 30.50
	got := roundToTickSize(30.60)
	if got != 30.50 {
		t.Errorf("price 30.60: want 30.50, got %v", got)
	}
}

func TestRoundToTickSize_TierBelow100(t *testing.T) {
	// tick = 0.50; 75.70 → floor to 75.50
	got := roundToTickSize(75.70)
	if got != 75.50 {
		t.Errorf("price 75.70: want 75.50, got %v", got)
	}
}

func TestRoundToTickSize_TierAbove100(t *testing.T) {
	// tick = 1.00; 123.9 → floor to 123.0
	got := roundToTickSize(123.9)
	if got != 123.0 {
		t.Errorf("price 123.9: want 123.0, got %v", got)
	}
}

func TestRoundToTickSize_ExactBoundary(t *testing.T) {
	// Exactly at tier boundary 2.00 → tick = 0.02; 2.00 is already multiple of 0.02
	got := roundToTickSize(2.00)
	if got != 2.00 {
		t.Errorf("price 2.00: want 2.00, got %v", got)
	}
}

// --- toSetSymbol ---

func TestToSetSymbol_SlashSymbol(t *testing.T) {
	got := toSetSymbol("PTT/THB")
	if got != "PTT" {
		t.Errorf("want PTT, got %q", got)
	}
}

func TestToSetSymbol_NoSlash(t *testing.T) {
	got := toSetSymbol("aot")
	if got != "AOT" {
		t.Errorf("want AOT, got %q", got)
	}
}

func TestToSetSymbol_LowercaseBase(t *testing.T) {
	got := toSetSymbol("cpf/thb")
	if got != "CPF" {
		t.Errorf("want CPF, got %q", got)
	}
}

func TestToSetSymbol_AlreadyUpper(t *testing.T) {
	got := toSetSymbol("KBANK/THB")
	if got != "KBANK" {
		t.Errorf("want KBANK, got %q", got)
	}
}

// --- settradeOrderToCCXT status mapping ---

func orderWithStatus(status string) settradeOrder {
	return settradeOrder{
		OrderNo:   "ord1",
		Symbol:    "PTT",
		Side:      "Buy",
		PriceType: "Limit",
		Volume:    100,
		Price:     30.0,
		Status:    status,
	}
}

func TestSettradeOrderToCCXT_OpenStatuses(t *testing.T) {
	openStatuses := []string{"A", "P", "SC", "S", "SX", "WC", "OPEN", "PENDING",
		"a", "open", "pending"}
	for _, s := range openStatuses {
		o := settradeOrderToCCXT(orderWithStatus(s))
		if *o.Status != "open" {
			t.Errorf("status %q: want open, got %q", s, *o.Status)
		}
	}
}

func TestSettradeOrderToCCXT_ClosedStatuses(t *testing.T) {
	closedStatuses := []string{"M", "MATCHED", "FILLED", "matched", "filled"}
	for _, s := range closedStatuses {
		o := settradeOrderToCCXT(orderWithStatus(s))
		if *o.Status != "closed" {
			t.Errorf("status %q: want closed, got %q", s, *o.Status)
		}
	}
}

func TestSettradeOrderToCCXT_CanceledStatuses(t *testing.T) {
	canceledStatuses := []string{"C", "CM", "CS", "CX", "CANCELLED", "CANCELED",
		"cancelled", "canceled"}
	for _, s := range canceledStatuses {
		o := settradeOrderToCCXT(orderWithStatus(s))
		if *o.Status != "canceled" {
			t.Errorf("status %q: want canceled, got %q", s, *o.Status)
		}
	}
}

func TestSettradeOrderToCCXT_RejectedStatuses(t *testing.T) {
	for _, s := range []string{"R", "REJECTED", "rejected"} {
		o := settradeOrderToCCXT(orderWithStatus(s))
		if *o.Status != "rejected" {
			t.Errorf("status %q: want rejected, got %q", s, *o.Status)
		}
	}
}

func TestSettradeOrderToCCXT_PriceTypeMapping(t *testing.T) {
	cases := []struct {
		priceType string
		wantType  string
	}{
		{"Limit", "limit"},
		{"ATO", "market"},
		{"ATC", "market"},
		{"MP", "mp"},
	}
	for _, tc := range cases {
		o := settradeOrderToCCXT(settradeOrder{PriceType: tc.priceType, Symbol: "PTT"})
		if *o.Type != tc.wantType {
			t.Errorf("priceType %q: want %q, got %q", tc.priceType, tc.wantType, *o.Type)
		}
	}
}

func TestSettradeOrderToCCXT_SymbolAppendsTHB(t *testing.T) {
	o := settradeOrderToCCXT(orderWithStatus("OPEN"))
	if *o.Symbol != "PTT/THB" {
		t.Errorf("want PTT/THB, got %q", *o.Symbol)
	}
}

func TestSettradeOrderToCCXT_SideLowercased(t *testing.T) {
	o := settradeOrderToCCXT(settradeOrder{Symbol: "PTT", Side: "Buy", Status: "OPEN"})
	if *o.Side != "buy" {
		t.Errorf("want buy, got %q", *o.Side)
	}
}

func TestSettradeOrderToCCXT_VolumeFields(t *testing.T) {
	raw := settradeOrder{
		OrderNo:   "x",
		Symbol:    "PTT",
		Side:      "Buy",
		PriceType: "Limit",
		Volume:    500,
		FilledVol: 200,
		Balance:   300,
		Price:     32.0,
		Status:    "P",
	}
	o := settradeOrderToCCXT(raw)
	if *o.Amount != 500 {
		t.Errorf("Amount: want 500, got %v", *o.Amount)
	}
	if *o.Filled != 200 {
		t.Errorf("Filled: want 200, got %v", *o.Filled)
	}
	if *o.Remaining != 300 {
		t.Errorf("Remaining: want 300, got %v", *o.Remaining)
	}
}

// --- isUnauthorized ---

func TestIsUnauthorized_NilError(t *testing.T) {
	if isUnauthorized(nil) {
		t.Error("isUnauthorized(nil): expected false")
	}
}

func TestIsUnauthorized_UnrelatedError(t *testing.T) {
	if isUnauthorized(errors.New("network timeout")) {
		t.Error("isUnauthorized: expected false for non-401 error")
	}
}

func TestIsUnauthorized_401Prefix(t *testing.T) {
	err := errors.New("settrade: HTTP 401 unauthorized — token expired")
	if !isUnauthorized(err) {
		t.Error("isUnauthorized: expected true for 401 error")
	}
}

func TestIsUnauthorized_ShortError(t *testing.T) {
	// Error string shorter than 22 bytes — should not panic.
	if isUnauthorized(errors.New("short")) {
		t.Error("isUnauthorized: expected false for short error")
	}
}

// --- refreshRequest ---

func TestRefreshRequest_MarshalsSDKFieldNames(t *testing.T) {
	body := refreshRequest{APIKey: "app-id", RefreshToken: "refresh-token"}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal refreshRequest: %v", err)
	}

	want := `{"refreshToken":"refresh-token","apiKey":"app-id"}`
	if string(data) != want {
		t.Fatalf("refreshRequest JSON: want %s, got %s", want, data)
	}
}

// --- quoteToCCXT ---

func TestQuoteToCCXT_MapsFields(t *testing.T) {
	resp := quoteResponse{
		Last:          100.5,
		High:          110.0,
		Low:           95.0,
		TotalVolume:   1_000_000,
		PercentChange: 2.5,
	}
	ticker := quoteToCCXT("PTT/THB", resp)

	if ticker.Symbol == nil || *ticker.Symbol != "PTT/THB" {
		t.Errorf("Symbol: want PTT/THB, got %v", ticker.Symbol)
	}
	if ticker.Last == nil || *ticker.Last != 100.5 {
		t.Errorf("Last: want 100.5, got %v", ticker.Last)
	}
	if ticker.High == nil || *ticker.High != 110.0 {
		t.Errorf("High: want 110.0, got %v", ticker.High)
	}
	if ticker.Low == nil || *ticker.Low != 95.0 {
		t.Errorf("Low: want 95.0, got %v", ticker.Low)
	}
	if ticker.BaseVolume == nil || *ticker.BaseVolume != 1_000_000 {
		t.Errorf("BaseVolume: want 1000000, got %v", ticker.BaseVolume)
	}
	if ticker.Percentage == nil || *ticker.Percentage != 2.5 {
		t.Errorf("Percentage: want 2.5, got %v", ticker.Percentage)
	}
	if ticker.Timestamp == nil || *ticker.Timestamp <= 0 {
		t.Error("Timestamp: expected positive Unix millisecond value")
	}
}
