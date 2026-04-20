package broker

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ListConfiguredAccounts returns all enabled provider/account pairs from config.
// It reads from both the legacy "exchanges" key and the new "brokers" key,
// deduplicating by (providerID, account) pair.
func ListConfiguredAccounts(cfg *config.Config) []AccountRef {
	var refs []AccountRef

	ex := cfg.Exchanges
	if ex.Binance.Enabled {
		for i, acc := range ex.Binance.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, AccountRef{ProviderID: "binance", Account: name})
		}
	}
	if ex.BinanceTH.Enabled {
		for i, acc := range ex.BinanceTH.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, AccountRef{ProviderID: "binanceth", Account: name})
		}
	}
	if ex.Bitkub.Enabled {
		for i, acc := range ex.Bitkub.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, AccountRef{ProviderID: "bitkub", Account: name})
		}
	}
	if ex.OKX.Enabled {
		for i, acc := range ex.OKX.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, AccountRef{ProviderID: "okx", Account: name})
		}
	}
	if ex.Settrade.Enabled {
		for i, acc := range ex.Settrade.Accounts {
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			refs = append(refs, AccountRef{ProviderID: "settrade", Account: name})
		}
	}

	return refs
}
