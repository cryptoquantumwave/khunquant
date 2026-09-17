package broker

import (
	"fmt"
	"math"
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
		// H-4: this used to return nil for every unresolved account, so a
		// permission check on a misspelt provider or account name passed
		// silently and the order went out unchecked. The scopes that move
		// money now fail closed.
		//
		// Scoped to providers that have accounts configured but none matching.
		// That is the case this protects: a named account whose permissions
		// were meant to restrict it, skipped because the name did not match.
		//
		// When a provider has *no* accounts configured, nothing can
		// authenticate - the default factory yields a public, credential-less
		// adapter and a real order fails at the exchange - so refusing here
		// would only break dry-runs and previews without preventing a trade.
		// Unknown providers likewise fall through to CreateProviderForAccount,
		// which returns the real "not registered" error.
		if scopeMovesFunds(scope) && configuredAccountCount(cfg, providerID) > 0 {
			return fmt.Errorf("account %q on provider %q could not be resolved — cannot verify %q permission; check config.exchanges.%s.accounts[]",
				accountName, providerID, scope, strings.ToLower(providerID))
		}
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
		// Resolution is the resolver's own answer, not "does it carry a
		// static API key". OKX and Webull accounts can authenticate without
		// one (Webull holds a session), and treating a keyless account as
		// unresolved meant its configured permissions were never consulted.
		acc, ok := ex.OKX.ResolveAccount(accountName)
		return acc.ExchangeAccount, ok
	case "webull":
		acc, ok := ex.Webull.ResolveAccount(accountName)
		return acc.ExchangeAccount, ok
	case "settrade":
		acc, ok := ex.Settrade.ResolveAccount(accountName)
		return acc.ExchangeAccount, ok
	}
	return config.ExchangeAccount{}, false
}

// configuredAccountCount returns how many accounts providerID has configured,
// and 0 for a provider that keeps no accounts section at all.
//
// MT5 is deliberately absent: MT5ExchangeConfig is still a TODO in
// pkg/config/config.go, it is not a field of ExchangesConfig, and its
// RegisterAccountFactory call is commented out, so there is nothing to resolve
// against and no way to reach it through CreateProviderForAccount.
func configuredAccountCount(cfg *config.Config, providerID string) int {
	ex := cfg.Exchanges
	switch strings.ToLower(providerID) {
	case "binance":
		return len(ex.Binance.Accounts)
	case "binanceth":
		return len(ex.BinanceTH.Accounts)
	case "bitkub":
		return len(ex.Bitkub.Accounts)
	case "okx":
		return len(ex.OKX.Accounts)
	case "webull":
		return len(ex.Webull.Accounts)
	case "settrade":
		return len(ex.Settrade.Accounts)
	}
	return 0
}

// scopeMovesFunds reports whether a scope authorises something that moves money,
// which is where an unverifiable permission has to be refused rather than
// assumed.
func scopeMovesFunds(scope config.PermissionScope) bool {
	return scope == config.ScopeTrade || scope == config.ScopeTransfer
}

// CheckOrderType applies the order-type guards that need no price: today, the
// margin/leverage gate. Split out of CheckRisk so a caller that has to fetch a
// market price first can still refuse a forbidden order type up front.
func CheckOrderType(cfg *config.Config, orderType string) error {
	if !cfg.TradingRisk.AllowMargin && isMarginOrderType(orderType) {
		return fmt.Errorf("margin order type %q is disabled — set trading_risk.allow_margin=true to enable", orderType)
	}
	return nil
}

// CheckRiskNotional validates an order's USD notional against the configured
// ceiling.
//
// M-7: NaN and Inf are rejected explicitly. They arrive from JSON tool
// arguments and defeat every ordinary bound check, because comparisons against
// NaN are false - "notional > max" does not fire, and a `<= 0` guard passes it.
func CheckRiskNotional(cfg *config.Config, notionalUSD float64) error {
	if math.IsNaN(notionalUSD) || math.IsInf(notionalUSD, 0) {
		return fmt.Errorf("order notional is not a finite number (%v)", notionalUSD)
	}

	max := cfg.TradingRisk.MaxOrderValueUSD
	if max > 0 && notionalUSD > max {
		return fmt.Errorf("order notional %.2f USD exceeds configured max_order_value_usd %.2f", notionalUSD, max)
	}
	return nil
}

// CheckRisk validates an order against the TradingRiskConfig.
// Returns an error if the order violates any risk control.
func CheckRisk(cfg *config.Config, side, orderType string, amount float64, price *float64) error {
	if err := CheckOrderType(cfg, orderType); err != nil {
		return err
	}

	if price == nil {
		// H-3: an unpriced order used to return nil here, so a market order -
		// which carries no price - skipped the notional ceiling entirely and
		// no configured limit applied to it. A caller that cannot supply a
		// price must fetch one (see CreateOrderTool, which resolves a market
		// price from the ticker) rather than be waved through.
		if cfg.TradingRisk.MaxOrderValueUSD > 0 {
			return fmt.Errorf("cannot evaluate max_order_value_usd without a price — supply a price or use CheckRiskNotional with a resolved market price")
		}
		return nil
	}

	if math.IsNaN(amount) || math.IsInf(amount, 0) || math.IsNaN(*price) || math.IsInf(*price, 0) {
		return fmt.Errorf("order amount and price must be finite numbers")
	}

	return CheckRiskNotional(cfg, amount*(*price))
}

// CheckLeverage rejects futures actions when trading_risk.allow_leverage is false.
// Every live futures mutation tool must call this before executing.
func CheckLeverage(cfg *config.Config, action string) error {
	if !cfg.TradingRisk.AllowLeverage {
		return fmt.Errorf("%s requires trading_risk.allow_leverage=true — leverage trading is disabled in this configuration", action)
	}
	return nil
}

func isMarginOrderType(t string) bool {
	switch strings.ToLower(t) {
	case "margin", "margin_market", "margin_limit":
		return true
	}
	return false
}
