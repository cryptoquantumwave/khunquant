package broker

import (
	"fmt"
	"strings"
	"sync"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ProviderFactory creates a Provider from config (default account).
type ProviderFactory func(cfg *config.Config) (Provider, error)

// ProviderAccountFactory creates a Provider for a specific named sub-account.
type ProviderAccountFactory func(cfg *config.Config, accountName string) (Provider, error)

var (
	mu               sync.RWMutex
	factories        = map[string]ProviderFactory{}
	accountFactories = map[string]ProviderAccountFactory{}
	instanceCache    = map[string]Provider{}
)

// RegisterFactory registers a named provider factory. Called from adapter init() functions.
// Re-registering a name drops that name's cached instances (tests re-register
// stubs under the same name and must not receive a previous stub).
func RegisterFactory(name string, f ProviderFactory) {
	mu.Lock()
	defer mu.Unlock()
	factories[name] = f
	dropCachedInstancesLocked(name)
}

// RegisterAccountFactory registers an account-aware provider factory.
func RegisterAccountFactory(name string, f ProviderAccountFactory) {
	mu.Lock()
	defer mu.Unlock()
	accountFactories[name] = f
	dropCachedInstancesLocked(name)
}

// dropCachedInstancesLocked removes every cached instance for name (all
// accounts). Caller must hold mu.
func dropCachedInstancesLocked(name string) {
	prefix := name + "\x00"
	for key := range instanceCache {
		if strings.HasPrefix(key, prefix) {
			delete(instanceCache, key)
		}
	}
}

// CreateProvider creates a provider by name using the registered factory.
// Instances are cached like CreateProviderForAccount — see there for why.
func CreateProvider(name string, cfg *config.Config) (Provider, error) {
	return CreateProviderForAccount(name, "", cfg)
}

// CreateProviderForAccount creates a provider for a specific named
// sub-account. Instances are cached by (name, accountName), mirroring
// exchanges.CreateExchangeForAccount: session-stateful providers (Webull's
// in-app-approved token, Settrade's single active session) must not mint a
// fresh, unauthenticated client on every tool call — doing so loses the
// session each time and, for Webull, burns through the login-attempt rate
// limit. Falls back to the default-account factory when accountName is
// empty or no account factory is registered.
func CreateProviderForAccount(name, accountName string, cfg *config.Config) (Provider, error) {
	cacheKey := name + "\x00" + accountName

	mu.RLock()
	if p, ok := instanceCache[cacheKey]; ok {
		mu.RUnlock()
		return p, nil
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	// Double-check after acquiring write lock.
	if p, ok := instanceCache[cacheKey]; ok {
		return p, nil
	}

	var (
		p   Provider
		err error
	)
	if af, ok := accountFactories[name]; ok && accountName != "" {
		p, err = af(cfg, accountName)
	} else if f, ok := factories[name]; ok {
		p, err = f(cfg)
	} else {
		return nil, fmt.Errorf("broker: provider %q not registered", name)
	}
	if err != nil {
		return nil, err
	}
	instanceCache[cacheKey] = p
	return p, nil
}

// ResetInstanceCache drops all cached provider instances so the next
// CreateProvider/CreateProviderForAccount call re-runs the factory against
// current config. Call after persisting config changes so edited
// credentials take effect without a process restart.
func ResetInstanceCache() {
	mu.Lock()
	defer mu.Unlock()
	instanceCache = map[string]Provider{}
}
