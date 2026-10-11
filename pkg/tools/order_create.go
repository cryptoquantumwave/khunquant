package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const positionWarnThresholdUSD = 10_000

// CreateOrderTool submits an order through 7 sequential safety gates.
type CreateOrderTool struct {
	cfg *config.Config
}

func NewCreateOrderTool(cfg *config.Config) *CreateOrderTool {
	return &CreateOrderTool{cfg: cfg}
}

func (t *CreateOrderTool) Name() string { return NameCreateOrder }

func (t *CreateOrderTool) Description() string {
	return "Place a new order on an exchange. Runs 7 safety checks before executing: rate limit, market status, balance preflight, and position size warning. Requires explicit confirmation for live orders."
}

func (t *CreateOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":  map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":   map[string]any{"type": "string", "description": "Trading pair (e.g. 'BTC/USDT')."},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"limit", "market", "stop_loss", "take_profit"},
				"description": "Order type.",
			},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Order side.",
			},
			"amount": map[string]any{"type": "number", "description": "Order quantity in base currency units."},
			"price":  map[string]any{"type": "number", "description": "Limit price (required for limit orders)."},
			"params": map[string]any{
				"type": "object",
				"description": "Extra provider-specific execution parameters (optional), e.g. clientOrderId, timeInForce, postOnly, reduceOnly, stopPrice. " +
					"Parameters that change the trading mode (tdMode, marginMode, leverage, borrow flags) are refused unless trading_risk permits margin or leverage; unrecognised keys are refused.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to actually place the order. Use false to dry-run the safety checks only.",
			},
		},
		"required": []string{"provider", "symbol", "type", "side", "amount", "confirm"},
	}
}

