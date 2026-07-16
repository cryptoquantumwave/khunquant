package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OptionCreateOrderTool submits a single-leg options order through safety gates.
type OptionCreateOrderTool struct {
	cfg *config.Config
}

func NewOptionCreateOrderTool(cfg *config.Config) *OptionCreateOrderTool {
	return &OptionCreateOrderTool{cfg: cfg}
}

func (t *OptionCreateOrderTool) Name() string { return NameOptionCreateOrder }

func (t *OptionCreateOrderTool) Description() string {
	return "Place a single-leg options order. Supports LIMIT, STOP_LOSS, STOP_LOSS_LIMIT order types with DAY or GTC time-in-force (GTC rejected on SELL). Requires explicit confirm=true. Single-leg only; multi-leg spreads not supported."
}

func (t *OptionCreateOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":   map[string]any{"type": "string", "description": "Provider/exchange name."},
			"account":    map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"underlying": map[string]any{"type": "string", "description": "Underlying asset symbol (e.g. 'AAPL')."},
			"expiry":     map[string]any{"type": "string", "description": "Expiration date in yyyy-MM-dd format."},
			"strike":     map[string]any{"type": "number", "description": "Strike price."},
			"option_type": map[string]any{
				"type":        "string",
				"enum":        []string{"CALL", "PUT"},
				"description": "Option type.",
			},
			"side": map[string]any{
				"type":        "string",
				"enum":        []string{"buy", "sell"},
				"description": "Order side.",
			},
			"quantity": map[string]any{"type": "number", "description": "Number of contracts (each contract = 100 shares)."},
			"type": map[string]any{
				"type":        "string",
				"enum":        []string{"limit", "stop_loss", "stop_loss_limit"},
				"description": "Order type (market orders not supported for options).",
			},
			"limit_price": map[string]any{"type": "number", "description": "Limit price per contract (required for limit and stop_loss_limit)."},
			"stop_price":  map[string]any{"type": "number", "description": "Stop price per contract (required for stop_loss and stop_loss_limit)."},
			"time_in_force": map[string]any{
				"type":        "string",
				"enum":        []string{"DAY", "GTC"},
				"description": "Time in force (GTC not allowed on SELL orders).",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Must be true to actually place the order. Use false to dry-run the safety checks only.",
			},
		},
		"required": []string{"provider", "underlying", "expiry", "strike", "option_type", "side", "quantity", "type", "confirm"},
	}
}

