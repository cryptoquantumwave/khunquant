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
