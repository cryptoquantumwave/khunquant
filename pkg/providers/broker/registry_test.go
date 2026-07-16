package broker_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestRegisterFactory_Roundtrip(t *testing.T) {
	const name = "test_exchange_registry"
	broker.RegisterFactory(name, func(cfg *config.Config) (broker.Provider, error) {
		return NewMockProvider(name), nil
	})

	p, err := broker.CreateProvider(name, &config.Config{})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID() != name {
		t.Fatalf("expected ID %q, got %q", name, p.ID())
	}
}

func TestRegisterAccountFactory_Roundtrip(t *testing.T) {
	const name = "test_exchange_account_registry"
	broker.RegisterAccountFactory(name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		return NewMockProvider(accountName), nil
	})

	p, err := broker.CreateProviderForAccount(name, "myaccount", &config.Config{})
	if err != nil {
		t.Fatalf("CreateProviderForAccount: %v", err)
	}
	if p.ID() != "myaccount" {
		t.Fatalf("expected ID %q, got %q", "myaccount", p.ID())
	}
}

func TestCreateProvider_NotRegistered(t *testing.T) {
	_, err := broker.CreateProvider("nonexistent_provider_xyz", &config.Config{})
	if err == nil {
		t.Fatal("expected error for unregistered provider")
	}
}

// TestCreateProviderForAccount_CachesInstances verifies repeated calls reuse
// the same provider instance instead of re-running the factory — session-
// stateful providers (Webull, Settrade) must not mint a fresh login per tool
// call.
func TestCreateProviderForAccount_CachesInstances(t *testing.T) {
	const name = "test_registry_instance_cache"
	factoryCalls := 0
	broker.RegisterAccountFactory(name, func(cfg *config.Config, accountName string) (broker.Provider, error) {
		factoryCalls++
		return NewMockProvider(accountName), nil
	})

	p1, err := broker.CreateProviderForAccount(name, "main", &config.Config{})
	if err != nil {
		t.Fatalf("CreateProviderForAccount #1: %v", err)
	}
	p2, err := broker.CreateProviderForAccount(name, "main", &config.Config{})
	if err != nil {
		t.Fatalf("CreateProviderForAccount #2: %v", err)
	}
	if p1 != p2 {
		t.Fatal("expected the cached instance to be reused for the same (name, account)")
	}
	if factoryCalls != 1 {
		t.Fatalf("expected the factory to run once, ran %d times", factoryCalls)
	}

	// A different account gets its own instance.
	if _, err := broker.CreateProviderForAccount(name, "other", &config.Config{}); err != nil {
		t.Fatalf("CreateProviderForAccount (other): %v", err)
	}
	if factoryCalls != 2 {
		t.Fatalf("expected the factory to run for a new account, calls=%d", factoryCalls)
	}
}

// TestRegisterFactory_DropsCachedInstances verifies re-registering a name
// (tests swap stubs under a stable name) evicts previously cached instances.
func TestRegisterFactory_DropsCachedInstances(t *testing.T) {
	const name = "test_registry_reregister_evicts"
	broker.RegisterFactory(name, func(cfg *config.Config) (broker.Provider, error) {
		return NewMockProvider("first"), nil
	})
	p1, err := broker.CreateProvider(name, &config.Config{})
	if err != nil {
		t.Fatalf("CreateProvider #1: %v", err)
	}
	if p1.ID() != "first" {
		t.Fatalf("expected first stub, got %q", p1.ID())
	}

	broker.RegisterFactory(name, func(cfg *config.Config) (broker.Provider, error) {
		return NewMockProvider("second"), nil
	})
	p2, err := broker.CreateProvider(name, &config.Config{})
	if err != nil {
		t.Fatalf("CreateProvider #2: %v", err)
	}
	if p2.ID() != "second" {
		t.Fatalf("expected re-registration to evict the cached first stub, got %q", p2.ID())
	}
}

// TestResetInstanceCache verifies a reset forces factories to re-run — the
// web launcher calls this after saving config so edited credentials take
// effect without a restart.
func TestResetInstanceCache(t *testing.T) {
	const name = "test_registry_reset_cache"
	factoryCalls := 0
	broker.RegisterFactory(name, func(cfg *config.Config) (broker.Provider, error) {
		factoryCalls++
		return NewMockProvider(name), nil
	})

	if _, err := broker.CreateProvider(name, &config.Config{}); err != nil {
		t.Fatalf("CreateProvider #1: %v", err)
	}
	broker.ResetInstanceCache()
	if _, err := broker.CreateProvider(name, &config.Config{}); err != nil {
		t.Fatalf("CreateProvider #2: %v", err)
	}
	if factoryCalls != 2 {
		t.Fatalf("expected the factory to re-run after ResetInstanceCache, calls=%d", factoryCalls)
	}
}