func (t *OptionCreateOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	underlying, _ := args["underlying"].(string)
	expiry, _ := args["expiry"].(string)
	strike, _ := args["strike"].(float64)
	optionType, _ := args["option_type"].(string)
	side, _ := args["side"].(string)
	quantity, _ := args["quantity"].(float64)
	orderType, _ := args["type"].(string)
	timeInForce, _ := args["time_in_force"].(string)
	confirm, _ := args["confirm"].(bool)

	var limitPrice *float64
	if v, ok := args["limit_price"].(float64); ok && v > 0 {
		limitPrice = &v
	}

	var stopPrice *float64
	if v, ok := args["stop_price"].(float64); ok && v > 0 {
		stopPrice = &v
	}

	// --- Gate 0: paper trading mode redirect ---
	if t.cfg.TradingRisk.PaperTradingMode {
		paper := NewPaperTradeTool(t.cfg)
		return paper.Execute(ctx, args)
	}

	// --- Gate 1: basic validation ---
	if providerID == "" || underlying == "" || expiry == "" || optionType == "" || side == "" || orderType == "" {
		return ErrorResult("provider, underlying, expiry, option_type, side, and type are required")
	}
	if quantity <= 0 {
		return ErrorResult("quantity must be positive")
	}

	// Normalize for gates and messages.
	optionTypeUpper := strings.ToUpper(optionType)
	sideUpper := strings.ToUpper(side)
	orderTypeUpper := strings.ToUpper(orderType)
	tifUpper := strings.ToUpper(timeInForce)

	// Build the order request and validate its shape once, via the single source
	// of truth (broker.OptionOrderRequest.Validate): order type + required prices,
	// side, TIF (GTC not on SELL), single leg, CALL/PUT. Policy gates below stay
	// in this tool layer.
	leg := broker.OptionLeg{
		Side:       sideUpper,
		Quantity:   quantity,
		Underlying: strings.ToUpper(underlying),
		Strike:     strike,
		Expiry:     expiry,
		OptionType: optionTypeUpper,
	}
	req := broker.OptionOrderRequest{
		Underlying:  strings.ToUpper(underlying),
		Strategy:    "SINGLE",
		OrderType:   orderTypeUpper,
		Side:        sideUpper,
		Quantity:    quantity,
		LimitPrice:  limitPrice,
		StopPrice:   stopPrice,
		TimeInForce: tifUpper,
		Legs:        []broker.OptionLeg{leg},
	}
	if err := req.Validate(); err != nil {
		return ErrorResult(err.Error())
	}

	// --- Gate 1b: permission check ---
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	// --- Gate 1c: risk controls ---
	// For options, calculate notional with 100x multiplier
	notionalPrice := limitPrice
	if notionalPrice == nil {
		notionalPrice = stopPrice
	}
	riskAmount := quantity
	if notionalPrice != nil {
		// notional = limit_price × 100 (contracts) × quantity
		riskAmount = quantity * (*notionalPrice) * 100
	}
	if err := broker.CheckRisk(t.cfg, strings.ToLower(side), strings.ToLower(orderTypeUpper), riskAmount, nil); err != nil {
		return ErrorResult(err.Error())
	}

	// --- Gate 1d: daily loss limit ---
	if err := broker.GlobalLossTracker.CheckDailyLoss(t.cfg.TradingRisk.DailyLossLimitUSD); err != nil {
		return ErrorResult(err.Error())
	}

	// --- Gate 2: rate limit ---
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q — try again in a minute", providerID)).
			WithError(broker.ErrRateLimited)
	}

	p, err := broker.CreateProviderForAccount(providerID, account, t.cfg)
	if err != nil {
		return ErrorResult(fmt.Sprintf("provider %q: %v", providerID, err))
	}

	opt, ok := p.(broker.OptionTradingProvider)
	if !ok {
		return ErrorResult(fmt.Sprintf("provider %q does not support option order execution", providerID))
	}

	// --- Gate 3: market status ---
	status, err := p.GetMarketStatus(ctx, underlying)
	if err == nil && status == broker.MarketClosed {
		return ErrorResult(fmt.Sprintf("market %s is currently closed", underlying))
	}

	// --- Gate 4: balance preflight (for buy orders) ---
	if sideUpper == "BUY" {
		if pp, ok := p.(broker.PortfolioProvider); ok {
			bals, err := pp.GetBalances(ctx)
			if err == nil {
				var cashBalance float64
				for _, b := range bals {
					if strings.EqualFold(b.Asset, "USD") {
						cashBalance = b.Free
						break
					}
				}
				// Estimate cost = limit_price × 100 (contracts) × quantity
				needed := quantity * 100
				if notionalPrice != nil {
					needed = quantity * (*notionalPrice) * 100
				}
				if cashBalance < needed {
					return ErrorResult(fmt.Sprintf("insufficient USD balance: have %.8g, need %.8g", cashBalance, needed)).
						WithError(broker.ErrInsufficientFunds)
				}
			}
		}
	}

	// --- Gate 5: position size warning ---
	if notionalPrice != nil {
		notional := quantity * (*notionalPrice) * 100
		if notional > positionWarnThresholdUSD {
			if !confirm {
				return ErrorResult(fmt.Sprintf("large order warning: notional value %.2f USD exceeds %.0f USD. Set confirm=true to proceed.", notional, float64(positionWarnThresholdUSD)))
			}
		}
	}

	// --- Gate 6: confirmation required ---
	if !confirm {
		out := fmt.Sprintf("Dry-run: all safety checks passed for options order:\n  Underlying: %s\n  Expiry: %s\n  Strike: %.2f\n  Type: %s\n  Side: %s\n  Quantity: %.0f contracts\n  Order Type: %s\n  Time-in-Force: %s",
			underlying, expiry, strike, optionTypeUpper, sideUpper, quantity, orderTypeUpper, tifUpper)
		if limitPrice != nil {
			out += fmt.Sprintf("\n  Limit Price: %.4f", *limitPrice)
		}
		if stopPrice != nil {
			out += fmt.Sprintf("\n  Stop Price: %.4f", *stopPrice)
		}
		out += fmt.Sprintf(" on %s. Set confirm=true to place the order.", providerID)
		return UserResult(out)
	}

	// --- Gate 7: execute --- (req was built and validated above)
	order, err := opt.PlaceOptionOrder(ctx, req)
	if err != nil {
		if hint := reauthHint(err, providerID, account); hint != nil {
			return hint
		}
		return ErrorResult(fmt.Sprintf("PlaceOptionOrder failed: %v", err)).WithError(err)
	}

	id := "-"
	if order.Id != nil {
		id = *order.Id
	}
	orderStatus := "-"
	if order.Status != nil {
		orderStatus = *order.Status
	}

	out := fmt.Sprintf("Option order placed on %s:\n  ID:          %s\n  Underlying:  %s\n  Expiry:      %s\n  Strike:      %.2f\n  Type:        %s\n  Side:        %s\n  Quantity:    %.0f contracts\n  Order Type:  %s\n  Time-in-Force: %s\n  Status:      %s\n",
		providerID, id, underlying, expiry, strike, optionTypeUpper, sideUpper, quantity, orderTypeUpper, tifUpper, orderStatus)
	if limitPrice != nil {
		out += fmt.Sprintf("  Limit Price: %.4f\n", *limitPrice)
	}
	if stopPrice != nil {
		out += fmt.Sprintf("  Stop Price:  %.4f\n", *stopPrice)
	}

	return UserResult(out)
}