func (t *CreateOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol, _ := args["symbol"].(string)
	orderType, _ := args["type"].(string)
	side, _ := args["side"].(string)
	amount, _ := args["amount"].(float64)
	confirm, _ := args["confirm"].(bool)

	var price *float64
	if v, ok := args["price"].(float64); ok {
		price = &v
	}

	// H-5: exchange parameters are not inert - {"tdMode":"cross"} turns a spot
	// order into a margin one - so they are filtered against the risk flags
	// rather than forwarded verbatim.
	params, paramsErr := sanitizeOrderParams(args["params"], t.cfg.TradingRisk)
	if paramsErr != nil {
		return ErrorResult(paramsErr.Error())
	}

	// --- Gate 0: paper trading mode redirect ---
	if t.cfg.TradingRisk.PaperTradingMode {
		// Force all orders through simulation — reuse PaperTradeTool logic
		paper := NewPaperTradeTool(t.cfg)
		return paper.Execute(ctx, args)
	}

	// --- Gate 1: basic validation ---
	if providerID == "" || symbol == "" || orderType == "" || side == "" {
		return ErrorResult("provider, symbol, type, and side are required")
	}
	// M-7: NaN defeats an ordinary "<= 0" guard, because every comparison
	// against it is false. It arrives straight from JSON tool arguments.
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return ErrorResult("amount must be a positive, finite number")
	}
	if price != nil && (math.IsNaN(*price) || math.IsInf(*price, 0) || *price <= 0) {
		return ErrorResult("price must be a positive, finite number")
	}
	if orderType == "limit" && price == nil {
		return ErrorResult("price is required for limit orders")
	}

	// --- Gate 1b: permission check ---
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	// --- Gate 1c: risk controls ---
	// Order-type guards need no price and run here. The notional ceiling is
	// measured in USD: a USD-like quote with an explicit price is checked now,
	// with no network call. Any other quote (THB...) needs a rate from the
	// provider, and a market order needs a price, so both are measured at
	// Gate 3b once the provider exists.
	if err := broker.CheckOrderType(t.cfg, orderType); err != nil {
		return ErrorResult(err.Error())
	}
	if price != nil && exchanges.USDLike(broker.SymbolQuote(symbol, providerID)) {
		if err := broker.CheckRiskNotional(t.cfg, amount*(*price)); err != nil {
			return ErrorResult(err.Error())
		}
	}

	// --- Gate 1d: daily loss limit ---
	// Buys, sells of a contract symbol (a ":" in the unified symbol, where a
	// sell can open a short) and sells carrying margin / borrow / leverage
	// params (which can open or enlarge a short on a spot symbol) add risk. A
	// plain spot sell only reduces exposure, so a tripped limit must never stop
	// the user getting out.
	if !strings.EqualFold(side, "sell") || strings.Contains(symbol, ":") || orderParamsAddRisk(params) {
		if err := broker.GlobalLossTracker.CheckDailyLossFor(t.cfg.TradingRisk.DailyLossLimitUSD, broker.AccountKey(t.cfg, providerID, account)); err != nil {
			return ErrorResult(err.Error())
		}
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	tp, ok := p.(broker.TradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support order execution", providerID))
	}

	// --- Gate 3: market status ---
	status, err := p.GetMarketStatus(ctx, symbol)
	if err == nil && status == broker.MarketClosed {
		return ErrorResult(fmt.Sprintf("market %s is currently closed", symbol))
	}

	// --- Gate 3b: price the order ---
	//
	// H-3: a market order carries no price, and every downstream check that
	// needed one simply skipped itself - max_order_value_usd never applied to
	// a market order, and the balance preflight below compared a base-currency
	// amount against a quote-currency balance. Resolve the price once here
	// (ask for a buy, bid for a sell, last as the fallback) and use it for both.
	//
	// The ceiling is in USD, but amount*price is in the symbol's quote
	// currency, so a 30,000 THB order used to be compared as "30,000". With a
	// ceiling set the notional is converted to USD, and an order whose USD
	// value cannot be determined is refused (fail closed). The conversion
	// reads the provider's public ticker; it does not touch the rate limiter.
	md, _ := p.(broker.MarketDataProvider)
	var nq broker.NotionalQuote
	priced := false
	if maxUSD := t.cfg.TradingRisk.MaxOrderValueUSD; maxUSD > 0 {
		est, estErr := broker.EstimateNotionalUSDWithFallback(ctx, t.cfg, md, symbol, side, amount, price)
		if estErr != nil {
			return ErrorResult(unmeasurableOrderMessage(symbol, providerID, estErr)).WithError(estErr)
		}
		if err := broker.CheckRiskNotional(t.cfg, est.USD); err != nil {
			return ErrorResult(err.Error())
		}
		nq, priced = est, true
	} else if est, estErr := broker.EstimateNotionalQuote(ctx, md, symbol, side, amount, price); estErr == nil {
		// No ceiling is configured, so no USD rate is fetched: a non-USD quote
		// keeps the quote-currency notional (est.USDKnown is false), which is
		// what the position warning below has always compared. The only
		// network call is the ticker an unpriced order already needed.
		nq, priced = est, true
	}

	// --- Gate 4: balance preflight (for buy orders) ---
	// Only when the order could be priced: needed is the QUOTE-currency notional
	// compared against the QUOTE balance. Without a price the base amount is
	// not comparable to that balance, so the preflight is skipped rather than
	// comparing units of two different assets.
	if side == "buy" && priced {
		if pp, ok := p.(broker.PortfolioProvider); ok && strings.Contains(symbol, "/") && nq.Quote != "" {
			if bals, err := pp.GetBalances(ctx); err == nil {
				var quoteBalance float64
				for _, b := range bals {
					if strings.EqualFold(b.Asset, nq.Quote) {
						quoteBalance = b.Free
						break
					}
				}
				if quoteBalance < nq.QuoteNotional {
					return ErrorResult(fmt.Sprintf("insufficient %s balance: have %.8g, need %.8g", nq.Quote, quoteBalance, nq.QuoteNotional)).
						WithError(broker.ErrInsufficientFunds)
				}
			}
		}
	}

	// --- Gate 5: position size warning ---
	if priced {
		if nq.USDKnown {
			if nq.USD > positionWarnThresholdUSD && !confirm {
				return ErrorResult(fmt.Sprintf("large order warning: notional value %.2f exceeds %.0f USD. Set confirm=true to proceed.", nq.USD, float64(positionWarnThresholdUSD)))
			}
		} else if nq.QuoteNotional > positionWarnThresholdUSD && !confirm {
			return ErrorResult(fmt.Sprintf("large order warning: notional value %.2f %s (not converted to USD) exceeds %.0f. Set confirm=true to proceed.", nq.QuoteNotional, nq.Quote, float64(positionWarnThresholdUSD)))
		}
	}

	// --- Gate 6: confirmation required ---
	if !confirm {
		out := fmt.Sprintf("Dry-run: all safety checks passed for %s %s %s x%.8g", side, orderType, symbol, amount)
		if price != nil {
			out += fmt.Sprintf(" @ %.8g", *price)
		}
		out += fmt.Sprintf(" on %s.", providerID)
		if note := usdEstimateNote(nq, providerID); note != "" {
			out += " Estimated order value " + note + "."
		}
		out += " Set confirm=true to place the order."
		return UserResult(out)
	}

	// --- Gate 7: rate limit, then execute ---
	// Taken only now, after the dry-run and large-order returns, so a preview spends nothing.
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q — try again in a minute", providerID)).
			WithError(broker.ErrRateLimited)
	}
	order, err := tp.CreateOrder(ctx, symbol, orderType, side, amount, price, params)
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("CreateOrder failed: %v", err)).WithError(err)
	}

	id := "-"
	if order.Id != nil {
		id = *order.Id
	}
	orderStatus := "-"
	if order.Status != nil {
		orderStatus = *order.Status
	}
	out := fmt.Sprintf("Order placed on %s:\n  ID:     %s\n  Symbol: %s\n  Side:   %s\n  Type:   %s\n  Amount: %.8g\n  Status: %s\n",
		providerID, id, symbol, side, orderType, amount, orderStatus)
	if price != nil {
		out += fmt.Sprintf("  Price:  %.8g\n", *price)
	}
	if note := usdEstimateNote(nq, providerID); note != "" {
		out += "  Value:  " + note + "\n"
	}

	return UserResult(out)
}

