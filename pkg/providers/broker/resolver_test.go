package broker_test

import (
	"errors"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestDefaultResolver_RegisterAndResolve(t *testing.T) {
	const prov = "test_resolver_prov_1"
	broker.DefaultResolver.Register(prov, "TESTCOIN1/USDT")

	gotProv, gotSym, err := broker.DefaultResolver.Resolve("TESTCOIN1/USDT")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if gotProv != prov {
		t.Errorf("provider: want %q got %q", prov, gotProv)
	}
	if gotSym != "TESTCOIN1/USDT" {
		t.Errorf("symbol: want %q got %q", "TESTCOIN1/USDT", gotSym)
	}
}

func TestDefaultResolver_NormalisesCase(t *testing.T) {
	const prov = "test_resolver_prov_2"
	broker.DefaultResolver.Register(prov, "TESTCOIN2/USDT")

	// Lower-case query should resolve via case-insensitive match.
	gotProv, _, err := broker.DefaultResolver.Resolve("testcoin2/usdt")
	if err != nil {
		t.Fatalf("Resolve lowercase: %v", err)
	}
	if gotProv != prov {
		t.Errorf("provider: want %q got %q", prov, gotProv)
	}
}

func TestDefaultResolver_NormalisesDash(t *testing.T) {
	const prov = "test_resolver_prov_3"
	broker.DefaultResolver.Register(prov, "TESTCOIN3/USDT")

	// Dash-separated input should be normalised to slash-separated.
	gotProv, _, err := broker.DefaultResolver.Resolve("TESTCOIN3-USDT")
	if err != nil {
		t.Fatalf("Resolve dash: %v", err)
	}
	if gotProv != prov {
		t.Errorf("provider: want %q got %q", prov, gotProv)
	}
}

func TestDefaultResolver_ErrSymbolNotFound(t *testing.T) {
	_, _, err := broker.DefaultResolver.Resolve("NOTREGISTERED_XYZ_999/USDT")
	if !errors.Is(err, broker.ErrSymbolNotFound) {
		t.Errorf("expected ErrSymbolNotFound, got %v", err)
	}
}

func TestResolveOrPassthrough_WithProviderID(t *testing.T) {
	// When providerID is explicitly set it is returned as-is without querying DefaultResolver.
	pid, sym, err := broker.ResolveOrPassthrough("binance", "BTC/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != "binance" {
		t.Errorf("provider: want %q got %q", "binance", pid)
	}
	if sym != "BTC/USDT" {
		t.Errorf("symbol: want %q got %q", "BTC/USDT", sym)
	}
}

func TestResolveOrPassthrough_WithoutProviderID_NotFound(t *testing.T) {
	_, _, err := broker.ResolveOrPassthrough("", "ZZZZ_DEFINITELY_NOT_REGISTERED/USDT")
	if !errors.Is(err, broker.ErrSymbolNotFound) {
		t.Errorf("expected ErrSymbolNotFound, got %v", err)
	}
}

func TestResolveOrPassthrough_WithoutProviderID_Found(t *testing.T) {
	const prov = "test_resolver_prov_4"
	broker.DefaultResolver.Register(prov, "TESTCOIN4/USDT")

	pid, _, err := broker.ResolveOrPassthrough("", "TESTCOIN4/USDT")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pid != prov {
		t.Errorf("provider: want %q got %q", prov, pid)
	}
}
