package exchanges_test

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// mockExchange is a minimal Exchange implementation for testing the registry.
type mockExchange struct{ name string }

func (m *mockExchange) Name() string                                               { return m.name }
func (m *mockExchange) GetBalances(_ context.Context) ([]exchanges.Balance, error) { return nil, nil }

func TestRegisterFactory_Roundtrip(t *testing.T) {
	const exName = "test_registry_exchange_1"
	exchanges.RegisterFactory(exName, func(_ *config.Config) (exchanges.Exchange, error) {
		return &mockExchange{name: exName}, nil
	})

	ex, err := exchanges.CreateExchange(exName, &config.Config{})
	if err != nil {
		t.Fatalf("CreateExchange: %v", err)
	}
	if ex.Name() != exName {
		t.Errorf("Name: want %q got %q", exName, ex.Name())
	}
}

func TestCreateExchange_NotRegistered(t *testing.T) {
	_, err := exchanges.CreateExchange("definitely_not_registered_xyz", &config.Config{})
	if err == nil {
		t.Fatal("expected error for unregistered exchange, got nil")
	}
}

func TestRegisterAccountFactory_Roundtrip(t *testing.T) {
	const exName = "test_registry_exchange_2"
	exchanges.RegisterAccountFactory(exName, func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		return &mockExchange{name: accountName}, nil
	})

	ex, err := exchanges.CreateExchangeForAccount(exName, "my_account", &config.Config{})
	if err != nil {
		t.Fatalf("CreateExchangeForAccount: %v", err)
	}
	if ex.Name() != "my_account" {
		t.Errorf("Name: want %q got %q", "my_account", ex.Name())
	}
}

func TestCreateExchangeForAccount_FallsBackToFactory(t *testing.T) {
	// Register only a basic factory (no account factory) for this name.
	const exName = "test_registry_exchange_3"
	exchanges.RegisterFactory(exName, func(_ *config.Config) (exchanges.Exchange, error) {
		return &mockExchange{name: exName}, nil
	})

	// CreateExchangeForAccount should fall back to the basic factory.
	ex, err := exchanges.CreateExchangeForAccount(exName, "any_account", &config.Config{})
	if err != nil {
		t.Fatalf("CreateExchangeForAccount fallback: %v", err)
	}
	if ex.Name() != exName {
		t.Errorf("Name: want %q got %q", exName, ex.Name())
	}
}

func TestErrAccountNotFound_Format(t *testing.T) {
	err := exchanges.ErrAccountNotFound("bitkub", "missing", []string{"main", "savings"})
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	msg := err.Error()
	if msg == "" {
		t.Error("expected non-empty error message")
	}
	// Should contain the exchange name, account name, and available list.
	for _, want := range []string{"bitkub", "missing", "main", "savings"} {
		if !containsStr(msg, want) {
			t.Errorf("error message %q does not contain %q", msg, want)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := range s {
				if i+len(sub) <= len(s) && s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
