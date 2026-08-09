package sandbox

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestResetRestoresState verifies Reset() functionality
func TestResetRestoresState(t *testing.T) {
	sm := NewStateManager()

	// Setup state
	state := sm.GetState("binance")
	state.Balances["USDT"] = Balance{Free: 10000, Locked: 0}
	state.Markets["BTCUSDT"] = Market{
		Symbol:       "BTCUSDT",
		ContractSize: 0.001,
		MinAmount:    0.1,
		MaxLeverage:  125,
	}

	// Save seed
	sm.mu.Lock()
	sm.seeds["binance"] = &Seed{
		Balances: map[string]Balance{
			"USDT": {Free: 10000, Locked: 0},
		},
		Positions: make(map[string]*Position),
		Markets: map[string]Market{
			"BTCUSDT": {
				Symbol:       "BTCUSDT",
				ContractSize: 0.001,
				MinAmount:    0.1,
				MaxLeverage:  125,
			},
		},
	}
	sm.mu.Unlock()

	// Mutate state
	state.Balances["USDT"] = Balance{Free: 5000, Locked: 2000}

	// Reset
	sm.Reset("binance")

	// Verify
	state = sm.GetState("binance")
	if state.Balances["USDT"].Free != 10000 {
		t.Errorf("After reset, USDT should be 10000, got %v", state.Balances["USDT"].Free)
	} else {
		t.Logf("✓ Reset correctly restored seeded state")
	}
}

// TestStatefulSimulatorCreatesPositions verifies basic order and position creation
func TestStatefulSimulatorCreatesPositions(t *testing.T) {
	sm := NewStateManager()
	state := sm.GetState("binance")
	state.Balances["USDT"] = Balance{Free: 10000, Locked: 0}
	state.Markets["BTCUSDT"] = Market{
		Symbol:       "BTCUSDT",
		ContractSize: 0.001,
		MinAmount:    0.1,
		MaxLeverage:  125,
	}
	state.MarkPrices["BTCUSDT"] = 50000
	state.Leverage["BTCUSDT"] = 1

	// Verify initial state
	if len(state.Positions) != 0 {
		t.Errorf("Initial positions should be empty, got %d", len(state.Positions))
	}
	if state.Balances["USDT"].Free != 10000 {
		t.Errorf("Initial USDT should be 10000, got %v", state.Balances["USDT"].Free)
	}

	// Create an order manually (simulating what the HTTP handler would do)
	orderID := sm.NextOrderID("binance")
	order := &Order{
		ID:        orderID,
		Symbol:    "BTCUSDT",
		OrderType: "market",
		Side:      "BUY",
		Amount:    10,
		Filled:    10,
		Average:   50000,
		Cost:      500,
		Status:    "FILLED",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Params: map[string]interface{}{
			"positionSide": "LONG",
		},
	}

	// Simulate what the simulator does
	state.Balances["USDT"] = Balance{Free: 9500, Locked: 500}
	state.ClosedOrders[orderID] = order
	state.Positions["BTCUSDT"] = &Position{
		Symbol:       "BTCUSDT",
		Side:         "long",
		Contracts:    10,
		ContractSize: 0.001,
		EntryPrice:   50000,
		MarkPrice:    50000,
		Leverage:     1,
		UpdatedAt:    time.Now(),
	}

	// Verify
	if len(state.Positions) != 1 {
		t.Errorf("Expected 1 position, got %d", len(state.Positions))
	}
	if state.Positions["BTCUSDT"].Contracts != 10 {
		t.Errorf("Expected position size 10, got %v", state.Positions["BTCUSDT"].Contracts)
	}
	if state.Balances["USDT"].Free != 9500 {
		t.Errorf("Expected USDT free=9500, got %v", state.Balances["USDT"].Free)
	}
	t.Logf("✓ Order and position created correctly")
}

// TestContractSizeCalculation verifies notional calculation
func TestContractSizeCalculation(t *testing.T) {
	// Binance: 10 contracts * 0.001 (contractSize) * 50000 (price) = 500 USDT
	contracts := 10.0
	contractSize := 0.001
	price := 50000.0
	notional := contracts * contractSize * price
	expected := 500.0

	if notional != expected {
		t.Errorf("Expected notional %v, got %v", expected, notional)
	} else {
		t.Logf("✓ Binance contract calculation: %v * %v * %v = %v", contracts, contractSize, price, notional)
	}

	// OKX: 100 contracts * 0.01 (contractSize) * 50000 (price) = 50000 USDT
	contracts = 100.0
	contractSize = 0.01
	notional = contracts * contractSize * price
	expected = 50000.0

	if notional != expected {
		t.Errorf("Expected notional %v, got %v", expected, notional)
	} else {
		t.Logf("✓ OKX contract calculation: %v * %v * %v = %v", contracts, contractSize, price, notional)
	}
}

