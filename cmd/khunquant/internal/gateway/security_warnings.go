package gateway

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// openPermissionAccounts returns "provider/account" for every configured
// exchange account whose Permissions list is empty.
//
// An empty list means "all scopes" (config.ExchangeAccount.HasPermission), which
// keeps older configs working but also means an account nobody restricted can
// trade and transfer. It covers every provider that broker.CheckPermission
// resolves accounts for: binance, binanceth, bitkub, okx, webull and settrade.
// Accounts are listed whether or not their exchange is enabled, because
// CheckPermission does not consult the enabled flag either.
//
// Unnamed accounts are reported under their positional name ("1", "2", ...),
// the same name the tools and the permission resolver use for them.
func openPermissionAccounts(cfg *config.Config) []string {
	var open []string

	add := func(provider string, accounts []config.ExchangeAccount) {
		for i, acc := range accounts {
			if len(acc.Permissions) != 0 {
				continue
			}
			name := acc.Name
			if name == "" {
				name = fmt.Sprintf("%d", i+1)
			}
			open = append(open, provider+"/"+name)
		}
	}

	ex := cfg.Exchanges
	add("binance", ex.Binance.Accounts)
	add("binanceth", ex.BinanceTH.Accounts)
	add("bitkub", ex.Bitkub.Accounts)

	okx := make([]config.ExchangeAccount, 0, len(ex.OKX.Accounts))
	for _, a := range ex.OKX.Accounts {
		okx = append(okx, a.ExchangeAccount)
	}
	add("okx", okx)

	webull := make([]config.ExchangeAccount, 0, len(ex.Webull.Accounts))
	for _, a := range ex.Webull.Accounts {
		webull = append(webull, a.ExchangeAccount)
	}
	add("webull", webull)

	settrade := make([]config.ExchangeAccount, 0, len(ex.Settrade.Accounts))
	for _, a := range ex.Settrade.Accounts {
		settrade = append(settrade, a.ExchangeAccount)
	}
	add("settrade", settrade)

	return open
}

// warnOpenPermissions logs one SECURITY warning per exchange account that has no
// explicit permissions, and returns the warning messages so a caller or test can
// inspect them. Called once at gateway start-up; it changes no behaviour.
func warnOpenPermissions(cfg *config.Config) []string {
	var warnings []string
	for _, ref := range openPermissionAccounts(cfg) {
		msg := fmt.Sprintf("account %s has no explicit permissions; all scopes granted", ref)
		warnings = append(warnings, msg)
		logger.WarnCF("gateway", "SECURITY: "+msg, map[string]any{
			"account": ref,
			"hint": "Set permissions on the account (e.g. [\"market_data\"] for read-only, " +
				"[\"market_data\",\"trade\"] to allow orders) to restrict what the agent may do with it.",
		})
	}
	return warnings
}
