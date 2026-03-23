package broker

import (
	"fmt"
	"strings"
	"sync"
)

// SymbolResolver maps plain agent-facing symbols ("BTC/USDT", "PTT") to the
// correct (providerID, normalizedSymbol) pair without requiring the LLM to
// specify URN syntax.
//
// Agent tools accept plain `symbol` parameters. Internally the resolver finds
// which registered provider handles that symbol and normalises it for the wire.
type SymbolResolver interface {
	// Resolve returns the canonical providerID and normalised symbol for s.
	// Returns ErrSymbolNotFound if no registered provider knows the symbol.
	Resolve(s string) (providerID string, normalised string, err error)

	// Register maps a symbol (or prefix) to a provider.
	// Called by provider init() functions to advertise their known symbols.
	// An empty prefix ("") means the provider accepts any symbol (catch-all).
	Register(providerID string, symbols ...string)
}

// symbolEntry is a (providerID, symbol) mapping stored by DefaultResolver.
type symbolEntry struct {
	providerID string
	symbol     string
}

// defaultResolver is the package-level resolver implementation.
type defaultResolver struct {
	mu      sync.RWMutex
	exact   map[string][]symbolEntry // exact symbol → providers
	catchAll []string                // provider IDs that accept any symbol
}

// DefaultResolver is the global SymbolResolver used by all tools.
// Provider adapters register their symbols in init().
var DefaultResolver SymbolResolver = &defaultResolver{
	exact: make(map[string][]symbolEntry),
}

func (r *defaultResolver) Register(providerID string, symbols ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(symbols) == 0 {
		// catch-all registration
		r.catchAll = append(r.catchAll, providerID)
		return
	}
	for _, s := range symbols {
		norm := normaliseSymbol(s)
		r.exact[norm] = append(r.exact[norm], symbolEntry{providerID: providerID, symbol: s})
	}
}

// Resolve looks up a provider for the given symbol.
// Resolution order:
//  1. Exact match (case-insensitive, "/" normalised)
//  2. First catch-all provider
//  3. ErrSymbolNotFound
func (r *defaultResolver) Resolve(s string) (string, string, error) {
	norm := normaliseSymbol(s)

	r.mu.RLock()
	entries, ok := r.exact[norm]
	catchAll := r.catchAll
	r.mu.RUnlock()

	if ok && len(entries) > 0 {
		e := entries[0]
		return e.providerID, e.symbol, nil
	}
	if len(catchAll) > 0 {
		// return the symbol as-is for the catch-all provider
		return catchAll[0], s, nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrSymbolNotFound, s)
}

// normaliseSymbol upper-cases and normalises separator to "/".
// "btc/usdt" → "BTC/USDT", "BTC-USDT" → "BTC/USDT", "BTCUSDT" → "BTCUSDT"
func normaliseSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "/")
	return s
}

// ResolveOrPassthrough is a helper used by tools: if the caller already
// provided a providerID it is returned as-is (passthrough). Otherwise
// DefaultResolver is queried.
func ResolveOrPassthrough(providerID, symbol string) (string, string, error) {
	if providerID != "" {
		return providerID, symbol, nil
	}
	return DefaultResolver.Resolve(symbol)
}
