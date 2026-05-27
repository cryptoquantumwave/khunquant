package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

const maxFuturesFundingHistoryLimit = 100

func normalizeFuturesSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(symbol, "_", "/")))
	if s == "" || strings.Contains(s, ":") {
		return s
	}
	if !strings.Contains(s, "/") {
		for _, q := range []string{"USDT", "USDC", "USD"} {
			if strings.HasSuffix(s, q) && len(s) > len(q) {
				return s[:len(s)-len(q)] + "/" + q + ":" + q
			}
		}
		return s
	}
	parts := strings.SplitN(s, "/", 2)
	quote := parts[1]
	if idx := strings.Index(quote, "-"); idx >= 0 {
		quote = quote[:idx]
	}
	return parts[0] + "/" + parts[1] + ":" + quote
}

func futuresProvider(ctx context.Context, cfg *config.Config, providerID, account string) (broker.FuturesProvider, error) {
	p, err := broker.CreateProviderForAccount(providerID, account, cfg)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", providerID, err)
	}
	fp, ok := p.(broker.FuturesProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support futures trading (Binance TH and Bitkub are spot-only here)", providerID)
	}
	_ = ctx
	return fp, nil
}

func futuresPositionSide(side string) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "long", "buy":
		return "buy", "long", nil
	case "short", "sell":
		return "sell", "short", nil
	default:
		return "", "", fmt.Errorf("side must be long or short")
	}
}

func futuresCloseSide(positionSide string) string {
	if strings.EqualFold(positionSide, "short") {
		return "buy"
	}
	return "sell"
}

func marginModeOrDefault(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "cross"
	}
	return v
}

func copyToolParams(v any) map[string]interface{} {
	out := map[string]interface{}{}
	if m, ok := v.(map[string]interface{}); ok {
		for k, val := range m {
			out[k] = val
		}
	}
	return out
}

type FuturesSetLeverageTool struct{ cfg *config.Config }

func NewFuturesSetLeverageTool(cfg *config.Config) *FuturesSetLeverageTool {
	return &FuturesSetLeverageTool{cfg: cfg}
}

func (t *FuturesSetLeverageTool) Name() string { return NameFuturesSetLeverage }

func (t *FuturesSetLeverageTool) Description() string {
	return "Set leverage for a Binance or OKX perpetual futures symbol. Symbols use CCXT contract format, e.g. BTC/USDT:USDT; BTCUSDT and BTC/USDT are normalized to that format."
}

func (t *FuturesSetLeverageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":      map[string]any{"type": "string", "description": "binance or okx."},
			"account":       map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":        map[string]any{"type": "string", "description": "Perp symbol, e.g. BTC/USDT:USDT, BTC/USDT, or BTCUSDT."},
			"leverage":      map[string]any{"type": "integer", "description": "Initial leverage, 1-125."},
			"margin_mode":   map[string]any{"type": "string", "enum": []string{"cross", "isolated"}, "description": "Default cross."},
			"position_side": map[string]any{"type": "string", "enum": []string{"long", "short", "net"}, "description": "Optional hedge-mode side."},
			"confirm":       map[string]any{"type": "boolean", "description": "Must be true to set leverage."},
		},
		"required": []string{"provider", "symbol", "leverage", "confirm"},
	}
}

