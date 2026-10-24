package tools

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// --- futures_close_position ---

type FuturesClosePositionTool struct{ cfg *config.Config }

func NewFuturesClosePositionTool(cfg *config.Config) *FuturesClosePositionTool {
	return &FuturesClosePositionTool{cfg: cfg}
}

func (t *FuturesClosePositionTool) Name() string        { return NameFuturesClosePosition }
func (t *FuturesClosePositionTool) Description() string { return DescFuturesClosePosition }

func (t *FuturesClosePositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":      map[string]any{"type": "string", "description": "binance or okx."},
			"account":       map[string]any{"type": "string"},
			"symbol":        map[string]any{"type": "string", "description": "Futures symbol, e.g. BTC/USDT:USDT."},
			"order_type":    map[string]any{"type": "string", "enum": []string{"market", "limit"}, "description": "Default market."},
			"limit_price":   map[string]any{"type": "number", "description": "Required for limit close orders."},
			"position_side": map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "Explicit position side for hedge-mode accounts."},
			"confirm":       map[string]any{"type": "boolean", "description": "Must be true to execute."},
		},
		"required": []string{"provider", "symbol", "confirm"},
	}
}

// Execute runs the exit. Under paper mode it still acts on the real exchange,
// and the result says so (see markRealExit).
func (t *FuturesClosePositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return markRealExit(t.cfg, t.execute(ctx, args))
}

func (t *FuturesClosePositionTool) execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	orderType := strings.ToLower(stringArg(args, "order_type"))
	if orderType == "" {
		orderType = "market"
	}
	var limitPrice *float64
	if v := numberArg(args, "limit_price"); v > 0 {
		limitPrice = &v
	}
	positionSideArg := strings.ToLower(stringArg(args, "position_side"))
	confirm, _ := args["confirm"].(bool)

	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	if orderType == "limit" && limitPrice == nil {
		return ErrorResult("limit_price is required for limit close orders")
	}
	// A close only reduces exposure: the leverage switch must never block it.
	if err := broker.CheckLeverage(t.cfg, "close futures position", broker.RiskReduce); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	// Load live position to determine size and side
	positions, err := fp.FetchFuturesPositions(ctx, []string{symbol})
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_close_position: fetch positions: %v", err)).WithError(err)
	}
	var closeAmount float64
	var positionSide, positionMarginMode string
	// C-4: snapshot the unrealised PnL before the close; afterwards the position
	// is gone and the loss it realised is unrecoverable.
	var pnlBeforeClose *float64
	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 {
			continue
		}
		sym := ""
		if p.Symbol != nil {
			sym = normalizeFuturesSymbol(*p.Symbol)
		}
		if sym != symbol {
			continue
		}
		closeAmount = math.Abs(*p.Contracts)
		pnlBeforeClose = p.UnrealizedPnl
		if p.Side != nil {
			positionSide = *p.Side
		}
		if p.MarginMode != nil {
			positionMarginMode = *p.MarginMode
		}
		break
	}
	if positionSideArg != "" {
		positionSide = positionSideArg
	}
	if closeAmount == 0 {
		return ErrorResult(fmt.Sprintf("no active %s position found on %s", symbol, providerID))
	}
	closeSide := futuresCloseSide(positionSide)

	if !confirm {
		return UserResult(fmt.Sprintf("Dry-run: would close %s position on %s — %s %.8g %s (reduce-only). Set confirm=true to execute.",
			symbol, providerID, closeSide, closeAmount, orderType))
	}

	// Rate limit: spent only on a confirmed close, right before the order.
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q", providerID)).WithError(broker.ErrRateLimited)
	}

	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       symbol,
		OrderType:    orderType,
		Side:         closeSide,
		Amount:       closeAmount,
		Price:        limitPrice,
		PositionSide: positionSide,
		ReduceOnly:   true,
		MarginMode:   positionMarginMode,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_close_position failed: %v", err)).WithError(err)
	}
	closeID := orderID(order)

	// Verify fill
	filled, status, partial, verifyErr := verifyFuturesFill(ctx, fp, closeID, symbol, closeAmount)
	// closeAmount is the whole position here, so it is both the requested
	// amount and the denominator.
	recordRealisedLoss(symbol, pnlBeforeClose, filled, closeAmount, closeAmount, verifyErr)
	publishFuturesExit(ctx, NameFuturesClosePosition, providerID, account, symbol, closeSide, orderType, closeAmount, order, filled, status, verifyErr)
	fillNote := ""
	if partial {
		fillNote = fmt.Sprintf(" (partial: %.8g / %.8g filled)", filled, closeAmount)
	}

	return UserResult(fmt.Sprintf("Futures close order placed on %s:\n  Order:    %s\n  Symbol:   %s\n  Side:     %s\n  Amount:   %.8g\n  Type:     %s\n  Status:   %s%s\n",
		providerID, closeID, symbol, closeSide, closeAmount, orderType, status, fillNote))
}

