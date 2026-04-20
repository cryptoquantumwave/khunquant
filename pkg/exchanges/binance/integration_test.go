package binance_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	_ "github.com/cryptoquantumwave/khunquant/pkg/exchanges/binance"
)

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	home, _ := os.UserHomeDir()
	cfg, err := config.LoadConfig(home + "/.khunquant/config.json")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, ok := cfg.Exchanges.Binance.ResolveAccount(""); !ok {
		t.Skip("binance credentials not configured, skipping")
	}
	t.Logf("exchange enabled: %v", cfg.Exchanges.Binance.Enabled)
	t.Logf("testnet:          %v", cfg.Exchanges.Binance.Testnet)
	return cfg
}

// TestBinanceGetBalances_Integration tests the basic Exchange interface (spot).
func TestBinanceGetBalances_Integration(t *testing.T) {
	cfg := loadConfig(t)

	ex, err := exchanges.CreateExchange("binance", cfg)
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}

	balances, err := ex.GetBalances(context.Background())
	if err != nil {
		t.Fatalf("get balances: %v", err)
	}

	if len(balances) == 0 {
		t.Log("OK: connected — no non-zero spot balances found")
		return
	}
	t.Logf("OK: %d spot asset(s) with non-zero balance:", len(balances))
	for _, b := range balances {
		t.Log(fmt.Sprintf("  %-10s  free=%.8f  locked=%.8f", b.Asset, b.Free, b.Locked))
	}
}

// TestBinanceGetWalletBalances_Integration tests each wallet type via WalletExchange.
func TestBinanceGetWalletBalances_Integration(t *testing.T) {
	cfg := loadConfig(t)

	ex, err := exchanges.CreateExchange("binance", cfg)
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}

	we, ok := ex.(exchanges.WalletExchange)
	if !ok {
		t.Fatal("exchange does not implement WalletExchange")
	}

	for _, wt := range we.SupportedWalletTypes() {
		if wt == "all" || wt == "earn" {
			continue // tested separately
		}
		t.Run(wt, func(t *testing.T) {
			balances, err := we.GetWalletBalances(context.Background(), wt)
			if err != nil {
				t.Logf("SKIP: %v (wallet may not be enabled)", err)
				return
			}
			if len(balances) == 0 {
				t.Logf("OK: no non-zero balances in %s wallet", wt)
				return
			}
			t.Logf("OK: %d asset(s) in %s wallet:", len(balances), wt)
			for _, b := range balances {
				line := fmt.Sprintf("  %-10s  free=%.8f  locked=%.8f", b.Asset, b.Free, b.Locked)
				for k, v := range b.Extra {
					line += fmt.Sprintf("  %s=%s", k, v)
				}
				t.Log(line)
			}
		})
	}
}

// TestBinanceGetAllBalances_Integration tests the "all" aggregate wallet type.
func TestBinanceGetAllBalances_Integration(t *testing.T) {
	cfg := loadConfig(t)

	ex, err := exchanges.CreateExchange("binance", cfg)
	if err != nil {
		t.Fatalf("create exchange: %v", err)
	}

	we, ok := ex.(exchanges.WalletExchange)
	if !ok {
		t.Skip("exchange does not implement WalletExchange")
	}

	balances, err := we.GetWalletBalances(context.Background(), "all")
	if err != nil {
		t.Fatalf("get all balances: %v", err)
	}

	t.Logf("OK: %d total non-zero balance(s) across all wallets:", len(balances))
	for _, b := range balances {
		t.Log(fmt.Sprintf("  [%-14s] %-10s  free=%.8f  locked=%.8f", b.WalletType, b.Asset, b.Free, b.Locked))
	}
}
