package exchanges

import (
	"fmt"
	"sync"

	"github.com/khunquant/khunquant/pkg/config"
)

// ExchangeFactory is a constructor function that creates an Exchange from config.
// Each exchange subpackage registers one factory via init().
type ExchangeFactory func(cfg *config.Config) (Exchange, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]ExchangeFactory{}
)

// RegisterFactory registers a named exchange factory. Called from subpackage init() functions.
func RegisterFactory(name string, f ExchangeFactory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[name] = f
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