// --- futures_reduce_position ---

type FuturesReducePositionTool struct{ cfg *config.Config }

func NewFuturesReducePositionTool(cfg *config.Config) *FuturesReducePositionTool {
	return &FuturesReducePositionTool{cfg: cfg}
}

func (t *FuturesReducePositionTool) Name() string        { return NameFuturesReducePosition }
func (t *FuturesReducePositionTool) Description() string { return DescFuturesReducePosition }

func (t *FuturesReducePositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":      map[string]any{"type": "string", "description": "binance or okx."},
			"account":       map[string]any{"type": "string"},
			"symbol":        map[string]any{"type": "string", "description": "Futures symbol, e.g. BTC/USDT:USDT."},
			"amount":        map[string]any{"type": "number", "description": "Absolute amount to reduce (use this OR percent, not both)."},
			"percent":       map[string]any{"type": "number", "description": "Percentage of position to reduce (1–100)."},
			"order_type":    map[string]any{"type": "string", "enum": []string{"market", "limit"}, "description": "Default market."},
			"limit_price":   map[string]any{"type": "number", "description": "Required for limit close orders."},
			"position_side": map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "Explicit side for hedge-mode."},
			"confirm":       map[string]any{"type": "boolean", "description": "Must be true to execute."},
		},
		"required": []string{"provider", "symbol", "confirm"},
	}
}

// Execute runs the exit. Under paper mode it still acts on the real exchange,
// and the result says so (see markRealExit).
func (t *FuturesReducePositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return markRealExit(t.cfg, t.execute(ctx, args))
}

