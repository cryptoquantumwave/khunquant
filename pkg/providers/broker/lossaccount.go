package broker

import (
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// NormalizeAccountKey builds the canonical "provider/account" key the daily loss
// guard uses, lower-cased so config casing and tool-argument casing agree.
func NormalizeAccountKey(providerID, account string) string {
	return normalizeKey(strings.TrimSpace(providerID) + "/" + strings.TrimSpace(account))
}

// AccountKey is the loss-guard key for the account an order targets. An empty
// account name selects the provider's first configured account (the same rule
// the provider factories apply), so it is resolved here to match the key the
// equity sampler samples under.
func AccountKey(cfg *config.Config, providerID, account string) string {
	if strings.TrimSpace(account) == "" && cfg != nil {
		for _, ref := range ListConfiguredAccounts(cfg) {
			if strings.EqualFold(ref.ProviderID, providerID) {
				return NormalizeAccountKey(ref.ProviderID, ref.Account)
			}
		}
	}
	return NormalizeAccountKey(providerID, account)
}

// LossStatePath is where the daily loss guard persists its state for a
// workspace. The agent loop and the `khunquant risk` command must agree on it.
func LossStatePath(workspace string) string {
	return filepath.Join(workspace, "state", "daily_loss.json")
}
