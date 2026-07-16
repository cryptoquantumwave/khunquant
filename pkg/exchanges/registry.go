package exchanges

import (
	"fmt"
	"strings"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ErrAccountNotFound returns a standardised error when a named account does not exist
// for a given exchange. All exchange packages should use this instead of a hand-crafted
// fmt.Errorf so that the message format is maintained in a single place.
func ErrAccountNotFound(exchange, accountName string, available []string) error {
	return fmt.Errorf("%s: account %q not found (available: %v)", exchange, accountName, available)
}

// ExchangeFactory is a constructor function that creates an Exchange from config.
// Each exchange subpackage registers one factory via init().
type ExchangeFactory func(cfg *config.Config) (Exchange, error)

// ExchangeAccountFactory creates an Exchange for a specific named sub-account.
type ExchangeAccountFactory func(cfg *config.Config, accountName string) (Exchange, error)

var (
	factoriesMu      sync.RWMutex
	factories        = map[string]ExchangeFactory{}
	accountFactories = map[string]ExchangeAccountFactory{}
	instanceCache    = map[string]Exchange{}
)

// RegisterFactory registers a named exchange factory. Called from subpackage init() functions.
// Re-registering a name drops that name's cached instances (tests re-register
// stubs under the same name and must not receive a previous stub).
func RegisterFactory(name string, f ExchangeFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = f
	dropCachedInstancesLocked(name)
}

// RegisterAccountFactory registers an account-aware exchange factory.
func RegisterAccountFactory(name string, f ExchangeAccountFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	accountFactories[name] = f
	dropCachedInstancesLocked(name)
}

// dropCachedInstancesLocked removes every cached instance for name (all
// accounts). Caller must hold factoriesMu.
func dropCachedInstancesLocked(name string) {
	prefix := name + "\x00"
	for key := range instanceCache {
		if strings.HasPrefix(key, prefix) {
			delete(instanceCache, key)
		}
	}
}

// CreateExchange creates an exchange by name using the registered factory.
func CreateExchange(name string, cfg *config.Config) (Exchange, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("exchange %q not registered", name)
	}
	return f(cfg)
}

// CreateExchangeForAccount creates an exchange for a specific named sub-account.
// Instances are cached by (name, accountName) so concurrent calls reuse the same
// client — important for exchanges like Settrade that enforce a single active session.
// Falls back to CreateExchange (default account) if no account factory is registered.
func CreateExchangeForAccount(name, accountName string, cfg *config.Config) (Exchange, error) {
	cacheKey := name + "\x00" + accountName

	factoriesMu.RLock()
	if ex, ok := instanceCache[cacheKey]; ok {
		factoriesMu.RUnlock()
		return ex, nil
	}
	factoriesMu.RUnlock()

	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	// Double-check after acquiring write lock.
	if ex, ok := instanceCache[cacheKey]; ok {
		return ex, nil
	}

	var (
		ex  Exchange
		err error
	)
	if af, ok := accountFactories[name]; ok {
		ex, err = af(cfg, accountName)
	} else if f, ok := factories[name]; ok {
		ex, err = f(cfg)
	} else {
		return nil, fmt.Errorf("exchange %q not registered", name)
	}
	if err != nil {
		return nil, err
	}
	instanceCache[cacheKey] = ex
	return ex, nil
}

// ResetInstanceCache drops all cached exchange instances so the next
// CreateExchangeForAccount call re-runs the factory against current config.
// Call after persisting config changes (e.g. the web launcher's config-save
// handlers) so edited credentials/hosts take effect without a process
// restart.
func ResetInstanceCache() {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	instanceCache = map[string]Exchange{}
}
