package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
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
	// Order-type guards need no price and run here. The notional ceiling runs
	// at Gate 3b, once a market order has a price to be measured against.
	if err := broker.CheckOrderType(t.cfg, orderType); err != nil {
		return ErrorResult(err.Error())
	}
	if price != nil {
		if err := broker.CheckRiskNotional(t.cfg, amount*(*price)); err != nil {
			return ErrorResult(err.Error())
		}
	}

	// --- Gate 1d: daily loss limit ---
	if err := broker.GlobalLossTracker.CheckDailyLoss(t.cfg.TradingRisk.DailyLossLimitUSD); err != nil {
		return ErrorResult(err.Error())
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
	// amount against a quote-currency balance ("need 2 BTC, have 5000 USDT"
	// passes). Resolve the live price once here and use it for both.
	effectivePrice := price
	if effectivePrice == nil {
		if md, ok := p.(broker.MarketDataProvider); ok {
			if ticker, err := md.FetchTicker(ctx, symbol); err == nil && ticker.Last != nil {
				if last := *ticker.Last; last > 0 && !math.IsNaN(last) && !math.IsInf(last, 0) {
					effectivePrice = &last
				}
			}
		}
	}
	if effectivePrice != nil {
		if err := broker.CheckRiskNotional(t.cfg, amount*(*effectivePrice)); err != nil {
			return ErrorResult(err.Error())
		}
	} else if t.cfg.TradingRisk.MaxOrderValueUSD > 0 {
		return ErrorResult(fmt.Sprintf(
			"cannot price %s on %s to enforce max_order_value_usd — refusing the order rather than placing it unchecked",
			symbol, providerID))
	}

	// --- Gate 4: balance preflight (for buy orders) ---
	if side == "buy" {
		if pp, ok := p.(broker.PortfolioProvider); ok {
			parts := strings.SplitN(symbol, "/", 2)
			if len(parts) == 2 {
				quoteCurrency := parts[1]
				bals, err := pp.GetBalances(ctx)
				if err == nil {
					var quoteBalance float64
					for _, b := range bals {
						if strings.EqualFold(b.Asset, quoteCurrency) {
							quoteBalance = b.Free
							break
						}
					}
					needed := amount
					if effectivePrice != nil {
						needed = amount * (*effectivePrice)
					}
					if quoteBalance < needed {
						return ErrorResult(fmt.Sprintf("insufficient %s balance: have %.8g, need %.8g", quoteCurrency, quoteBalance, needed)).
							WithError(broker.ErrInsufficientFunds)
					}
				}
			}
		}
	}

	// --- Gate 5: position size warning ---
	if effectivePrice != nil {
		notional := amount * (*effectivePrice)
		if notional > positionWarnThresholdUSD {
			if !confirm {
				return ErrorResult(fmt.Sprintf("large order warning: notional value %.2f exceeds %.0f USD. Set confirm=true to proceed.", notional, float64(positionWarnThresholdUSD)))
			}
		}
	}

	// --- Gate 6: confirmation required ---
	if !confirm {
		out := fmt.Sprintf("Dry-run: all safety checks passed for %s %s %s x%.8g", side, orderType, symbol, amount)
		if price != nil {
			out += fmt.Sprintf(" @ %.8g", *price)
		}
		out += fmt.Sprintf(" on %s. Set confirm=true to place the order.", providerID)
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

	return UserResult(out)
}
