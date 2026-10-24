package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// --- futures_modify_protection ---

type FuturesModifyProtectionTool struct{ cfg *config.Config }

func NewFuturesModifyProtectionTool(cfg *config.Config) *FuturesModifyProtectionTool {
	return &FuturesModifyProtectionTool{cfg: cfg}
}

func (t *FuturesModifyProtectionTool) Name() string        { return NameFuturesModifyProtection }
func (t *FuturesModifyProtectionTool) Description() string { return DescFuturesModifyProtection }

func (t *FuturesModifyProtectionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":      map[string]any{"type": "string", "description": "binance or okx."},
			"account":       map[string]any{"type": "string"},
			"symbol":        map[string]any{"type": "string", "description": "Futures symbol, e.g. BTC/USDT:USDT."},
			"stop_loss":     map[string]any{"type": "number", "description": "New stop-loss trigger price."},
			"take_profit":   map[string]any{"type": "number", "description": "New take-profit trigger price."},
			"replace":       map[string]any{"type": "boolean", "description": "Cancel existing SL/TP before placing new ones (default true)."},
			"position_side": map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "Position side for hedge-mode accounts."},
			"amount":        map[string]any{"type": "number", "description": "Protection order quantity. Uses current position size if omitted."},
			"confirm":       map[string]any{"type": "boolean", "description": "Must be true to execute."},
		},
		"required": []string{"provider", "symbol", "confirm"},
	}
}

func (t *FuturesModifyProtectionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	stopLoss := numberArg(args, "stop_loss")
	takeProfit := numberArg(args, "take_profit")
	replace := true
	if v, ok := args["replace"].(bool); ok {
		replace = v
	}
	positionSideArg := strings.ToLower(stringArg(args, "position_side"))
	protectionQty := numberArg(args, "amount")
	confirm, _ := args["confirm"].(bool)

	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	if stopLoss == 0 && takeProfit == 0 {
		return ErrorResult("at least one of stop_loss or take_profit is required")
	}
	// Whether a protection change tightens or loosens risk cannot be known before the
	// existing orders are fetched, so it is classified as RiskIncrease (fail closed).
	if err := broker.CheckLeverage(t.cfg, "modify futures protection", broker.RiskIncrease); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	// Determine position side, quantity, and margin mode from live position if not specified
	positionSide := positionSideArg
	var posMarginMode string
	if protectionQty == 0 || positionSide == "" {
		positions, posErr := fp.FetchFuturesPositions(ctx, []string{symbol})
		if posErr == nil {
			for _, p := range positions {
				if p.Contracts == nil || *p.Contracts == 0 || p.Symbol == nil {
					continue
				}
				if normalizeFuturesSymbol(*p.Symbol) != symbol {
					continue
				}
				if protectionQty == 0 && p.Contracts != nil {
					protectionQty = math.Abs(*p.Contracts)
				}
				if positionSide == "" && p.Side != nil {
					positionSide = *p.Side
				}
				if p.MarginMode != nil {
					posMarginMode = *p.MarginMode
				}
				break
			}
		}
	}
	if protectionQty == 0 {
		return ErrorResult("cannot determine position size — specify amount explicitly")
	}
	if positionSide == "" {
		return ErrorResult("cannot determine position side — specify position_side explicitly")
	}

	closeSide := futuresCloseSide(positionSide)

	if !confirm {
		var parts []string
		if stopLoss > 0 {
			parts = append(parts, fmt.Sprintf("SL @ %.8g", stopLoss))
		}
		if takeProfit > 0 {
			parts = append(parts, fmt.Sprintf("TP @ %.8g", takeProfit))
		}
		replaceNote := ""
		if replace {
			replaceNote = " (cancelling existing protection first)"
		}
		return UserResult(fmt.Sprintf("Dry-run: would set %s protection on %s %s qty %.8g%s. Set confirm=true to execute.",
			strings.Join(parts, " and "), symbol, positionSide, protectionQty, replaceNote))
	}

	// Rate limit: spent only on a confirmed change, before the first cancel/placement.
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q", providerID)).WithError(broker.ErrRateLimited)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Modify protection on %s %s:\n", symbol, providerID))

	// Cancel existing protection orders if replace=true
	if replace {
		existingOrders, listErr := fp.FetchFuturesOpenOrders(ctx, symbol)
		if listErr == nil {
			for _, o := range existingOrders {
				isProtection := false
				if o.StopLossPrice != nil && *o.StopLossPrice > 0 {
					isProtection = true
				}
				if o.TakeProfitPrice != nil && *o.TakeProfitPrice > 0 {
					isProtection = true
				}
				if o.ReduceOnly != nil && *o.ReduceOnly {
					isProtection = true
				}
				if isProtection && o.Id != nil {
					_, cancelErr := fp.CancelFuturesOrder(ctx, *o.Id, symbol)
					if cancelErr != nil {
						sb.WriteString(fmt.Sprintf("  Cancel %s: failed — %v\n", *o.Id, cancelErr))
					} else {
						sb.WriteString(fmt.Sprintf("  Cancel %s: ok\n", *o.Id))
					}
				}
			}
		}
	}

	var protectionErr bool

	if stopLoss > 0 {
		slParams := map[string]interface{}{"stopLossPrice": stopLoss}
		order, slErr := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
			Symbol: symbol, OrderType: "market", Side: closeSide, Amount: protectionQty,
			PositionSide: positionSide, ReduceOnly: true, MarginMode: posMarginMode, Params: slParams,
		})
		sb.WriteString(fmt.Sprintf("  stop_loss:   %s\n", formatOrderLine("stop_loss", order, slErr)))
		if slErr != nil {
			protectionErr = true
		}
	}
	if takeProfit > 0 {
		tpParams := map[string]interface{}{"takeProfitPrice": takeProfit}
		order, tpErr := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
			Symbol: symbol, OrderType: "market", Side: closeSide, Amount: protectionQty,
			PositionSide: positionSide, ReduceOnly: true, MarginMode: posMarginMode, Params: tpParams,
		})
		sb.WriteString(fmt.Sprintf("  take_profit: %s\n", formatOrderLine("take_profit", order, tpErr)))
		if tpErr != nil {
			protectionErr = true
		}
	}

	if protectionErr {
		return ErrorResult(fmt.Sprintf("UNPROTECTED — some protection orders failed for %s. Review and retry.\n\n%s", symbol, sb.String()))
	}
	return UserResult(sb.String())
}