func (t *FuturesSetLeverageTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	lev := int64(numberArg(args, "leverage"))
	marginMode := marginModeOrDefault(stringArg(args, "margin_mode"))
	positionSide := strings.ToLower(stringArg(args, "position_side"))
	confirm, _ := args["confirm"].(bool)

	if providerID == "" || symbol == "" || lev <= 0 {
		return ErrorResult("provider, symbol, and positive leverage are required")
	}
	if lev > 125 {
		return ErrorResult("leverage must be <= 125")
	}
	if !confirm {
		return UserResult(fmt.Sprintf("Dry-run: would set %dx leverage on %s %s (%s). Set confirm=true to apply.", lev, providerID, symbol, marginMode))
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	if _, err := fp.SetFuturesLeverage(ctx, symbol, lev, marginMode, positionSide); err != nil {
		return ErrorResult(fmt.Sprintf("futures_set_leverage failed: %v", err)).WithError(err)
	}
	return UserResult(fmt.Sprintf("Set %dx futures leverage on %s %s (%s).", lev, providerID, symbol, marginMode))
}

type FuturesOpenPositionTool struct{ cfg *config.Config }

func NewFuturesOpenPositionTool(cfg *config.Config) *FuturesOpenPositionTool {
	return &FuturesOpenPositionTool{cfg: cfg}
}

func (t *FuturesOpenPositionTool) Name() string { return NameFuturesOpenPosition }

func (t *FuturesOpenPositionTool) Description() string {
	return "Open a Binance or OKX long/short perpetual futures position with leverage, optional limit price, and optional reduce-only stop-loss/take-profit protection orders. Requires confirm=true for live execution."
}

func (t *FuturesOpenPositionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":    map[string]any{"type": "string", "description": "binance or okx."},
			"account":     map[string]any{"type": "string", "description": "Account name (empty = default)."},
			"symbol":      map[string]any{"type": "string", "description": "Perp symbol, e.g. BTC/USDT:USDT, BTC/USDT, or BTCUSDT."},
			"side":        map[string]any{"type": "string", "enum": []string{"long", "short"}, "description": "Position direction."},
			"amount":      map[string]any{"type": "number", "description": "Contract/base quantity as expected by the exchange market."},
			"leverage":    map[string]any{"type": "integer", "description": "Initial leverage, 1-125."},
			"margin_mode": map[string]any{"type": "string", "enum": []string{"cross", "isolated"}, "description": "Default cross."},
			"order_type":  map[string]any{"type": "string", "enum": []string{"market", "limit"}, "description": "Default market."},
			"price":       map[string]any{"type": "number", "description": "Required for limit entry orders."},
			"stop_loss":   map[string]any{"type": "number", "description": "Optional stop-loss trigger price."},
			"take_profit": map[string]any{"type": "number", "description": "Optional take-profit trigger price."},
			"params":      map[string]any{"type": "object", "description": "Extra CCXT/exchange params."},
			"confirm":     map[string]any{"type": "boolean", "description": "Must be true to place live futures orders."},
		},
		"required": []string{"provider", "symbol", "side", "amount", "leverage", "confirm"},
	}
}

