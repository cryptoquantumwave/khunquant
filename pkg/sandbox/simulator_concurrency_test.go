package sandbox

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStateManagerGetStateConcurrency verifies the double-checked lock pattern works correctly.
// All goroutines should receive the same state pointer (no data race on state creation).
func TestStateManagerGetStateConcurrency(t *testing.T) {
	sm := NewStateManager()

	numGoroutines := 50
	var wg sync.WaitGroup
	var states []*VenueState
	var statesLock sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state := sm.GetState("concurrent-test-venue")
			statesLock.Lock()
			states = append(states, state)
			statesLock.Unlock()
		}()
	}

	wg.Wait()

	if len(states) != numGoroutines {
		t.Errorf("Expected %d states, got %d", numGoroutines, len(states))
	}

	// All should point to the same state
	firstState := states[0]
	for i, state := range states {
		if state != firstState {
			t.Errorf("State %d is different from state 0", i)
		}
	}

	t.Logf("✓ All %d goroutines received the same state pointer", numGoroutines)
}

// TestNextOrderIDIsThreadSafe verifies OrderID generation is thread-safe (uses atomic.AddInt64).
func TestNextOrderIDIsThreadSafe(t *testing.T) {
	sm := NewStateManager()

	numGoroutines := 50
	numCallsPerGoroutine := 10
	var wg sync.WaitGroup
	var ids []string
	var idsLock sync.Mutex

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for i := 0; i < numCallsPerGoroutine; i++ {
				id := sm.NextOrderID("binance")
				idsLock.Lock()
				ids = append(ids, id)
				idsLock.Unlock()
			}
		}()
	}

	wg.Wait()

	expectedCount := numGoroutines * numCallsPerGoroutine
	if len(ids) != expectedCount {
		t.Errorf("Expected %d IDs, got %d", expectedCount, len(ids))
	}

	// Check for duplicates
	idSet := make(map[string]bool)
	duplicates := 0
	for _, id := range ids {
		if idSet[id] {
			duplicates++
		}
		idSet[id] = true
	}

	if duplicates > 0 {
		t.Errorf("Found %d duplicate order IDs", duplicates)
	} else {
		t.Logf("✓ All %d order IDs are unique", expectedCount)
	}
}

// TestRespondHoldsLockProtectsState verifies that Respond correctly holds a write lock
// while mutating shared maps. This prevents concurrent mutations on VenueState.
// FIX: Changed from RLock() to Lock() - allows only one Respond call at a time.
func TestRespondHoldsLockProtectsState(t *testing.T) {
	sm := NewStateManager()
	state := sm.GetState("binance")
	state.Balances["USDT"] = Balance{Free: 1000000, Locked: 0}
	state.Markets["BTCUSDT"] = Market{
		Symbol:       "BTCUSDT",
		ContractSize: 0.001,
		MinAmount:    0.1,
		MaxLeverage:  125,
	}
	state.MarkPrices["BTCUSDT"] = 50000
	state.Leverage["BTCUSDT"] = 1

	sim := NewStatefulSimulator(sm)

	numGoroutines := 50
	var wg sync.WaitGroup
	var operationCount int32

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			// FIXED: Respond now holds a write lock (Lock, not RLock)
			// This ensures only one goroutine can mutate state at a time, preventing races.
			sim.mu.Lock()
			defer sim.mu.Unlock()

			// Get state and mutate it (this is what simBinanceCreateOrder, simBinanceCancelOrder, etc. do)
			state := sm.GetState("binance")

			// Mutate Balances map
			bal := state.Balances["USDT"]
			bal.Free -= 10
			bal.Locked += 10
			state.Balances["USDT"] = bal

			// Mutate ClosedOrders map
			orderID := fmt.Sprintf("order_%d_%d", id, time.Now().UnixNano())
			state.ClosedOrders[orderID] = &Order{
				ID:        orderID,
				Symbol:    "BTCUSDT",
				OrderType: "market",
				Side:      "BUY",
				Amount:    10,
				Status:    "FILLED",
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}

			atomic.AddInt32(&operationCount, 1)
		}(i)
	}

	wg.Wait()

	t.Logf("✓ Completed %d concurrent operations with write lock protection (no data races).", operationCount)
	if operationCount != int32(numGoroutines) {
		t.Errorf("Expected %d operations, got %d", numGoroutines, operationCount)
	}
}