// --- futures_cancel_orders ---

type FuturesCancelOrdersTool struct{ cfg *config.Config }

func NewFuturesCancelOrdersTool(cfg *config.Config) *FuturesCancelOrdersTool {
	return &FuturesCancelOrdersTool{cfg: cfg}
}

func (t *FuturesCancelOrdersTool) Name() string        { return NameFuturesCancelOrders }
func (t *FuturesCancelOrdersTool) Description() string { return DescFuturesCancelOrders }

func (t *FuturesCancelOrdersTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string"},
			"symbol":   map[string]any{"type": "string", "description": "Futures symbol. Required for order_id or type filter."},
			"order_id": map[string]any{"type": "string", "description": "Cancel a specific order by ID (requires symbol)."},
			"type":     map[string]any{"type": "string", "enum": []string{"all", "protection"}, "description": "Cancel all orders or only protection (reduce-only) orders on the symbol."},
			"confirm":  map[string]any{"type": "boolean", "description": "Must be true to execute."},
		},
		"required": []string{"provider", "confirm"},
	}
}

func (t *FuturesCancelOrdersTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	orderIDArg := stringArg(args, "order_id")
	cancelType := strings.ToLower(stringArg(args, "type"))
	confirm, _ := args["confirm"].(bool)

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if orderIDArg == "" && symbol == "" {
		return ErrorResult("symbol is required when not cancelling by order_id")
	}
	// Cancelling orders only removes pending exposure, so it is a risk-reducing action.
	if err := broker.CheckLeverage(t.cfg, "cancel futures orders", broker.RiskReduce); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	if !confirm {
		if orderIDArg != "" {
			return UserResult(fmt.Sprintf("Dry-run: would cancel futures order %s on %s %s. Set confirm=true to execute.", orderIDArg, providerID, symbol))
		}
		return UserResult(fmt.Sprintf("Dry-run: would cancel %q futures orders on %s %s. Set confirm=true to execute.", cancelType, providerID, symbol))
	}

	// Rate limit: spent only on a confirmed cancel, before the first cancellation.
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q", providerID)).WithError(broker.ErrRateLimited)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Cancel futures orders on %s %s:\n", providerID, symbol))

	if orderIDArg != "" {
		// Cancel single order
		o, cancelErr := fp.CancelFuturesOrder(ctx, orderIDArg, symbol)
		if cancelErr != nil {
			return ErrorResult(fmt.Sprintf("futures_cancel_orders: cancel %s: %v", orderIDArg, cancelErr)).WithError(cancelErr)
		}
		sb.WriteString(fmt.Sprintf("  Cancelled: %s\n", orderID(o)))
		return UserResult(sb.String())
	}

	// Cancel by type
	if cancelType == "all" || cancelType == "" {
		cancelled, cancelErr := fp.CancelAllFuturesOrders(ctx, symbol)
		if cancelErr != nil {
			return ErrorResult(fmt.Sprintf("futures_cancel_orders: cancel all: %v", cancelErr)).WithError(cancelErr)
		}
		sb.WriteString(fmt.Sprintf("  Cancelled %d orders\n", len(cancelled)))
		return UserResult(sb.String())
	}

	// Cancel protection orders only
	openOrders, listErr := fp.FetchFuturesOpenOrders(ctx, symbol)
	if listErr != nil {
		return ErrorResult(fmt.Sprintf("futures_cancel_orders: fetch open orders: %v", listErr)).WithError(listErr)
	}
	count := 0
	for _, o := range openOrders {
		isProtection := (o.StopLossPrice != nil && *o.StopLossPrice > 0) ||
			(o.TakeProfitPrice != nil && *o.TakeProfitPrice > 0) ||
			(o.ReduceOnly != nil && *o.ReduceOnly)
		if !isProtection {
			continue
		}
		if o.Id == nil {
			continue
		}
		_, cancelErr := fp.CancelFuturesOrder(ctx, *o.Id, symbol)
		if cancelErr != nil {
			sb.WriteString(fmt.Sprintf("  Cancel %s: failed — %v\n", *o.Id, cancelErr))
		} else {
			sb.WriteString(fmt.Sprintf("  Cancel %s: ok\n", *o.Id))
			count++
		}
	}
	sb.WriteString(fmt.Sprintf("  %d protection orders cancelled\n", count))
	return UserResult(sb.String())
}
