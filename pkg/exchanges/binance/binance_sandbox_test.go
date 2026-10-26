package binance

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A client built while the sandbox is armed but no server runs must not exist: it
// would point at the real exchange and the instance cache would keep it.
func TestNewExchange_ArmedWithoutServerFails(t *testing.T) {
	sandbox.SetGlobalState(true, "")
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	ex, err := NewBinanceExchange(config.ExchangeAccount{}, false)
	if err == nil || ex != nil {
		t.Fatalf("NewBinanceExchange = (%v, %v), want an error and no client", ex, err)
	}
}

func TestNewExchange_ArmedWithServerBuilds(t *testing.T) {
	sandbox.SetGlobalState(true, "http://127.0.0.1:9")
	t.Cleanup(func() { sandbox.SetGlobalState(false, "") })

	ex, err := NewBinanceExchange(config.ExchangeAccount{}, false)
	if err != nil || ex == nil {
		t.Fatalf("NewBinanceExchange = (%v, %v), want a client", ex, err)
	}
}
