package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Balance represents a single asset balance in an account.
type Balance struct {
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// Position represents a futures position.
type Position struct {
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"` // "long" or "short"
	Contracts     float64   `json:"contracts"`
	ContractSize  float64   `json:"contractSize"` // base currency per contract
	EntryPrice    float64   `json:"entryPrice"`
	Leverage      int64     `json:"leverage"`
	MarkPrice     float64   `json:"markPrice"`
	UnrealisedPnL float64   `json:"unrealisedPnL"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Order represents an open or closed order.
type Order struct {
	ID        string                 `json:"id"`
	Symbol    string                 `json:"symbol"`
	OrderType string                 `json:"orderType"` // "limit", "market", etc.
	Side      string                 `json:"side"`      // "buy" or "sell"
	Amount    float64                `json:"amount"`
	Filled    float64                `json:"filled"`
	Average   float64                `json:"average"`
	Cost      float64                `json:"cost"`   // total cost in quote currency
	Status    string                 `json:"status"` // "open", "closed", "canceled"
	Fee       float64                `json:"fee"`    // paid fee
	CreatedAt time.Time              `json:"createdAt"`
	UpdatedAt time.Time              `json:"updatedAt"`
	Params    map[string]interface{} `json:"params,omitempty"`
}

// Market represents a market with contract size and precision info.
type Market struct {
	Symbol       string  `json:"symbol"`
	ContractSize float64 `json:"contractSize"` // base currency per contract
	MinAmount    float64 `json:"minAmount"`    // minimum order amount
	MaxLeverage  int64   `json:"maxLeverage"`
}

// VenueState represents the complete state for a single venue (binance, okx, etc.).
type VenueState struct {
	Balances     map[string]Balance   `json:"balances"`     // asset → Balance
	Positions    map[string]*Position `json:"positions"`    // symbol → Position
	OpenOrders   map[string]*Order    `json:"openOrders"`   // orderId → Order
	ClosedOrders map[string]*Order    `json:"closedOrders"` // orderId → Order
	Markets      map[string]Market    `json:"markets"`      // symbol → Market
	OrderIDSeq   int64                `json:"-"`            // monotonic order ID counter
	Leverage     map[string]int64     `json:"leverage"`     // symbol → leverage
	MarkPrices   map[string]float64   `json:"markPrices"`   // symbol → mark price
	ClosedAt     time.Time            `json:"closedAt"`
}

// Seed holds the initial state for resetting via Reset().
// MarkPrices is included so that Reset() can restore mark prices (needed for order simulation).
type Seed struct {
	Balances   map[string]Balance   `json:"balances"`
	Positions  map[string]*Position `json:"positions"`
	Markets    map[string]Market    `json:"markets"`
	MarkPrices map[string]float64   `json:"markPrices,omitempty"`
}

// StateManager holds all venue states and is concurrency-safe.
type StateManager struct {
	mu          sync.RWMutex
	venueStates map[string]*VenueState       // venue → VenueState
	seeds       map[string]*Seed             // venue → seed (for Reset)
	errorForces map[string]map[string]string // venue→path→errorCode for error simulation
}

// NewStateManager creates an empty StateManager.
func NewStateManager() *StateManager {
	return &StateManager{
		venueStates: make(map[string]*VenueState),
		seeds:       make(map[string]*Seed),
		errorForces: make(map[string]map[string]string),
	}
}

// LoadState loads initial state from a JSON file (e.g., <fixturesDir>/<venue>/state.json).
// If the file does not exist, it initializes an empty state.
func (sm *StateManager) LoadState(venue, filePath string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Initialize empty state.
	state := &VenueState{
		Balances:     make(map[string]Balance),
		Positions:    make(map[string]*Position),
		OpenOrders:   make(map[string]*Order),
		ClosedOrders: make(map[string]*Order),
		Markets:      make(map[string]Market),
		Leverage:     make(map[string]int64),
		MarkPrices:   make(map[string]float64),
	}

	// Try to read the seed file.
	data, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		// No file; start with empty state.
		sm.venueStates[venue] = state
		sm.seeds[venue] = &Seed{
			Balances:  make(map[string]Balance),
			Positions: make(map[string]*Position),
			Markets:   make(map[string]Market),
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state file %s: %w", filePath, err)
	}

	// Parse the seed.
	var seed Seed
	if err := json.Unmarshal(data, &seed); err != nil {
		return fmt.Errorf("unmarshal state file %s: %w", filePath, err)
	}

	// Deep copy balances and markets.
	state.Balances = make(map[string]Balance)
	for k, v := range seed.Balances {
		state.Balances[k] = v
	}

	state.Positions = make(map[string]*Position)
	for k, v := range seed.Positions {
		if v != nil {
			p := *v // deep copy
			state.Positions[k] = &p
		}
	}

	state.Markets = make(map[string]Market)
	for k, v := range seed.Markets {
		state.Markets[k] = v
	}

	sm.venueStates[venue] = state
	sm.seeds[venue] = &seed
	return nil
}

// GetState returns the current state for a venue. Creates empty state if not exists.
func (sm *StateManager) GetState(venue string) *VenueState {
	sm.mu.RLock()
	if state, ok := sm.venueStates[venue]; ok {
		sm.mu.RUnlock()
		return state
	}
	sm.mu.RUnlock()

	// Create empty state if it doesn't exist.
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if state, ok := sm.venueStates[venue]; ok {
		return state // race: another goroutine created it first
	}
	state := &VenueState{
		Balances:     make(map[string]Balance),
		Positions:    make(map[string]*Position),
		OpenOrders:   make(map[string]*Order),
		ClosedOrders: make(map[string]*Order),
		Markets:      make(map[string]Market),
		Leverage:     make(map[string]int64),
		MarkPrices:   make(map[string]float64),
	}
	sm.venueStates[venue] = state
	return state
}

// NextOrderID returns the next monotonic order ID for a venue.
func (sm *StateManager) NextOrderID(venue string) string {
	state := sm.GetState(venue)
	seq := atomic.AddInt64(&state.OrderIDSeq, 1)
	return fmt.Sprintf("%d", seq+1000) // 1001, 1002, ...
}

// SnapshotAsSeed captures the current state of a venue as the reset baseline.
// This is called by the gateway after seeding (loading markets, mark prices, and initial balances
// from fixtures), so that Reset() can later restore to this known good state. The snapshot is
// atomic with respect to the mutex.
func (sm *StateManager) SnapshotAsSeed(venue string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	state, ok := sm.venueStates[venue]
	if !ok {
		// No state exists yet; create an empty seed
		sm.seeds[venue] = &Seed{
			Balances:   make(map[string]Balance),
			Positions:  make(map[string]*Position),
			Markets:    make(map[string]Market),
			MarkPrices: make(map[string]float64),
		}
		return
	}

	// Deep copy the current state as a seed
	seed := &Seed{
		Balances:   make(map[string]Balance),
		Positions:  make(map[string]*Position),
		Markets:    make(map[string]Market),
		MarkPrices: make(map[string]float64),
	}

	for k, v := range state.Balances {
		seed.Balances[k] = v
	}
	for k, v := range state.Positions {
		if v != nil {
			p := *v // deep copy
			seed.Positions[k] = &p
		}
	}
	for k, v := range state.Markets {
		seed.Markets[k] = v
	}
	for k, v := range state.MarkPrices {
		seed.MarkPrices[k] = v
	}

	sm.seeds[venue] = seed
}

// Reset restores a venue to its seeded state (initial state).
// If no seed has been registered (via SnapshotAsSeed), this creates an empty state.
func (sm *StateManager) Reset(venue string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	seed, ok := sm.seeds[venue]
	if !ok {
		// No seed saved; create empty state (consistent with seeded branch).
		state := &VenueState{
			Balances:     make(map[string]Balance),
			Positions:    make(map[string]*Position),
			OpenOrders:   make(map[string]*Order),
			ClosedOrders: make(map[string]*Order),
			Markets:      make(map[string]Market),
			Leverage:     make(map[string]int64),
			MarkPrices:   make(map[string]float64),
		}
		sm.venueStates[venue] = state
		return nil
	}

	// Deep copy seed to new state.
	state := &VenueState{
		Balances:     make(map[string]Balance),
		Positions:    make(map[string]*Position),
		OpenOrders:   make(map[string]*Order),
		ClosedOrders: make(map[string]*Order),
		Markets:      make(map[string]Market),
		Leverage:     make(map[string]int64),
		MarkPrices:   make(map[string]float64),
	}

	for k, v := range seed.Balances {
		state.Balances[k] = v
	}
	for k, v := range seed.Positions {
		if v != nil {
			p := *v // deep copy
			state.Positions[k] = &p
		}
	}
	for k, v := range seed.Markets {
		state.Markets[k] = v
	}
	for k, v := range seed.MarkPrices {
		state.MarkPrices[k] = v
	}

	sm.venueStates[venue] = state
	return nil
}

// SetErrorForce forces an endpoint to return an error.
// errorCode should be the exchange error code (e.g., "51008" for OKX, "-2015" for Binance).
func (sm *StateManager) SetErrorForce(venue, path, errorCode string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.errorForces[venue] == nil {
		sm.errorForces[venue] = make(map[string]string)
	}
	sm.errorForces[venue][path] = errorCode
}

// GetErrorForce returns the forced error code for an endpoint, or empty string if none.
func (sm *StateManager) GetErrorForce(venue, path string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.errorForces[venue][path]
}

// ClearErrorForce removes any forced error for an endpoint.
func (sm *StateManager) ClearErrorForce(venue, path string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.errorForces[venue] != nil {
		delete(sm.errorForces[venue], path)
	}
}
