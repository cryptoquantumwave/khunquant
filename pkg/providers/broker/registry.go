package broker

import (
	"fmt"
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
)

// RegisterFactory registers a named provider factory. Called from adapter init() functions.
func RegisterFactory(name string, f ProviderFactory) {
	mu.Lock()
	defer mu.Unlock()
	factories[name] = f
}

// RegisterAccountFactory registers an account-aware provider factory.
func RegisterAccountFactory(name string, f ProviderAccountFactory) {
	mu.Lock()
	defer mu.Unlock()
	accountFactories[name] = f
}

// CreateProvider creates a provider by name using the registered factory.
func CreateProvider(name string, cfg *config.Config) (Provider, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("broker: provider %q not registered", name)
	}
	return f(cfg)
}

// CreateProviderForAccount creates a provider for a specific named sub-account.
// Falls back to CreateProvider (default account) if no account factory is registered.
func CreateProviderForAccount(name, accountName string, cfg *config.Config) (Provider, error) {
	mu.RLock()
	af, ok := accountFactories[name]
	mu.RUnlock()
	if ok {
		return af(cfg, accountName)
	}
	return CreateProvider(name, cfg)
}