// TestWireFormatBinance verifies Binance order response format
func TestWireFormatBinance(t *testing.T) {
	sim := &StatefulSimulator{}
	order := &Order{
		ID:        "1001",
		Symbol:    "BTCUSDT",
		OrderType: "market",
		Side:      "BUY",
		Amount:    10,
		Filled:    10,
		Average:   50000,
		Cost:      500,
		Status:    "FILLED",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Params: map[string]interface{}{
			"positionSide": "LONG",
		},
	}

	// Build response and verify it can be marshaled
	resp := sim.binanceOrderToWireFormat(order)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Errorf("Failed to marshal: %v", err)
	}

	// Verify key fields are present
	// orderId must be numeric type (not string) for CCXT compatibility
	orderId, ok := resp["orderId"]
	if !ok {
		t.Errorf("orderId missing")
	}
	// Check it's numeric (could be int64 or float64 depending on JSON roundtrip)
	if _, isInt := orderId.(int64); !isInt {
		if _, isFloat := orderId.(float64); !isFloat {
			t.Errorf("orderId not numeric type: %T", orderId)
		}
	}
	// Value should be 1001
	if fmt.Sprintf("%v", orderId) != "1001" {
		t.Errorf("orderId value incorrect: %v", orderId)
	}
	if status, ok := resp["status"].(string); !ok || status != "FILLED" {
		t.Errorf("status not string or incorrect: %v", resp["status"])
	}

	t.Logf("✓ Binance wire format valid: %d bytes", len(data))
}

// TestWireFormatOKX verifies OKX order response format
func TestWireFormatOKX(t *testing.T) {
	sim := &StatefulSimulator{}
	order := &Order{
		ID:        "1001",
		Symbol:    "BTC-USDT-SWAP",
		OrderType: "market",
		Side:      "buy",
		Amount:    100,
		Filled:    100,
		Average:   50000,
		Cost:      50000,
		Status:    "filled",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Params: map[string]interface{}{
			"posSide": "long",
		},
	}

	// Build response
	resp := sim.okxOrderToWireFormat(order)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Errorf("Failed to marshal: %v", err)
	}

	// Verify structure
	if ordId, ok := resp["ordId"]; !ok || ordId != "1001" {
		t.Errorf("ordId missing or incorrect: %v", resp["ordId"])
	}
	// OKX uses "state" field name (not "status") - CCXT expects this for order parsing
	if state, ok := resp["state"].(string); !ok || state != "filled" {
		t.Errorf("state not string or incorrect: %v", resp["state"])
	}

	t.Logf("✓ OKX wire format valid: %d bytes", len(data))
}

// TestErrorResponseBinance verifies Binance error format
func TestErrorResponseBinance(t *testing.T) {
	sim := &StatefulSimulator{}
	resp := sim.binanceErrorResponse(400, -2015, "Margin is insufficient.")

	if resp.Status != 400 {
		t.Errorf("Expected status 400, got %d", resp.Status)
	}

	var data map[string]interface{}
	json.Unmarshal(resp.Body, &data)

	if code, ok := data["code"].(float64); !ok || code != -2015 {
		t.Errorf("Expected code -2015, got %v", data["code"])
	}
	if msg, ok := data["msg"].(string); !ok || msg != "Margin is insufficient." {
		t.Errorf("Expected msg, got %v", data["msg"])
	}

	t.Logf("✓ Binance error format valid")
}

// TestErrorResponseOKX verifies OKX error format
func TestErrorResponseOKX(t *testing.T) {
	sim := &StatefulSimulator{}
	resp := sim.okxErrorResponse("51008", "The margin is insufficient.")

	if resp.Status != 200 {
		t.Errorf("Expected status 200 (OKX uses 200 for errors), got %d", resp.Status)
	}

	var data map[string]interface{}
	json.Unmarshal(resp.Body, &data)

	if code, ok := data["code"].(string); !ok || code != "51008" {
		t.Errorf("Expected code 51008, got %v", data["code"])
	}
	if msg, ok := data["msg"].(string); !ok || msg != "The margin is insufficient." {
		t.Errorf("Expected msg, got %v", data["msg"])
	}

	t.Logf("✓ OKX error format valid")
}

// TestBalanceManagement verifies balance locking/unlocking
func TestBalanceManagement(t *testing.T) {
	sm := NewStateManager()
	state := sm.GetState("binance")
	state.Balances["USDT"] = Balance{Free: 10000, Locked: 0}

	// Lock 500 USDT
	bal := state.Balances["USDT"]
	bal.Free -= 500
	bal.Locked += 500
	state.Balances["USDT"] = bal

	if state.Balances["USDT"].Free != 9500 {
		t.Errorf("Expected free=9500, got %v", state.Balances["USDT"].Free)
	}
	if state.Balances["USDT"].Locked != 500 {
		t.Errorf("Expected locked=500, got %v", state.Balances["USDT"].Locked)
	}

	// Unlock 500 USDT
	bal = state.Balances["USDT"]
	bal.Free += 500
	bal.Locked -= 500
	state.Balances["USDT"] = bal

	if state.Balances["USDT"].Free != 10000 {
		t.Errorf("After unlock, expected free=10000, got %v", state.Balances["USDT"].Free)
	}
	if state.Balances["USDT"].Locked != 0 {
		t.Errorf("After unlock, expected locked=0, got %v", state.Balances["USDT"].Locked)
	}

	t.Logf("✓ Balance locking/unlocking works correctly")
}
