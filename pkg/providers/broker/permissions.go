package broker

import (
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// CheckPermission looks up the named account in cfg and verifies it has the
// requested scope. Returns a descriptive error if denied.
//
// Usage in tools (before calling TradingProvider):
//
//	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
//	    return tools.ErrorResult(err.Error())
//	}
func CheckPermission(cfg *config.Config, providerID, accountName string, scope config.PermissionScope) error {
	acc, ok := resolveExchangeAccount(cfg, providerID, accountName)
	if !ok {
		// Account not found — let downstream provider return the real error.
		return nil
	}
	if !acc.HasPermission(scope) {
		return fmt.Errorf("account %q on provider %q does not have %q permission — add it to config.exchanges.%s.accounts[].permissions",
			accountName, providerID, scope, strings.ToLower(providerID))
	}
	return nil
}

// resolveExchangeAccount finds the ExchangeAccount for a given provider+name.
// Returns (zero, false) when the account is not found.
func resolveExchangeAccount(cfg *config.Config, providerID, accountName string) (config.ExchangeAccount, bool) {
	ex := cfg.Exchanges
	switch strings.ToLower(providerID) {
	case "binance":
		acc, ok := ex.Binance.ResolveAccount(accountName)
		return acc, ok
	case "binanceth":
		acc, ok := ex.BinanceTH.ResolveAccount(accountName)
		return acc, ok
	case "bitkub":
		acc, ok := ex.Bitkub.ResolveAccount(accountName)
		return acc, ok
	case "okx":
		acc, _ := ex.OKX.ResolveAccount(accountName)
		// OKXExchangeAccount embeds ExchangeAccount — return it directly
		return config.ExchangeAccount{
			Name:        acc.Name,
			APIKey:      acc.APIKey,
			Secret:      acc.Secret,
			Permissions: acc.Permissions,
		}, acc.APIKey.String() != ""
	}
	return config.ExchangeAccount{}, false
}

// CheckRisk validates an order against the TradingRiskConfig.
// Returns an error if the order violates any risk control.
func CheckRisk(cfg *config.Config, side, orderType string, amount float64, price *float64) error {
	risk := cfg.TradingRisk

	// Margin / leverage guard
	if !risk.AllowMargin && isMarginOrderType(orderType) {
		return fmt.Errorf("margin order type %q is disabled — set trading_risk.allow_margin=true to enable", orderType)
	}

	if price == nil {
		return nil
	}

	notional := amount * (*price)

	// Max single order value
	if risk.MaxOrderValueUSD > 0 && notional > risk.MaxOrderValueUSD {
		return fmt.Errorf("order notional %.2f USD exceeds configured max_order_value_usd %.2f", notional, risk.MaxOrderValueUSD)
	}

	// Daily loss limit is tracked externally; this just checks config is valid
	// (actual accumulation tracked by DailyLossTracker in the tool layer).
	_ = risk.DailyLossLimitUSD

	return nil
}

func isMarginOrderType(t string) bool {
	switch strings.ToLower(t) {
	case "margin", "margin_market", "margin_limit":
		return true
	}
	return false
}