// unmeasurableOrderMessage explains why an order could not be valued in USD
// while max_order_value_usd is set. The order is refused rather than placed
// unchecked.
func unmeasurableOrderMessage(symbol, providerID string, err error) string {
	cause := "price"
	if errors.Is(err, broker.ErrNoUSDRate) {
		cause = "USD conversion rate"
	}
	return fmt.Sprintf(
		"cannot determine the %s of %s on %s to enforce max_order_value_usd (%v) — refusing the order rather than placing it unchecked",
		cause, symbol, providerID, err)
}

// usdEstimateNote renders the USD value of an order whose quote currency is not
// USD-like, e.g. "5000 THB ≈ $138.89 USD at 36 THB per USD", naming the
// providing exchange when the rate did not come from the order's own provider.
// It is empty when the order is already in USD or no rate was fetched.
func usdEstimateNote(nq broker.NotionalQuote, providerID string) string {
	if !nq.USDKnown || nq.USDPerQuote <= 0 || exchanges.USDLike(nq.Quote) {
		return ""
	}
	note := fmt.Sprintf("%.8g %s ≈ $%.2f USD at %.4g %s per USD", nq.QuoteNotional, nq.Quote, nq.USD, 1/nq.USDPerQuote, nq.Quote)
	if nq.RateProvider != "" && !strings.EqualFold(nq.RateProvider, providerID) {
		note += " (rate from " + nq.RateProvider + ")"
	}
	return note
}
