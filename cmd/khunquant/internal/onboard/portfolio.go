package onboard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

type exchangeDef struct {
	id    string
	label string
}

var exchangeDefs = []exchangeDef{
	{id: "binance", label: "Binance"},
	{id: "binanceth", label: "Binance TH"},
	{id: "bitkub", label: "Bitkub"},
	{id: "okx", label: "OKX"},
	{id: "settrade", label: "Settrade"},
}

// setupPortfolios prompts the user to enable one or more exchanges, flips
// Enabled=true, and appends a placeholder account named "main" for each
// selected exchange. Returns the IDs of all exchanges that were enabled.
func setupPortfolios(cfg *config.Config) ([]string, error) {
	fmt.Println("\nSet up portfolios (exchange accounts)")
	fmt.Println("---------------------------------------")
	fmt.Println("Available exchanges:")
	for i, ex := range exchangeDefs {
		fmt.Printf("  %d. %s\n", i+1, ex.label)
	}
	fmt.Println("")
	fmt.Print("Select exchanges to enable (e.g. 1,2,4 or 'all', empty to skip and setup later): ")

	var input string
	fmt.Scanln(&input)
	input = strings.TrimSpace(input)
	if input == "" {
		fmt.Println("Skipped portfolio setup.")
		return nil, nil
	}

	selected, err := parseExchangeSelection(input, len(exchangeDefs))
	if err != nil {
		return nil, err
	}

	var enabled []string
	for _, idx := range selected {
		ex := exchangeDefs[idx]
		if err := enableExchange(cfg, ex.id); err != nil {
			return nil, fmt.Errorf("enabling %s: %w", ex.id, err)
		}
		enabled = append(enabled, ex.id)
		fmt.Printf("  Enabled %s (placeholder account 'main' added)\n", ex.label)
	}

	return enabled, nil
}

// parseExchangeSelection parses comma-separated 1-based indices or "all".
func parseExchangeSelection(input string, total int) ([]int, error) {
	if strings.EqualFold(input, "all") {
		indices := make([]int, total)
		for i := range indices {
			indices[i] = i
		}
		return indices, nil
	}

	seen := map[int]bool{}
	var indices []int
	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > total {
			return nil, fmt.Errorf("invalid selection %q (choose 1-%d)", part, total)
		}
		idx := n - 1
		if !seen[idx] {
			seen[idx] = true
			indices = append(indices, idx)
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("no valid exchanges selected")
	}
	return indices, nil
}

// enableExchange sets Enabled=true and adds a placeholder account on the right
// exchange sub-config within cfg.
func enableExchange(cfg *config.Config, id string) error {
	placeholder := config.ExchangeAccount{Name: "main"}
	switch id {
	case "binance":
		cfg.Exchanges.Binance.Enabled = true
		cfg.Exchanges.Binance.Accounts = appendIfNoMain(cfg.Exchanges.Binance.Accounts, placeholder)
	case "binanceth":
		cfg.Exchanges.BinanceTH.Enabled = true
		cfg.Exchanges.BinanceTH.Accounts = appendIfNoMain(cfg.Exchanges.BinanceTH.Accounts, placeholder)
	case "bitkub":
		cfg.Exchanges.Bitkub.Enabled = true
		cfg.Exchanges.Bitkub.Accounts = appendIfNoMain(cfg.Exchanges.Bitkub.Accounts, placeholder)
	case "okx":
		cfg.Exchanges.OKX.Enabled = true
		cfg.Exchanges.OKX.Accounts = appendIfNoMainOKX(cfg.Exchanges.OKX.Accounts, config.OKXExchangeAccount{ExchangeAccount: placeholder})
	case "settrade":
		cfg.Exchanges.Settrade.Enabled = true
		cfg.Exchanges.Settrade.Accounts = appendIfNoMainSettrade(cfg.Exchanges.Settrade.Accounts, config.SettradeExchangeAccount{ExchangeAccount: placeholder})
	default:
		return fmt.Errorf("unknown exchange %q", id)
	}
	return nil
}

func appendIfNoMain(accounts []config.ExchangeAccount, placeholder config.ExchangeAccount) []config.ExchangeAccount {
	for _, a := range accounts {
		if strings.EqualFold(a.Name, "main") || a.Name == "" {
			return accounts
		}
	}
	return append(accounts, placeholder)
}

func appendIfNoMainOKX(accounts []config.OKXExchangeAccount, placeholder config.OKXExchangeAccount) []config.OKXExchangeAccount {
	for _, a := range accounts {
		if strings.EqualFold(a.Name, "main") || a.Name == "" {
			return accounts
		}
	}
	return append(accounts, placeholder)
}

func appendIfNoMainSettrade(accounts []config.SettradeExchangeAccount, placeholder config.SettradeExchangeAccount) []config.SettradeExchangeAccount {
	for _, a := range accounts {
		if strings.EqualFold(a.Name, "main") || a.Name == "" {
			return accounts
		}
	}
	return append(accounts, placeholder)
}