func (t *FuturesOpenPositionTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID, _ := args["provider"].(string)
	account, _ := args["account"].(string)
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	entrySide, positionSide, sideErr := futuresPositionSide(stringArg(args, "side"))
	amount := numberArg(args, "amount")
	lev := int64(numberArg(args, "leverage"))
	marginMode := marginModeOrDefault(stringArg(args, "margin_mode"))
	orderType := strings.ToLower(stringArg(args, "order_type"))
	if orderType == "" {
		orderType = "market"
	}
	confirm, _ := args["confirm"].(bool)
	params := copyToolParams(args["params"])

	var price *float64
	if v := numberArg(args, "price"); v > 0 {
		price = &v
	}
	stopLoss := numberArg(args, "stop_loss")
	takeProfit := numberArg(args, "take_profit")

	if providerID == "" || symbol == "" || amount <= 0 || lev <= 0 {
		return ErrorResult("provider, symbol, amount, and positive leverage are required")
	}
	if sideErr != nil {
		return ErrorResult(sideErr.Error())
	}
	if lev > 125 {
		return ErrorResult("leverage must be <= 125")
	}
	if orderType == "limit" && price == nil {
		return ErrorResult("price is required for limit futures entries")
	}
	if providerID != "binance" && providerID != "okx" {
		return ErrorResult("futures trading is currently supported only for binance and okx")
	}

	notional := amount
	if price != nil {
		notional *= *price
	}
	if !confirm {
		return UserResult(fmt.Sprintf("Dry-run: would open %s %s %.8g %s at %dx %s on %s%s%s. Set confirm=true to execute.",
			positionSide, orderType, amount, symbol, lev, marginMode, providerID,
			priceText(price), protectionText(stopLoss, takeProfit)))
	}
	if err := broker.CheckPermission(t.cfg, providerID, account, config.ScopeTrade); err != nil {
		return ErrorResult(err.Error())
	}
	if err := broker.GlobalLossTracker.CheckDailyLoss(t.cfg.TradingRisk.DailyLossLimitUSD); err != nil {
		return ErrorResult(err.Error())
	}
	if notional > positionWarnThresholdUSD && price != nil {
		return ErrorResult(fmt.Sprintf("large futures position warning: notional value %.2f exceeds %.0f USD. Re-run only after explicit user confirmation.", notional, float64(positionWarnThresholdUSD)))
	}
	if !broker.DefaultLimiter.Allow(providerID) {
		return ErrorResult(fmt.Sprintf("rate limit exceeded for provider %q - try again in a minute", providerID)).WithError(broker.ErrRateLimited)
	}

	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	if _, err := fp.SetFuturesLeverage(ctx, symbol, lev, marginMode, positionSide); err != nil {
		return ErrorResult(fmt.Sprintf("set leverage failed: %v", err)).WithError(err)
	}
	entry, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
		Symbol:       symbol,
		OrderType:    orderType,
		Side:         entrySide,
		Amount:       amount,
		Price:        price,
		MarginMode:   marginMode,
		PositionSide: positionSide,
		Params:       params,
	})
	if err != nil {
		return ErrorResult(fmt.Sprintf("entry order failed: %v", err)).WithError(err)
	}

	var protective []string
	closeSide := futuresCloseSide(positionSide)
	if stopLoss > 0 {
		slParams := map[string]interface{}{"stopLossPrice": stopLoss}
		order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
			Symbol: symbol, OrderType: "market", Side: closeSide, Amount: amount,
			MarginMode: marginMode, PositionSide: positionSide, ReduceOnly: true, Params: slParams,
		})
		protective = append(protective, formatOrderLine("stop_loss", order, err))
	}
	if takeProfit > 0 {
		tpParams := map[string]interface{}{"takeProfitPrice": takeProfit}
		order, err := fp.CreateFuturesOrder(ctx, broker.FuturesOrderRequest{
			Symbol: symbol, OrderType: "market", Side: closeSide, Amount: amount,
			MarginMode: marginMode, PositionSide: positionSide, ReduceOnly: true, Params: tpParams,
		})
		protective = append(protective, formatOrderLine("take_profit", order, err))
	}

	out := fmt.Sprintf("Futures position entry placed on %s:\n  Entry order: %s\n  Symbol:      %s\n  Side:        %s\n  Amount:      %.8g\n  Leverage:    %dx\n  Margin mode: %s\n",
		providerID, orderID(entry), symbol, positionSide, amount, lev, marginMode)
	if len(protective) > 0 {
		out += "\nProtection orders:\n  " + strings.Join(protective, "\n  ") + "\n"
	}
	return UserResult(out)
}

type FuturesGetOrderTool struct{ cfg *config.Config }

func NewFuturesGetOrderTool(cfg *config.Config) *FuturesGetOrderTool {
	return &FuturesGetOrderTool{cfg: cfg}
}
func (t *FuturesGetOrderTool) Name() string        { return NameFuturesGetOrder }
func (t *FuturesGetOrderTool) Description() string { return DescFuturesGetOrder }
func (t *FuturesGetOrderTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string"},
			"account":  map[string]any{"type": "string"},
			"order_id": map[string]any{"type": "string"},
			"symbol":   map[string]any{"type": "string"},
		},
		"required": []string{"provider", "order_id", "symbol"},
	}
}
func (t *FuturesGetOrderTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	orderIDArg := stringArg(args, "order_id")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	if providerID == "" || orderIDArg == "" || symbol == "" {
		return ErrorResult("provider, order_id, and symbol are required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	order, err := fp.FetchFuturesOrder(ctx, orderIDArg, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_get_order failed: %v", err)).WithError(err)
	}
	return UserResult(formatFuturesOrder(providerID, orderIDArg, order))
}

type FuturesGetPositionsTool struct{ cfg *config.Config }

