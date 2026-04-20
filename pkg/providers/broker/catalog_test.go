package broker_test

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestListConfiguredAccounts_Empty(t *testing.T) {
	cfg := &config.Config{}
	refs := broker.ListConfiguredAccounts(cfg)
	if len(refs) != 0 {
		t.Errorf("expected 0 accounts, got %d", len(refs))
	}
}

func TestListConfiguredAccounts_DisabledExchanges(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  false,
				Accounts: []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("k")}},
			},
		},
	}
	refs := broker.ListConfiguredAccounts(cfg)
	if len(refs) != 0 {
		t.Errorf("expected 0 accounts for disabled exchange, got %d", len(refs))
	}
}

func TestListConfiguredAccounts_SingleBinanceAccount(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main", APIKey: *config.NewSecureString("k")}},
			},
		},
	}
	refs := broker.ListConfiguredAccounts(cfg)
	if len(refs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(refs))
	}
	if refs[0].ProviderID != "binance" {
		t.Errorf("providerID: want %q got %q", "binance", refs[0].ProviderID)
	}
	if refs[0].Account != "main" {
		t.Errorf("account: want %q got %q", "main", refs[0].Account)
	}
}

func TestListConfiguredAccounts_DefaultAccountName(t *testing.T) {
	// When an account has no Name, it should receive positional name "1".
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Bitkub: config.BitkubExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{APIKey: *config.NewSecureString("k")}},
			},
		},
	}
	refs := broker.ListConfiguredAccounts(cfg)
	if len(refs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(refs))
	}
	if refs[0].Account != "1" {
		t.Errorf("expected positional name %q, got %q", "1", refs[0].Account)
	}
}

func TestListConfiguredAccounts_MultipleExchanges(t *testing.T) {
	cfg := &config.Config{
		Exchanges: config.ExchangesConfig{
			Binance: config.BinanceExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "spot"}, {Name: "futures"}},
			},
			Bitkub: config.BitkubExchangeConfig{
				Enabled:  true,
				Accounts: []config.ExchangeAccount{{Name: "main"}},
			},
		},
	}
	refs := broker.ListConfiguredAccounts(cfg)
	if len(refs) != 3 {
		t.Errorf("expected 3 accounts, got %d", len(refs))
	}
}
