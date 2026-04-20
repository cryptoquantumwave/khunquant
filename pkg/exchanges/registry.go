package exchanges

import (
	"fmt"
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
)

// RegisterFactory registers a named exchange factory. Called from subpackage init() functions.
func RegisterFactory(name string, f ExchangeFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = f
}

// RegisterAccountFactory registers an account-aware exchange factory.
func RegisterAccountFactory(name string, f ExchangeAccountFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	accountFactories[name] = f
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
// Falls back to CreateExchange (default account) if no account factory is registered.
func CreateExchangeForAccount(name, accountName string, cfg *config.Config) (Exchange, error) {
	factoriesMu.RLock()
	af, ok := accountFactories[name]
	factoriesMu.RUnlock()
	if ok {
		return af(cfg, accountName)
	}
	return CreateExchange(name, cfg)
}