func NewFuturesGetPositionsTool(cfg *config.Config) *FuturesGetPositionsTool {
	return &FuturesGetPositionsTool{cfg: cfg}
}
func (t *FuturesGetPositionsTool) Name() string        { return NameFuturesGetPositions }
func (t *FuturesGetPositionsTool) Description() string { return DescFuturesGetPositions }
func (t *FuturesGetPositionsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider": map[string]any{"type": "string", "description": "binance or okx."},
			"account":  map[string]any{"type": "string"},
			"symbol":   map[string]any{"type": "string", "description": "Optional futures symbol filter."},
		},
		"required": []string{"provider"},
	}
}
func (t *FuturesGetPositionsTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	if providerID == "" {
		return ErrorResult("provider is required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	var symbols []string
	if symbol != "" {
		symbols = []string{symbol}
	}
	positions, err := fp.FetchFuturesPositions(ctx, symbols)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_get_positions failed: %v", err)).WithError(err)
	}
	return UserResult(formatFuturesPositions(providerID, positions))
}

type FuturesGetFundingTool struct{ cfg *config.Config }

func NewFuturesGetFundingTool(cfg *config.Config) *FuturesGetFundingTool {
	return &FuturesGetFundingTool{cfg: cfg}
}
func (t *FuturesGetFundingTool) Name() string        { return NameFuturesGetFunding }
func (t *FuturesGetFundingTool) Description() string { return DescFuturesGetFunding }
func (t *FuturesGetFundingTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"provider":        map[string]any{"type": "string", "description": "binance or okx."},
			"account":         map[string]any{"type": "string"},
			"symbol":          map[string]any{"type": "string", "description": "Futures symbol."},
			"include_history": map[string]any{"type": "boolean", "description": "Also fetch funding payments paid/received."},
			"since":           map[string]any{"type": "string", "description": "History start, e.g. 30d or 2026-01-01."},
			"limit":           map[string]any{"type": "integer", "description": "Funding history limit, max 100."},
		},
		"required": []string{"provider", "symbol"},
	}
}
func (t *FuturesGetFundingTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	providerID := stringArg(args, "provider")
	account := stringArg(args, "account")
	symbol := normalizeFuturesSymbol(stringArg(args, "symbol"))
	includeHistory, _ := args["include_history"].(bool)
	limit := int(numberArg(args, "limit"))
	if limit <= 0 || limit > maxFuturesFundingHistoryLimit {
		limit = maxFuturesFundingHistoryLimit
	}
	var since *int64
	if s := stringArg(args, "since"); s != "" {
		if ts := parseTimeParam(s); ts != nil {
			ms := ts.UnixMilli()
			since = &ms
		}
	}
	if providerID == "" || symbol == "" {
		return ErrorResult("provider and symbol are required")
	}
	fp, err := futuresProvider(ctx, t.cfg, providerID, account)
	if err != nil {
		return ErrorResult(err.Error()).WithError(err)
	}
	rate, err := fp.FetchFuturesFundingRate(ctx, symbol)
	if err != nil {
		return ErrorResult(fmt.Sprintf("futures_get_funding: FetchFundingRate: %v", err)).WithError(err)
	}
	out := formatFundingRate(providerID, symbol, rate)
	if includeHistory {
		history, err := fp.FetchFuturesFundingHistory(ctx, symbol, since, limit)
		if err != nil {
			return ErrorResult(fmt.Sprintf("futures_get_funding: FetchFundingHistory: %v", err)).WithError(err)
		}
		out += "\n" + formatFundingHistory(history)
	}
	return UserResult(out)
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func numberArg(args map[string]any, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return 0
	}
}

func priceText(price *float64) string {
	if price == nil {
		return ""
	}
	return fmt.Sprintf(" @ %.8g", *price)
}

func protectionText(sl, tp float64) string {
	var parts []string
	if sl > 0 {
		parts = append(parts, fmt.Sprintf("SL %.8g", sl))
	}
	if tp > 0 {
		parts = append(parts, fmt.Sprintf("TP %.8g", tp))
	}
	if len(parts) == 0 {
		return ""
	}
	return " with " + strings.Join(parts, " / ")
}

func orderID(o ccxt.Order) string {
	if o.Id == nil || *o.Id == "" {
		return "-"
	}
	return *o.Id
}

func formatOrderLine(kind string, o ccxt.Order, err error) string {
	if err != nil {
		return fmt.Sprintf("%s: failed (%v)", kind, err)
	}
	return fmt.Sprintf("%s: %s", kind, orderID(o))
}

