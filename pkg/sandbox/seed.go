package sandbox

// SeedDevDefaults gives every venue in store the dev-mode starting state the
// stateful simulator needs to accept orders: markets read from the fixtures, a mark
// price on each, and a USDT balance. The values are arbitrary dev defaults. Each
// seeded state is captured as the reset baseline, so a reset restores it instead of
// leaving the simulator inert until the process restarts.
func SeedDevDefaults(store *Store, sm *StateManager) {
	for _, venue := range store.Venues() {
		venueState := sm.GetState(venue)
		SeedMarketsFromFixtures(venue, store, venueState)

		// Seed dev-mode default values for MarkPrices and Balances
		// These are required for the simulator to accept orders
		// Values are arbitrary dev defaults; easily changed
		for symbol := range venueState.Markets {
			venueState.MarkPrices[symbol] = 50000 // Default price for all symbols
		}
		venueState.Balances["USDT"] = Balance{Free: 100000, Locked: 0}

		// Capture the seeded state as the reset baseline. Without this, calling the reset
		// tool will wipe all simulator state and leave it inert until process restart.
		sm.SnapshotAsSeed(venue)
	}
}