func (t *FuturesReducePositionTool) execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	reduceAmount := numberArg(args, "amount")
	reducePercent := numberArg(args, "percent")
	orderType := strings.ToLower(stringArg(args, "order_type"))
	if orderType == "" {
		orderType = "market"
	}
	var limitPrice *float64
	if v := numberArg(args, "limit_price"); v > 0 {
		limitPrice = &v
	}
	positionSideArg := strings.ToLower(stringArg(args, "position_side"))
	confirm, _ := args["confirm"].(bool)

	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	hasAmount := reduceAmount > 0
	hasPercent := reducePercent > 0
	if !hasAmount && !hasPercent {
		return ErrorResult("exactly one of amount or percent is required")
	}
	if hasAmount && hasPercent {
		return ErrorResult("specify amount or percent, not both")
	}
	if hasPercent && (reducePercent < 1 || reducePercent > 100) {
		return ErrorResult("percent must be between 1 and 100")
	}
	if orderType == "limit" && limitPrice == nil {
		return ErrorResult("limit_price is required for limit close orders")
	}
	if err := broker.CheckLeverage(t.cfg, "reduce futures position", broker.RiskReduce); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	positions, err := fp.FetchFuturesPositions(ctx, []string{symbol})
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_reduce_position: fetch positions: %v", err)).WithError(err)
	}
	var positionSize float64
	var positionSide, posMarginMode string
	var pnlBeforeClose *float64
	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 {
			continue
		}
		sym := ""
		if p.Symbol != nil {
			sym = normalizeFuturesSymbol(*p.Symbol)
		}
		if sym != symbol {
			continue
		}
		positionSize = math.Abs(*p.Contracts)
		pnlBeforeClose = p.UnrealizedPnl
		if p.Side != nil {
			positionSide = *p.Side
		}
		if p.MarginMode != nil {
			posMarginMode = *p.MarginMode
		}
		break
	}
	if positionSideArg != "" {
		positionSide = positionSideArg
	}
	if positionSize == 0 {
		return ErrorResult(fmt.Sprintf("no active %s position found on %s", symbol, providerID))
	}

	// Compute reduce amount
	var finalAmount float64
	if hasPercent {
		finalAmount = positionSize * (reducePercent / 100.0)
	} else {
		finalAmount = reduceAmount
	}
	if finalAmount > positionSize {
		finalAmount = positionSize
	}

	closeSide := futuresCloseSide(positionSide)

	if !confirm {
		return UserResult(fmt.Sprintf("Dry-run: would reduce %s position on %s by %.8g (%.2f%%) — %s %.8g %s reduce-only. Set confirm=true to execute.",
			symbol, providerID, finalAmount, finalAmount/positionSize*100, closeSide, finalAmount, orderType))
	}

	// Rate limit: spent only on a confirmed reduce, right before the order.
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q", providerID)).WithError(broker.ErrRateLimited)
	}

	order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       symbol,
		OrderType:    orderType,
		Side:         closeSide,
		Amount:       finalAmount,
		Price:        limitPrice,
		PositionSide: positionSide,
		ReduceOnly:   true,
		MarginMode:   posMarginMode,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_reduce_position failed: %v", err)).WithError(err)
	}
	closeID := orderID(order)
	filled, status, partial, verifyErr := verifyFuturesFill(ctx, fp, closeID, symbol, finalAmount)
	// The snapshot describes the whole position, so prorate against the
	// position's size - dividing by finalAmount would over-record by 1/fraction.
	// finalAmount is what was requested, used only if the fill is unverifiable.
	recordRealisedLoss(symbol, pnlBeforeClose, filled, finalAmount, positionSize, verifyErr)
	publishFuturesExit(ctx, NameFuturesReducePosition, providerID, account, symbol, closeSide, orderType, finalAmount, order, filled, status, verifyErr)
	fillNote := ""
	if partial {
		fillNote = fmt.Sprintf(" (partial: %.8g / %.8g filled)", filled, finalAmount)
	}
	return UserResult(fmt.Sprintf("Futures reduce order placed on %s:\n  Order:    %s\n  Symbol:   %s\n  Reduced:  %.8g (%.2f%%)\n  Side:     %s\n  Type:     %s\n  Status:   %s%s\n",
		providerID, closeID, symbol, finalAmount, finalAmount/positionSize*100, closeSide, orderType, status, fillNote))
}

// --- futures_emergency_flatten ---

type FuturesEmergencyFlattenTool struct{ cfg *config.Config }

func NewFuturesEmergencyFlattenTool(cfg *config.Config) *FuturesEmergencyFlattenTool {
	return &FuturesEmergencyFlattenTool{cfg: cfg}
}

func (t *FuturesEmergencyFlattenTool) Name() string        { return NameFuturesEmergencyFlatten }
func (t *FuturesEmergencyFlattenTool) Description() string { return DescFuturesEmergencyFlatten }

func (t *FuturesEmergencyFlattenTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string"},
			"confirm":  map[string]any{"type": "boolean", "description": "Must be true to execute emergency flatten."},
		},
		"required": []string{"provider", "confirm"},
	}
}

// Execute runs the exit. Under paper mode it still acts on the real exchange,
// and the result says so (see markRealExit).
func (t *FuturesEmergencyFlattenTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return markRealExit(t.cfg, t.execute(ctx, args))
}