func formatFuturesOrder(providerID, requestedID string, o ccxt.Order) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Futures order %s on %s:\n", requestedID, providerID))
	if o.Symbol != nil {
		sb.WriteString(fmt.Sprintf("  Symbol:    %s\n", *o.Symbol))
	}
	if o.Type != nil {
		sb.WriteString(fmt.Sprintf("  Type:      %s\n", *o.Type))
	}
	if o.Side != nil {
		sb.WriteString(fmt.Sprintf("  Side:      %s\n", *o.Side))
	}
	if o.Amount != nil {
		sb.WriteString(fmt.Sprintf("  Amount:    %.8g\n", *o.Amount))
	}
	if o.Price != nil {
		sb.WriteString(fmt.Sprintf("  Price:     %.8g\n", *o.Price))
	}
	if o.Filled != nil {
		sb.WriteString(fmt.Sprintf("  Filled:    %.8g\n", *o.Filled))
	}
	if o.Remaining != nil {
		sb.WriteString(fmt.Sprintf("  Remaining: %.8g\n", *o.Remaining))
	}
	if o.Status != nil {
		sb.WriteString(fmt.Sprintf("  Status:    %s\n", *o.Status))
	}
	if o.Datetime != nil {
		sb.WriteString(fmt.Sprintf("  Created:   %s\n", *o.Datetime))
	}
	return sb.String()
}

func formatFuturesPositions(providerID string, positions []ccxt.Position) string {
	var active []ccxt.Position
	for _, p := range positions {
		if p.Contracts != nil && *p.Contracts != 0 {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		return fmt.Sprintf("No active futures positions on %s.", providerID)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Futures positions on %s (%d):\n\n", providerID, len(active)))
	sb.WriteString(fmt.Sprintf("%-18s %-6s %12s %8s %12s %12s %12s\n", "Symbol", "Side", "Contracts", "Lev", "Entry", "Mark", "Unreal PnL"))
	for _, p := range active {
		sb.WriteString(fmt.Sprintf("%-18s %-6s %12s %8s %12s %12s %12s\n",
			futuresStrPtr(p.Symbol), futuresStrPtr(p.Side), futuresFloatPtr(p.Contracts), futuresFloatPtr(p.Leverage),
			futuresFloatPtr(p.EntryPrice), futuresFloatPtr(p.MarkPrice), futuresFloatPtr(p.UnrealizedPnl)))
		if p.RealizedPnl != nil {
			sb.WriteString(fmt.Sprintf("  realized_pnl=%s", futuresFloatPtr(p.RealizedPnl)))
		}
		if p.LiquidationPrice != nil {
			sb.WriteString(fmt.Sprintf("  liquidation=%s", futuresFloatPtr(p.LiquidationPrice)))
		}
		if p.MarginMode != nil {
			sb.WriteString(fmt.Sprintf("  margin=%s", *p.MarginMode))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func formatFundingRate(providerID, symbol string, r ccxt.FundingRate) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Funding for %s on %s:\n", symbol, providerID))
	if r.FundingRate != nil {
		sb.WriteString(fmt.Sprintf("  Current rate: %.8g\n", *r.FundingRate))
	}
	if r.NextFundingRate != nil {
		sb.WriteString(fmt.Sprintf("  Next rate:    %.8g\n", *r.NextFundingRate))
	}
	if r.MarkPrice != nil {
		sb.WriteString(fmt.Sprintf("  Mark price:   %.8g\n", *r.MarkPrice))
	}
	if r.IndexPrice != nil {
		sb.WriteString(fmt.Sprintf("  Index price:  %.8g\n", *r.IndexPrice))
	}
	if r.FundingTimestamp != nil {
		sb.WriteString(fmt.Sprintf("  Funding time: %s\n", time.UnixMilli(int64(*r.FundingTimestamp)).UTC().Format(time.RFC3339)))
	}
	if r.Interval != nil {
		sb.WriteString(fmt.Sprintf("  Interval:     %s\n", *r.Interval))
	}
	return sb.String()
}

func formatFundingHistory(history []ccxt.FundingHistory) string {
	if len(history) == 0 {
		return "No funding-fee history returned."
	}
	total := 0.0
	var currency string
	for _, h := range history {
		if h.Amount != nil {
			total += *h.Amount
		}
		if currency == "" && h.Currency != nil {
			currency = *h.Currency
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Funding-fee history (%d): total %.8g %s\n", len(history), total, currency))
	for i, h := range history {
		if i >= 10 {
			sb.WriteString("  ...\n")
			break
		}
		sb.WriteString(fmt.Sprintf("  %s  %s %s\n", futuresStrPtr(h.Datetime), futuresFloatPtr(h.Amount), futuresStrPtr(h.Currency)))
	}
	return sb.String()
}

func futuresStrPtr(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func futuresFloatPtr(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.8g", *f)
}
