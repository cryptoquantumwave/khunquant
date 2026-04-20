package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// ListPortfoliosTool lists all enabled exchange accounts from config.
type ListPortfoliosTool struct {
	cfg *config.Config
}

// NewListPortfoliosTool creates a new ListPortfoliosTool.
func NewListPortfoliosTool(cfg *config.Config) *ListPortfoliosTool {
	return &ListPortfoliosTool{cfg: cfg}
}

func (t *ListPortfoliosTool) Name() string {
	return NameListPortfolios
}

func (t *ListPortfoliosTool) Description() string {
	return "List all available portfolio accounts (exchange + account name pairs) that are enabled and have credentials configured. Call this first to discover what exchange accounts are available before using get_assets_list or get_total_value."
}

func (t *ListPortfoliosTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
		"required":   []string{},
	}
}

func (t *ListPortfoliosTool) Execute(_ context.Context, _ map[string]any) *ToolResult {
	type row struct {
		exchange    string
		account     string
		walletTypes string
		canPrice    string
	}

	var rows []row

	ex := t.cfg.Exchanges

	if ex.Binance.Enabled {
		for i, acc := range ex.Binance.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			rows = append(rows, row{exchange: "binance", account: name})
		}
	}

	if ex.BinanceTH.Enabled {
		for i, acc := range ex.BinanceTH.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			rows = append(rows, row{exchange: "binanceth", account: name})
		}
	}

	if ex.Bitkub.Enabled {
		for i, acc := range ex.Bitkub.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			rows = append(rows, row{exchange: "bitkub", account: name})
		}
	}

	if ex.OKX.Enabled {
		for i, acc := range ex.OKX.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			rows = append(rows, row{exchange: "okx", account: name})
		}
	}

	if ex.Settrade.Enabled {
		for i, acc := range ex.Settrade.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			rows = append(rows, row{exchange: "settrade", account: name})
		}
	}

	if len(rows) == 0 {
		return UserResult("No exchange accounts are configured. Enable an exchange and add credentials to get started.")
	}

	// Populate capabilities by creating exchange instances.
	for i := range rows {
		inst, err := exchanges.CreateExchangeForAccount(rows[i].exchange, rows[i].account, t.cfg)
		if err != nil {
			rows[i].walletTypes = "?"
			rows[i].canPrice = "?"
			continue
		}
		if we, ok := inst.(exchanges.WalletExchange); ok {
			rows[i].walletTypes = strings.Join(we.SupportedWalletTypes(), ",")
		} else {
			rows[i].walletTypes = "spot"
		}
		if _, ok := inst.(exchanges.PricedExchange); ok {
			rows[i].canPrice = "yes"
		} else {
			rows[i].canPrice = "no"
		}
	}

	// Compute max walletTypes column width for alignment.
	walletsWidth := len("Wallets")
	for _, r := range rows {
		if len(r.walletTypes) > walletsWidth {
			walletsWidth = len(r.walletTypes)
		}
	}

	var sb strings.Builder
	sb.WriteString("Available portfolios (exchange accounts with credentials):\n\n")
	fmtStr := fmt.Sprintf("%%-12s  %%-10s  %%-%ds  %%s\n", walletsWidth)
	sb.WriteString(fmt.Sprintf(fmtStr, "Exchange", "Account", "Wallets", "Pricing"))
	dashW := strings.Repeat("-", walletsWidth)
	sb.WriteString(fmt.Sprintf(fmtStr, "----------", "----------", dashW, "-------"))
	for _, r := range rows {
		sb.WriteString(fmt.Sprintf(fmtStr, r.exchange, r.account, r.walletTypes, r.canPrice))
	}
	return UserResult(sb.String())
}