func (t *FuturesEmergencyFlattenTool) execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	confirm, _ := args["confirm"].(bool)

	if providerID == "" {
		return ErrorResult("provider is required")
	}
	if err := broker.CheckLeverage(t.cfg, "emergency flatten", broker.RiskReduce); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}

	// Load positions
	positions, err := fp.FetchFuturesPositions(ctx, nil)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_emergency_flatten: fetch positions: %v", err)).WithError(err)
	}
	type activePos struct {
		symbol, side, marginMode string
		contracts                float64
		pnlBeforeClose           *float64
	}
	var active []activePos
	for _, p := range positions {
		if p.Contracts == nil || *p.Contracts == 0 || p.Symbol == nil {
			continue
		}
		ap := activePos{
			symbol:         normalizeFuturesSymbol(*p.Symbol),
			contracts:      math.Abs(*p.Contracts),
			pnlBeforeClose: p.UnrealizedPnl,
		}
		if p.Side != nil {
			ap.side = *p.Side
		}
		if p.MarginMode != nil {
			ap.marginMode = *p.MarginMode
		}
		active = append(active, ap)
	}

	if !confirm {
		if len(active) == 0 {
			return UserResult(fmt.Sprintf("Dry-run: no active futures positions on %s to flatten.", providerID))
		}
		var posLines []string
		for _, ap := range active {
			posLines = append(posLines, fmt.Sprintf("  %s %s %.8g contracts", ap.side, ap.symbol, ap.contracts))
		}
		return UserResult(fmt.Sprintf("Dry-run: would flatten %d positions on %s:\n%s\nSet confirm=true to execute.",
			len(active), providerID, strings.Join(posLines, "\n")))
	}

	if len(active) == 0 {
		return UserResult(fmt.Sprintf("No active futures positions on %s — nothing to flatten.", providerID))
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Emergency flatten on %s:\n\n", providerID))

	// Step 1: cancel all open orders
	sb.WriteString("  Cancelling all open futures orders... ")
	cancelledOrders, cancelErr := fp.CancelAllFuturesOrders(ctx, "")
	if cancelErr != nil {
		sb.WriteString(fmt.Sprintf("error: %v\n", cancelErr))
	} else {
		sb.WriteString(fmt.Sprintf("%d orders cancelled\n", len(cancelledOrders)))
	}

	// Step 2: close each active position
	sb.WriteString("\n  Closing positions:\n")
	for _, ap := range active {
		closeSide := futuresCloseSide(ap.side)
		order, closeErr := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
			Symbol:       ap.symbol,
			OrderType:    "market",
			Side:         closeSide,
			Amount:       ap.contracts,
			PositionSide: ap.side,
			ReduceOnly:   true,
			MarginMode:   ap.marginMode,
		})
		if closeErr != nil {
			sb.WriteString(fmt.Sprintf("    %s %s: FAILED — %v\n", ap.side, ap.symbol, closeErr))
			continue
		}
		closeID := orderID(order)
		filled, status, partial, verifyErr := verifyFuturesFill(ctx, fp, closeID, ap.symbol, ap.contracts)
		// Flatten closes many positions; each records its own loss.
		recordRealisedLoss(ap.symbol, ap.pnlBeforeClose, filled, ap.contracts, ap.contracts, verifyErr)
		publishFuturesExit(ctx, NameFuturesEmergencyFlatten, providerID, account, ap.symbol, closeSide, "market", ap.contracts, order, filled, status, verifyErr)
		fillNote := ""
		if partial {
			fillNote = fmt.Sprintf(" partial %.8g/%.8g", filled, ap.contracts)
		}
		sb.WriteString(fmt.Sprintf("    %s %s: order %s — %s%s\n", ap.side, ap.symbol, closeID, status, fillNote))
	}

	// Step 3: final position check
	remaining, checkErr := fp.FetchFuturesPositions(ctx, nil)
	if checkErr == nil {
		var residual []string
		for _, p := range remaining {
			if p.Contracts != nil && *p.Contracts != 0 && p.Symbol != nil {
				residual = append(residual, fmt.Sprintf("%s %.8g", *p.Symbol, *p.Contracts))
			}
		}
		sb.WriteString("\n  Residual exposure: ")
		if len(residual) == 0 {
			sb.WriteString("none\n")
		} else {
			sb.WriteString(strings.Join(residual, ", ") + "\n")
		}
	}

	return UserResult(sb.String())
}
