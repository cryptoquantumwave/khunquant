package broker

import (
	"errors"
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ErrPaperModeUnsupported marks an operation that has no simulated form and so
// must not run while paper_trading_mode is on. Callers wrap it so the refusal
// can be told apart from an ordinary failure.
var ErrPaperModeUnsupported = errors.New("broker: operation is not simulated in paper_trading_mode")

// ErrOrderExecutionUnsupported is returned by TradingProviderFor when the
// provider exists but cannot place orders.
var ErrOrderExecutionUnsupported = errors.New("broker: provider does not support order execution")

// IsPaperMode reports whether paper_trading_mode is on. It reads the same
// config flag the tool policy layer is armed from (tools.SetPolicyConfig), so
// the two cannot disagree. A nil config is treated as live: callers that reach
// an exchange already need a non-nil one.
func IsPaperMode(cfg *config.Config) bool {
	return cfg != nil && cfg.TradingRisk.PaperTradingMode
}

// TradingProviderFor returns the provider to place SPOT orders with, plus the
// underlying real provider for reads.
//
// In paper mode the first value is a SimulationProvider wrapping the real one:
// order placement and cancellation are simulated and never reach the exchange,
// while market data and balances still come from the real exchange. Outside
// paper mode the first value is the real provider itself.
//
// The real provider is returned in both modes so a caller can run its gates
// (market status, pricing, balance preflight) identically either way. Place
// orders ONLY through the first value.
//
// Errors: a provider that cannot be built returns (nil, nil, err); one that
// cannot place orders returns (nil, real, ErrOrderExecutionUnsupported).
func TradingProviderFor(cfg *config.Config, providerID, account string) (TradingProvider, Provider, error) {
	if cfg == nil {
		return nil, nil, errors.New("broker: nil config")
	}
	p, err := CreateProviderForAccount(providerID, account, cfg)
	if err != nil {
		return nil, nil, err
	}
	tp, ok := p.(TradingProvider)
	if !ok {
		return nil, p, fmt.Errorf("%w: %q", ErrOrderExecutionUnsupported, providerID)
	}
	if IsPaperMode(cfg) {
		return NewSimulationProvider(p), p, nil
	}
	return tp, p, nil
}
