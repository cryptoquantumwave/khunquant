package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StatefulSimulator is a Responder that handles stateful mutations for futures trading.
// It intercepts write endpoints (place order, cancel order, set leverage) and mutates
// in-memory state, then serves state-dependent read endpoints (fetch balance, positions, etc.).
// For unmodeled endpoints, it returns (nil, false) to fall through to the fixture store.
//
// Wire Format Semantics:
// - All responses are in the venue's exact wire format (JSON with string-typed numbers where required).
// - Binance USDM futures use /fapi/v1/ or /fapi/v3/ endpoints with string numbers ("100.5" not 100.5).
// - OKX perpetuals use /api/v5/ endpoints with string numbers in arrays {"code":"0","data":[...]}.
// - Contracts are in contract units (not base currency); market metadata includes contractSize.
// - Order side is "buy"/"sell" (not "long"/"short"); posSide/positionSide is separate.
//
// State Model:
// - Balance: Free/Locked per asset. Opening an order locks notional; closing unlocks it.
// - Position: Side, contracts, entry price, mark price, leverage. Created on order fill.
// - Orders: Tracked in open/closed maps. Market orders fill immediately; limit orders rest.
// - MarkPrice: Per-symbol, settable/seedable. Used for PnL display and order pricing.
//
// Does NOT simulate:
// - Fees, funding payments, slippage, partial fills
// - Liquidation, risk constraints, negative balance protection
// - Multi-leg orders, advanced order types
// - Order cancellation race conditions
type StatefulSimulator struct {
	mu           sync.RWMutex // protects concurrent Respond calls
	stateManager *StateManager
}

// NewStatefulSimulator creates a new stateful simulator with the given state manager.
func NewStatefulSimulator(stateManager *StateManager) *StatefulSimulator {
	return &StatefulSimulator{stateManager: stateManager}
}

// Respond handles stateful requests before the fixture store is consulted.
// Returns (resp, true) if it handled the request, or (nil, false) to fall through.
// SAFETY: Takes a write lock (Lock, not RLock) because handlers mutate shared VenueState maps
// (Balances, Positions, OpenOrders, ClosedOrders, etc.). See TestRespondHoldsRLockButMutatesState.
func (sim *StatefulSimulator) Respond(venue, method, path string, r *http.Request) (*Response, bool) {
	sim.mu.Lock()
	defer sim.mu.Unlock()

	// Only handle Binance and OKX for now.
	switch venue {
	case "binance":
		return sim.respondBinance(method, path, r)
	case "okx":
		return sim.respondOKX(method, path, r)
	default:
		return nil, false // fall through to fixture store
	}
}

// -- Binance Futures Endpoints (/fapi/v1/ and /fapi/v3/) --

func (sim *StatefulSimulator) respondBinance(method, path string, r *http.Request) (*Response, bool) {
	state := sim.stateManager.GetState("binance")

	// Dispatch by method and path. Check for /fapi/v3 first (newer), then /fapi/v1.
	switch {
	// Create futures order (v1 or v3)
	case method == "POST" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
		return sim.simBinanceCreateOrder(state, r)
	// Fetch futures order (v1 or v3)
	case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
		return sim.simBinanceFetchOrder(state, r)
	// Cancel futures order (v1 or v3)
	case method == "DELETE" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
		return sim.simBinanceCancelOrder(state, r)
	// Fetch open orders (v1 or v3)
	case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/openOrders") || strings.HasPrefix(path, "/fapi/v3/openOrders")):
		return sim.simBinanceFetchOpenOrders(state, r)
	// Fetch positions (positionRisk is Binance's positions endpoint, v1 or v3)
	case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/positionRisk") || strings.HasPrefix(path, "/fapi/v3/positionRisk")):
		return sim.simBinanceFetchPositions(state, r)
	// Fetch account (balance, v1 or v3)
	case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/account") || strings.HasPrefix(path, "/fapi/v3/account")):
		return sim.simBinanceFetchAccount(state, r)
	// Set leverage (v1 or v3)
	case method == "POST" && (strings.HasPrefix(path, "/fapi/v1/leverage") || strings.HasPrefix(path, "/fapi/v3/leverage")):
		return sim.simBinanceSetLeverage(state, r)
	// Fetch markets (exchangeInfo, v1 or v3)
	case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/exchangeInfo") || strings.HasPrefix(path, "/fapi/v3/exchangeInfo")):
		return sim.simBinanceFetchExchangeInfo(state, r)
	default:
		return nil, false // fall through to fixture store
	}
}

// simBinanceCreateOrder handles POST /fapi/v1/order and /fapi/v3/order for Binance.
// Binance uses query string + body parameters for signed POSTs.
func (sim *StatefulSimulator) simBinanceCreateOrder(state *VenueState, r *http.Request) (*Response, bool) {
	// Parse both query and body (Binance signed POSTs use query params).
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid request"), true
	}

	// Read body if present (for multipart or raw body)
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	symbol := r.FormValue("symbol")
	sideRaw := r.FormValue("side")
	orderType := strings.ToLower(r.FormValue("type"))
	quantityStr := r.FormValue("quantity")
	priceStr := r.FormValue("price")
	positionSideRaw := r.FormValue("positionSide")

	// Limit orders not yet implemented; reject until explicit fill helper added
	if orderType == "limit" {
		return sim.binanceErrorResponse(400, -1022, "Limit orders not yet implemented in sandbox"), true
	}

	// Validate side (must be "BUY" or "SELL", not "LONG"/"SHORT").
	side := strings.ToUpper(sideRaw)
	if side != "BUY" && side != "SELL" {
		// Binance error code -1022 for invalid side
		return sim.binanceErrorResponse(400, -1022, fmt.Sprintf("Unknown order side: %s", sideRaw)), true
	}

	// Validate positionSide (must be "LONG" or "SHORT", default to "LONG" if not provided).
	positionSide := strings.ToUpper(positionSideRaw)
	if positionSide == "" {
		// Default to LONG if not provided
		positionSide = "LONG"
	} else if positionSide != "LONG" && positionSide != "SHORT" {
		// Binance error code -1022 for invalid position side
		return sim.binanceErrorResponse(400, -1022, fmt.Sprintf("Invalid positionSide: %s", positionSideRaw)), true
	}

	// Validate symbol and quantity
	if symbol == "" {
		return sim.binanceErrorResponse(400, -1022, "symbol is missing"), true
	}

	quantity, err := strconv.ParseFloat(quantityStr, 64)
	if err != nil || quantity <= 0 {
		return sim.binanceErrorResponse(400, -1022, "Quantity is invalid"), true
	}

	// Get market metadata
	market, ok := state.Markets[symbol]
	if !ok {
		// Unknown market - return error instead of falling through
		return sim.binanceErrorResponse(400, -4625, "This symbol does not exist"), true
	}

	// Parse price if provided (for limit orders)
	price := 0.0
	if priceStr != "" {
		p, err := strconv.ParseFloat(priceStr, 64)
		if err == nil && p > 0 {
			price = p
		}
	}

	// For market orders, use mark price
	if orderType == "market" {
		// Try to use configured mark price for the symbol
		markPrice := state.MarkPrices[symbol]
		if markPrice > 0 {
			price = markPrice
		}
	}

	// If still no price, return error (need price data for order validation)
	if price <= 0 {
		return sim.binanceErrorResponse(400, -4625, fmt.Sprintf("Mark price unavailable for %s", symbol)), true
	}

	// Calculate notional value in quote currency (USDT)
	// notional = quantity (contracts) * contractSize (base per contract) * price (quote per base)
	notional := quantity * market.ContractSize * price

	// Check USDT balance (lock this amount)
	usdtBalance := state.Balances["USDT"]
	if usdtBalance.Free < notional {
		// Binance error code -2015 for insufficient balance
		return sim.binanceErrorResponse(400, -2015, "Margin is insufficient."), true
	}

	// Create the order
	orderID := sim.stateManager.NextOrderID("binance")
	now := time.Now()
	order := &Order{
		ID:        orderID,
		Symbol:    symbol,
		OrderType: orderType,
		Side:      side,
		Amount:    quantity,
		Filled:    quantity, // market orders fill immediately
		Average:   price,
		Cost:      notional,
		Status:    "FILLED", // market orders are immediately filled
		CreatedAt: now,
		UpdatedAt: now,
		Params: map[string]interface{}{
			"positionSide": positionSide,
		},
	}

	// Lock balance (deduct from free, add to locked)
	usdtBalance.Free -= notional
	usdtBalance.Locked += notional
	state.Balances["USDT"] = usdtBalance

	// Update or create position (filled immediately for market orders)
	if existing, ok := state.Positions[symbol]; ok {
		// Position exists, add to it
		existing.Contracts += quantity
		existing.EntryPrice = price
		existing.MarkPrice = price
		existing.UpdatedAt = now
	} else {
		// New position
		state.Positions[symbol] = &Position{
			Symbol:       symbol,
			Side:         strings.ToLower(positionSide),
			Contracts:    quantity,
			ContractSize: market.ContractSize,
			EntryPrice:   price,
			MarkPrice:    price,
			Leverage:     state.Leverage[symbol],
			UpdatedAt:    now,
		}
	}

	// Store order in closed orders (since it filled)
	state.ClosedOrders[orderID] = order

	// Build wire-format Binance order response
	respBody := sim.buildBinanceOrderResponseBody(order)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceFetchOrder handles GET /fapi/v1/order and /fapi/v3/order for Binance.
func (sim *StatefulSimulator) simBinanceFetchOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	_ = r.FormValue("symbol") // unused
	orderId := r.FormValue("orderId")

	if orderId == "" {
		return sim.binanceErrorResponse(400, -1022, "orderId is missing"), true
	}

	// Check open orders first, then closed
	order, ok := state.OpenOrders[orderId]
	if !ok {
		order, ok = state.ClosedOrders[orderId]
	}
	if !ok {
		return sim.binanceErrorResponse(404, -2013, "Order does not exist."), true
	}

	respBody := sim.buildBinanceOrderResponseBody(order)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceCancelOrder handles DELETE /fapi/v1/order and /fapi/v3/order for Binance.
func (sim *StatefulSimulator) simBinanceCancelOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	orderId := r.FormValue("orderId")
	if orderId == "" {
		return sim.binanceErrorResponse(400, -1022, "orderId is missing"), true
	}

	order, ok := state.OpenOrders[orderId]
	if !ok {
		// Order not found or already closed
		return sim.binanceErrorResponse(404, -2013, "Order does not exist."), true
	}

	// Move from open to closed
	delete(state.OpenOrders, orderId)
	order.Status = "CANCELED"
	order.UpdatedAt = time.Now()
	state.ClosedOrders[orderId] = order

	// Unlock the locked balance (refund the reservation)
	// The cost is locked, refund it entirely (for unfilled orders) or partially
	unfilled := order.Amount - order.Filled
	if unfilled > 0 {
		market, ok := state.Markets[order.Symbol]
		if ok {
			refundAmount := unfilled * market.ContractSize * order.Average
			usdtBal := state.Balances["USDT"]
			usdtBal.Locked -= refundAmount
			usdtBal.Free += refundAmount
			state.Balances["USDT"] = usdtBal
		}
	}

	respBody := sim.buildBinanceOrderResponseBody(order)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceFetchOpenOrders handles GET /fapi/v1/openOrders and /fapi/v3/openOrders for Binance.
func (sim *StatefulSimulator) simBinanceFetchOpenOrders(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	symbol := r.FormValue("symbol")
	var orders []map[string]interface{}

	for _, order := range state.OpenOrders {
		if symbol == "" || order.Symbol == symbol {
			orders = append(orders, sim.binanceOrderToWireFormat(order))
		}
	}

	// Marshal as array (Binance returns array of orders)
	respBody, _ := json.Marshal(orders)
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceFetchPositions handles GET /fapi/v1/positionRisk and /fapi/v3/positionRisk for Binance.
func (sim *StatefulSimulator) simBinanceFetchPositions(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	var positions []map[string]interface{}
	for _, pos := range state.Positions {
		if pos.Contracts > 0 {
			positions = append(positions, sim.binancePositionToWireFormat(pos))
		}
	}

	// Marshal as array (Binance returns array of positions)
	respBody, _ := json.Marshal(positions)
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceFetchAccount handles GET /fapi/v1/account and /fapi/v3/account for Binance.
// Returns the FUTURES account shape (not spot), with assets, positions, and totalWalletBalance.
func (sim *StatefulSimulator) simBinanceFetchAccount(state *VenueState, r *http.Request) (*Response, bool) {
	// Build futures account response with assets in wire format (string numbers).
	type FuturesAsset struct {
		Asset                  string `json:"asset"`
		WalletBalance          string `json:"walletBalance"`     // total balance
		AvailableBalance       string `json:"availableBalance"`  // free balance
		MarginBalance          string `json:"marginBalance"`     // available for margin
		MaintenanceMargin      string `json:"maintenanceMargin"` // minimum margin required
		InitialMargin          string `json:"initialMargin"`     // margin locked by open positions
		PositionInitialMargin  string `json:"positionInitialMargin"`
		OpenOrderInitialMargin string `json:"openOrderInitialMargin"`
		CrossWalletBalance     string `json:"crossWalletBalance"` // cross margin balance
		CrossUnPnl             string `json:"crossUnPnl"`         // unrealized PnL
		UnrealizedProfit       string `json:"unrealizedProfit"`   // total unrealized profit
		MaxWithdrawAmount      string `json:"maxWithdrawAmount"`  // max withdrawable
	}

	type FuturesPosition struct {
		Symbol                   string `json:"symbol"`
		InitialMargin            string `json:"initialMargin"`
		MaintMargin              string `json:"maintMargin"`
		OpenOrderInitialMargin   string `json:"openOrderInitialMargin"`
		PositionInitialMargin    string `json:"positionInitialMargin"`
		Positionamt              string `json:"positionAmt"`
		SymbolPrice              string `json:"symbolPrice"`
		UnrealizedProfit         string `json:"unrealizedProfit"`
		MicroUnitAmountRemaining int64  `json:"microUnitAmountRemaining"`
		BracketPosition          int64  `json:"bracketPosition"`
	}

	var assets []FuturesAsset
	totalWalletBalance := 0.0
	availableBalance := 0.0

	for asset, bal := range state.Balances {
		total := bal.Free + bal.Locked
		totalWalletBalance += total
		availableBalance += bal.Free

		assets = append(assets, FuturesAsset{
			Asset:                  asset,
			WalletBalance:          formatNumber(total),
			AvailableBalance:       formatNumber(bal.Free),
			MarginBalance:          formatNumber(bal.Free), // same as available in cross margin
			MaintenanceMargin:      "0",
			InitialMargin:          "0",
			PositionInitialMargin:  "0",
			OpenOrderInitialMargin: "0",
			CrossWalletBalance:     formatNumber(total),
			CrossUnPnl:             "0",
			UnrealizedProfit:       "0",
			MaxWithdrawAmount:      formatNumber(bal.Free),
		})
	}

	// Build positions array (empty for now, but structured for completeness)
	var positions []FuturesPosition
	for _, pos := range state.Positions {
		if pos.Contracts > 0 {
			// Position amount is contracts * contractSize (base currency units)
			positionAmt := formatNumber(pos.Contracts * pos.ContractSize)
			positions = append(positions, FuturesPosition{
				Symbol:                   pos.Symbol,
				InitialMargin:            "0",
				MaintMargin:              "0",
				OpenOrderInitialMargin:   "0",
				PositionInitialMargin:    "0",
				Positionamt:              positionAmt,
				SymbolPrice:              formatNumber(pos.MarkPrice),
				UnrealizedProfit:         "0",
				MicroUnitAmountRemaining: 0,
				BracketPosition:          0,
			})
		}
	}

	// Binance futures account response
	resp := map[string]interface{}{
		"assets":                       assets,
		"positions":                    positions,
		"totalWalletBalance":           formatNumber(totalWalletBalance),
		"availableBalance":             formatNumber(availableBalance),
		"totalUnrealizedProfit":        "0",
		"totalMarginRequired":          "0",
		"totalPositionInitialMargin":   "0",
		"totalOpenOrderInitialMargin":  "0",
		"totalCrossWalletBalance":      formatNumber(totalWalletBalance),
		"totalCrossUnPnl":              "0",
		"availableBalanceForOpenOrder": formatNumber(availableBalance),
		"canDeposit":                   true,
		"canTrade":                     true,
		"canWithdraw":                  true,
		"feeTier":                      0,
		"updateTime":                   time.Now().UnixMilli(),
	}

	respBody, _ := json.Marshal(resp)
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceSetLeverage handles POST /fapi/v1/leverage and /fapi/v3/leverage for Binance.
func (sim *StatefulSimulator) simBinanceSetLeverage(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	symbol := r.FormValue("symbol")
	leverageStr := r.FormValue("leverage")

	if symbol == "" {
		return sim.binanceErrorResponse(400, -1022, "symbol is missing"), true
	}
	if leverageStr == "" {
		return sim.binanceErrorResponse(400, -1022, "leverage is missing"), true
	}

	leverage, err := strconv.ParseInt(leverageStr, 10, 64)
	if err != nil || leverage < 1 {
		return sim.binanceErrorResponse(400, -1022, "invalid leverage"), true
	}

	// Check market limits
	if market, ok := state.Markets[symbol]; ok {
		if leverage > market.MaxLeverage {
			return sim.binanceErrorResponse(400, -1022, fmt.Sprintf("leverage exceeds max %d", market.MaxLeverage)), true
		}
	}

	// Update leverage in state
	state.Leverage[symbol] = leverage

	// Return Binance wire format response
	resp := map[string]interface{}{
		"symbol":   symbol,
		"leverage": leverage,
	}
	respBody, _ := json.Marshal(resp)
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simBinanceFetchExchangeInfo handles GET /fapi/v1/exchangeInfo and /fapi/v3/exchangeInfo for Binance.
func (sim *StatefulSimulator) simBinanceFetchExchangeInfo(state *VenueState, r *http.Request) (*Response, bool) {
	// If Markets is empty, fall through to fixtures (the recorded live data).
	// This prevents shadowing the fixture store's real payloads when simulator is fresh.
	if len(state.Markets) == 0 {
		return nil, false
	}

	// Build exchange info response with markets in wire format
	var symbols []map[string]interface{}

	for symbol, market := range state.Markets {
		// Binance USDM uses format like "BTCUSDT" not "BTC/USDT"
		parts := strings.Split(symbol, "/")
		var binanceSymbol string
		if len(parts) == 2 {
			binanceSymbol = parts[0] + parts[1]
		} else {
			binanceSymbol = symbol
		}

		baseAsset := strings.Split(binanceSymbol, "USDT")[0] // crude extraction, but works for USDT pairs
		quoteAsset := "USDT"

		symbols = append(symbols, map[string]interface{}{
			"symbol":             binanceSymbol,
			"contractType":       "PERPETUAL",
			"baseAsset":          baseAsset,
			"baseAssetPrecision": 8,
			"quoteAsset":         quoteAsset,
			"quotePrecision":     8,
			"marginAsset":        "USDT", // CCXT needs this to know it's a USDT-margined linear perpetual
			"pricePrecision":     2,
			"quantityPrecision":  3,
			"status":             "TRADING",
			"contractSize":       market.ContractSize,
			"orderTypes":         []string{"LIMIT", "MARKET"},
			"filters": []map[string]interface{}{
				{
					"filterType": "PRICE_FILTER",
					"minPrice":   "0.01",
					"maxPrice":   "1000000",
					"tickSize":   "0.01",
				},
				{
					"filterType": "LOT_SIZE",
					"minQty":     formatNumber(market.MinAmount),
					"maxQty":     "1000000",
					"stepSize":   formatNumber(market.MinAmount),
				},
			},
		})
	}

	resp := map[string]interface{}{
		"symbols": symbols,
	}

	respBody, _ := json.Marshal(resp)
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// -- OKX Perpetuals Endpoints (/api/v5/) --

func (sim *StatefulSimulator) respondOKX(method, path string, r *http.Request) (*Response, bool) {
	state := sim.stateManager.GetState("okx")

	// OKX uses query params for GET and body for POST.
	switch {
	// Create futures order (single or batch)
	case method == "POST" && (strings.HasPrefix(path, "/api/v5/trade/order") || strings.HasPrefix(path, "/api/v5/trade/batch-orders")):
		return sim.simOKXCreateOrder(state, r)
	// Fetch order
	case method == "GET" && strings.HasPrefix(path, "/api/v5/trade/order"):
		return sim.simOKXFetchOrder(state, r)
	// Cancel order
	case method == "POST" && strings.HasPrefix(path, "/api/v5/trade/cancel-order"):
		return sim.simOKXCancelOrder(state, r)
	// Fetch open orders
	case method == "GET" && strings.HasPrefix(path, "/api/v5/trade/orders-pending"):
		return sim.simOKXFetchOpenOrders(state, r)
	// Fetch positions (exact match: /api/v5/account/positions, not /api/v5/account/positions-history)
	case method == "GET" && path == "/api/v5/account/positions":
		return sim.simOKXFetchPositions(state, r)
	// Fetch balance
	case method == "GET" && strings.HasPrefix(path, "/api/v5/account/balance"):
		return sim.simOKXFetchBalance(state, r)
	// Set leverage
	case method == "POST" && strings.HasPrefix(path, "/api/v5/account/set-leverage"):
		return sim.simOKXSetLeverage(state, r)
	// Fetch instruments (markets)
	case method == "GET" && strings.HasPrefix(path, "/api/v5/public/instruments"):
		return sim.simOKXFetchInstruments(state, r)
	default:
		return nil, false // fall through to fixture store
	}
}

// simOKXCreateOrder handles POST /api/v5/trade/order and /api/v5/trade/batch-orders for OKX.
// OKX uses JSON body for POST requests.
func (sim *StatefulSimulator) simOKXCreateOrder(state *VenueState, r *http.Request) (*Response, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return sim.okxErrorResponse("51000", "invalid request body"), true
	}
	r.Body = io.NopCloser(bytes.NewReader(body)) // restore body

	// Check if this is a batch order request (array) or single order (object)
	isBatch := bytes.HasPrefix(bytes.TrimSpace(body), []byte("["))

	if isBatch {
		// Batch orders: parse as array and handle first order only (simplification)
		var requests []map[string]interface{}
		if err := json.Unmarshal(body, &requests); err != nil {
			return sim.okxErrorResponse("51000", "invalid JSON"), true
		}
		if len(requests) == 0 {
			return sim.okxErrorResponse("51000", "empty batch"), true
		}
		// Use first order in batch
		orderBody, _ := json.Marshal(requests[0])
		body = orderBody
	}

	var req struct {
		InstID  string `json:"instId"`
		TdMode  string `json:"tdMode"`
		Side    string `json:"side"`
		PosSide string `json:"posSide"`
		OrdType string `json:"ordType"`
		Sz      string `json:"sz"` // in contracts for perpetuals
		Px      string `json:"px"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return sim.okxErrorResponse("51000", "invalid JSON"), true
	}

	// Validate
	if req.InstID == "" {
		return sim.okxErrorResponse("51000", "missing instId"), true
	}
	if req.Side != "buy" && req.Side != "sell" {
		return sim.okxErrorResponse("51000", "invalid side"), true
	}
	// posSide can be "long", "short", or "net" (hedging mode); default to "net" if empty
	if req.PosSide == "" {
		req.PosSide = "net" // Default for batch/unified mode
	} else if req.PosSide != "long" && req.PosSide != "short" && req.PosSide != "net" {
		return sim.okxErrorResponse("51000", "invalid posSide"), true
	}

	// Limit orders not yet implemented; reject until explicit fill helper added
	if req.OrdType == "limit" {
		return sim.okxErrorResponse("51000", "Limit orders not yet implemented in sandbox"), true
	}

	sz, err := strconv.ParseFloat(req.Sz, 64)
	if err != nil || sz <= 0 {
		return sim.okxErrorResponse("51000", "invalid sz"), true
	}

	// Get market info
	market, ok := state.Markets[req.InstID]
	if !ok {
		// Unknown market - return error instead of falling through
		return sim.okxErrorResponse("51000", fmt.Sprintf("Instrument ID %s does not exist", req.InstID)), true
	}

	// Parse price
	price := 0.0
	if req.Px != "" {
		p, err := strconv.ParseFloat(req.Px, 64)
		if err == nil && p > 0 {
			price = p
		}
	}

	// For market orders, use mark price
	if req.OrdType == "market" {
		markPrice := state.MarkPrices[req.InstID]
		if markPrice > 0 {
			price = markPrice
		}
	}

	// If still no price, return error (need price data for order validation)
	if price <= 0 {
		return sim.okxErrorResponse("51000", fmt.Sprintf("Mark price unavailable for %s", req.InstID)), true
	}

	// Calculate notional value
	notional := sz * market.ContractSize * price

	// Check USDT balance
	usdtBal := state.Balances["USDT"]
	if usdtBal.Free < notional {
		// OKX insufficient funds error: code "51008"
		return sim.okxErrorResponse("51008", "The margin is insufficient."), true
	}

	// Create order
	orderID := sim.stateManager.NextOrderID("okx")
	now := time.Now()
	order := &Order{
		ID:        orderID,
		Symbol:    req.InstID,
		OrderType: req.OrdType,
		Side:      req.Side,
		Amount:    sz,
		Filled:    sz, // market orders fill immediately
		Average:   price,
		Cost:      notional,
		Status:    "filled",
		CreatedAt: now,
		UpdatedAt: now,
		Params: map[string]interface{}{
			"posSide": req.PosSide,
		},
	}

	// Lock balance
	usdtBal.Free -= notional
	usdtBal.Locked += notional
	state.Balances["USDT"] = usdtBal

	// Update or create position
	if existing, ok := state.Positions[req.InstID]; ok {
		existing.Contracts += sz
		existing.EntryPrice = price
		existing.MarkPrice = price
		existing.UpdatedAt = now
	} else {
		state.Positions[req.InstID] = &Position{
			Symbol:       req.InstID,
			Side:         req.PosSide,
			Contracts:    sz,
			ContractSize: market.ContractSize,
			EntryPrice:   price,
			MarkPrice:    price,
			Leverage:     state.Leverage[req.InstID],
			UpdatedAt:    now,
		}
	}

	// Store in closed orders
	state.ClosedOrders[orderID] = order

	// Return OKX-format response
	respBody := sim.buildOKXOrderResponseBody(orderID, req.InstID, req.Side, req.PosSide, sz, price)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXFetchOrder handles GET /api/v5/trade/order for OKX.
func (sim *StatefulSimulator) simOKXFetchOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}

	ordId := r.FormValue("ordId")
	_ = r.FormValue("instId") // unused

	if ordId == "" {
		return sim.okxErrorResponse("51000", "missing ordId"), true
	}

	order, ok := state.OpenOrders[ordId]
	if !ok {
		order, ok = state.ClosedOrders[ordId]
	}
	if !ok {
		return sim.okxErrorResponse("51014", "order not found"), true
	}

	posSide := order.Params["posSide"].(string)
	respBody := sim.buildOKXOrderResponseBody(ordId, order.Symbol, order.Side, posSide, order.Amount, order.Average)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXCancelOrder handles POST /api/v5/trade/cancel-order for OKX.
func (sim *StatefulSimulator) simOKXCancelOrder(state *VenueState, r *http.Request) (*Response, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return sim.okxErrorResponse("51000", "invalid request body"), true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req struct {
		OrdId  string `json:"ordId"`
		InstId string `json:"instId"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return sim.okxErrorResponse("51000", "invalid JSON"), true
	}

	order, ok := state.OpenOrders[req.OrdId]
	if !ok {
		return sim.okxErrorResponse("51014", "order not found"), true
	}

	delete(state.OpenOrders, req.OrdId)
	order.Status = "canceled"
	order.UpdatedAt = time.Now()
	state.ClosedOrders[req.OrdId] = order

	// Unlock balance
	unfilled := order.Amount - order.Filled
	if unfilled > 0 {
		market, ok := state.Markets[order.Symbol]
		if ok {
			refundAmount := unfilled * market.ContractSize * order.Average
			usdtBal := state.Balances["USDT"]
			usdtBal.Locked -= refundAmount
			usdtBal.Free += refundAmount
			state.Balances["USDT"] = usdtBal
		}
	}

	respBody := sim.buildOKXCancelResponseBody(req.OrdId, req.InstId)
	return &Response{
		Status: 200,
		Body:   respBody,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXFetchOpenOrders handles GET /api/v5/trade/orders-pending for OKX.
func (sim *StatefulSimulator) simOKXFetchOpenOrders(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}

	var orders []map[string]interface{}
	for _, order := range state.OpenOrders {
		orders = append(orders, sim.okxOrderToWireFormat(order))
	}

	respBody, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": orders,
	})
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXFetchPositions handles GET /api/v5/account/positions for OKX.
func (sim *StatefulSimulator) simOKXFetchPositions(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}

	var positions []map[string]interface{}
	for _, pos := range state.Positions {
		if pos.Contracts > 0 {
			positions = append(positions, sim.okxPositionToWireFormat(pos))
		}
	}

	respBody, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": positions,
	})
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXFetchBalance handles GET /api/v5/account/balance for OKX.
func (sim *StatefulSimulator) simOKXFetchBalance(state *VenueState, r *http.Request) (*Response, bool) {
	var balances []map[string]interface{}
	for asset, bal := range state.Balances {
		// OKX field names: availBal (available balance), frozenBal (frozen/locked balance)
		// CCXT reads these specific field names from details[] array
		balances = append(balances, map[string]interface{}{
			"ccy":       asset,
			"availBal":  formatNumber(bal.Free),
			"frozenBal": formatNumber(bal.Locked),
		})
	}

	respBody, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": []map[string]interface{}{
			{"details": balances},
		},
	})
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXSetLeverage handles POST /api/v5/account/set-leverage for OKX.
func (sim *StatefulSimulator) simOKXSetLeverage(state *VenueState, r *http.Request) (*Response, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return sim.okxErrorResponse("51000", "invalid request body"), true
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req struct {
		InstId string `json:"instId"`
		Lever  string `json:"lever"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return sim.okxErrorResponse("51000", "invalid JSON"), true
	}

	lever, err := strconv.ParseInt(req.Lever, 10, 64)
	if err != nil || lever < 1 {
		return sim.okxErrorResponse("51000", "invalid leverage"), true
	}

	// Check market limits
	if market, ok := state.Markets[req.InstId]; ok {
		if lever > market.MaxLeverage {
			return sim.okxErrorResponse("51000", fmt.Sprintf("leverage exceeds max %d", market.MaxLeverage)), true
		}
	}

	state.Leverage[req.InstId] = lever

	respBody, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": []map[string]interface{}{
			{
				"instId": req.InstId,
				"lever":  lever,
			},
		},
	})
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// simOKXFetchInstruments handles GET /api/v5/public/instruments for OKX.
func (sim *StatefulSimulator) simOKXFetchInstruments(state *VenueState, r *http.Request) (*Response, bool) {
	// If Markets is empty, fall through to fixtures (the recorded live data).
	// This prevents shadowing the fixture store's real payloads when simulator is fresh.
	if len(state.Markets) == 0 {
		return nil, false
	}

	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}

	var instruments []map[string]interface{}
	for symbol, market := range state.Markets {
		// Parse instrument ID to extract base and quote currencies
		// OKX format: "BTC-USDT-SWAP" for swaps
		parts := strings.Split(symbol, "-")
		var baseCcy, quoteCcy, instType string
		if len(parts) >= 3 && parts[len(parts)-1] == "SWAP" {
			// Swap market: "BTC-USDT-SWAP"
			baseCcy = parts[0]
			quoteCcy = parts[1]
			instType = "SWAP"
		} else if len(parts) >= 2 {
			// Spot or futures: "BTC-USDT"
			baseCcy = parts[0]
			quoteCcy = parts[1]
			instType = "SPOT" // Default to SPOT; could be FUTURES if datepart exists
		} else {
			baseCcy = symbol
			quoteCcy = "USDT"
			instType = "SWAP"
		}

		// OKX fields required by CCXT parseMarket for swap markets
		instruments = append(instruments, map[string]interface{}{
			"instId":    symbol,                                    // Market ID (e.g., "BTC-USDT-SWAP")
			"instType":  instType,                                  // Type: SPOT, FUTURES, SWAP, OPTION
			"baseCcy":   baseCcy,                                   // Base currency
			"quoteCcy":  quoteCcy,                                  // Quote currency
			"settleCcy": quoteCcy,                                  // Settlement currency (USDT for USDT swaps)
			"ctVal":     formatNumber(market.ContractSize),         // Contract value (size in base per contract)
			"ctValCcy":  baseCcy,                                   // Contract value currency
			"ctMult":    "1",                                       // Contract multiplier (1 for linear)
			"ctType":    "linear",                                  // Contract type: linear, inverse
			"state":     "live",                                    // Market state
			"lever":     formatNumber(float64(market.MaxLeverage)), // Max leverage
			"tickSz":    "0.01",                                    // Tick size (price precision)
			"lotSz":     formatNumber(market.MinAmount),            // Lot size (minimum order size)
			"minSz":     formatNumber(market.MinAmount),            // Minimum size
		})
	}

	respBody, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": instruments,
	})
	return &Response{
		Status: 200,
		Body:   json.RawMessage(respBody),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}, true
}

// -- Helper functions --

// formatNumber formats a number as a string without unnecessary decimals.
func formatNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	return s
}

// binanceOrderToWireFormat converts an Order to Binance wire format (map).
// CRITICAL: orderId is a JSON NUMBER (not string), status/side/type are uppercase strings,
// all prices/quantities are strings. This format must match exactly for CCXT to parse.
func (sim *StatefulSimulator) binanceOrderToWireFormat(order *Order) map[string]interface{} {
	// Parse orderId to int64 (Binance returns numeric orderId in JSON)
	var orderIDInt int64
	if parsed, err := strconv.ParseInt(order.ID, 10, 64); err == nil {
		orderIDInt = parsed
	}

	return map[string]interface{}{
		"orderId":      orderIDInt, // NUMBER type, not string
		"symbol":       order.Symbol,
		"status":       order.Status, // "FILLED", "NEW", "CANCELED", etc.
		"origQty":      formatNumber(order.Amount),
		"executedQty":  formatNumber(order.Filled),
		"avgPrice":     formatNumber(order.Average),
		"positionSide": strings.ToUpper(order.Params["positionSide"].(string)),
		"side":         strings.ToUpper(order.Side),      // "BUY" or "SELL"
		"type":         strings.ToUpper(order.OrderType), // "MARKET" or "LIMIT"
		"timeInForce":  "GTC",
		"time":         order.CreatedAt.UnixMilli(),
		"updateTime":   order.UpdatedAt.UnixMilli(),
	}
}

// binancePositionToWireFormat converts a Position to Binance wire format.
func (sim *StatefulSimulator) binancePositionToWireFormat(pos *Position) map[string]interface{} {
	return map[string]interface{}{
		"symbol":           pos.Symbol,
		"positionSide":     strings.ToUpper(pos.Side),
		"positionAmt":      formatNumber(pos.Contracts * pos.ContractSize),
		"initialMargin":    "0",
		"maintMargin":      "0",
		"unrealizedProfit": "0",
		"realizedProfit":   "0",
		"leverage":         pos.Leverage,
		"maxNotionalValue": "0",
		"marginType":       "cross",
		"isolatedCreated":  false,
		"updateTime":       pos.UpdatedAt.UnixMilli(),
	}
}

// buildBinanceOrderResponseBody builds a Binance order response in wire format.
func (sim *StatefulSimulator) buildBinanceOrderResponseBody(order *Order) json.RawMessage {
	resp := sim.binanceOrderToWireFormat(order)
	body, _ := json.Marshal(resp)
	return json.RawMessage(body)
}

// binanceErrorResponse builds a Binance error response.
func (sim *StatefulSimulator) binanceErrorResponse(httpStatus int, code int, msg string) *Response {
	body, _ := json.Marshal(map[string]interface{}{
		"code": code,
		"msg":  msg,
	})
	return &Response{
		Status: httpStatus,
		Body:   json.RawMessage(body),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
}

// okxOrderToWireFormat converts an Order to OKX wire format.
// CRITICAL: includes sCode (per-order success code) for CCXT error detection.
// CRITICAL: uses "state" field name (not "status") - CCXT expects OKX state values like "filled", "live", "canceled"
func (sim *StatefulSimulator) okxOrderToWireFormat(order *Order) map[string]interface{} {
	return map[string]interface{}{
		"ordId":   order.ID,
		"instId":  order.Symbol,
		"side":    order.Side,
		"posSide": order.Params["posSide"],
		"sz":      formatNumber(order.Amount),
		"state":   order.Status, // OKX uses "state" not "status"; value like "filled", "live", "canceled"
		"avgPx":   formatNumber(order.Average),
		"sCode":   "0", // Per-order success code (0 = success)
		"sMsg":    "",  // Per-order success message
	}
}

// okxPositionToWireFormat converts a Position to OKX wire format.
func (sim *StatefulSimulator) okxPositionToWireFormat(pos *Position) map[string]interface{} {
	return map[string]interface{}{
		"instId":  pos.Symbol,
		"posSide": pos.Side,
		"pos":     formatNumber(pos.Contracts),
		"avgPx":   formatNumber(pos.EntryPrice),
		"markPx":  formatNumber(pos.MarkPrice),
		"lever":   pos.Leverage,
		"uPL":     "0",
	}
}

// buildOKXOrderResponseBody builds an OKX createOrder response in wire format.
// CRITICAL: each element in data[] must include sCode for CCXT to detect per-order errors.
// Wire format per OKX API spec for POST /api/v5/trade/order response.
// Note: createOrder response only includes clOrdId, ordId, tag, sCode, sMsg.
// Status/state is only available via FetchOrder, not in CreateOrder response.
func (sim *StatefulSimulator) buildOKXOrderResponseBody(ordId, instId, side, posSide string, sz, price float64) json.RawMessage {
	// Ignore instId, side, posSide, sz, price - they're not in the real createOrder response
	body, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"msg":  "",
		"data": []map[string]interface{}{
			{
				"ordId":   ordId,
				"sCode":   "0", // Per-order success code (CRITICAL for CCXT)
				"sMsg":    "",  // Per-order message
				"clOrdId": "",  // Client order ID (optional)
				"tag":     "",  // Tag (optional)
			},
		},
	})
	return json.RawMessage(body)
}

// buildOKXCancelResponseBody builds an OKX cancel response.
func (sim *StatefulSimulator) buildOKXCancelResponseBody(ordId, instId string) json.RawMessage {
	body, _ := json.Marshal(map[string]interface{}{
		"code": "0",
		"data": []map[string]interface{}{
			{
				"ordId":  ordId,
				"instId": instId,
			},
		},
	})
	return json.RawMessage(body)
}

// okxErrorResponse builds an OKX error response.
func (sim *StatefulSimulator) okxErrorResponse(code, msg string) *Response {
	body, _ := json.Marshal(map[string]interface{}{
		"code": code,
		"msg":  msg,
	})
	return &Response{
		Status: 200, // OKX uses HTTP 200 with error code in body
		Body:   json.RawMessage(body),
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
	}
}

// SimulatorOwnedPath reports whether the stateful simulator unconditionally answers a request
// for the given venue, method, and path. When true, any fixture authored for that route is
// shadowed and has no effect. Only returns true for routes that ALWAYS answer (never fall through).
//
// NOTE: exchangeInfo (Binance) and instruments (OKX) are NOT included even though the simulator
// has handlers for them. Those routes explicitly fall through to the fixture store when markets
// are empty: `if len(state.Markets) == 0 { return nil, false }`. They are load-bearing fixtures
// used by SeedMarketsFromFixtures, so they must remain reachable by the fixture store.
func SimulatorOwnedPath(venue, method, path string) bool {
	switch venue {
	case "binance":
		// Binance futures endpoints: /fapi/v1/ and /fapi/v3/
		switch {
		// Create order
		case method == "POST" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
			return true
		// Fetch order
		case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
			return true
		// Cancel order
		case method == "DELETE" && (strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
			return true
		// Fetch open orders
		case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/openOrders") || strings.HasPrefix(path, "/fapi/v3/openOrders")):
			return true
		// Fetch positions
		case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/positionRisk") || strings.HasPrefix(path, "/fapi/v3/positionRisk")):
			return true
		// Fetch account (balance)
		case method == "GET" && (strings.HasPrefix(path, "/fapi/v1/account") || strings.HasPrefix(path, "/fapi/v3/account")):
			return true
		// Set leverage
		case method == "POST" && (strings.HasPrefix(path, "/fapi/v1/leverage") || strings.HasPrefix(path, "/fapi/v3/leverage")):
			return true
		default:
			return false
		}

	case "okx":
		// OKX perpetuals endpoints: /api/v5/
		switch {
		// Create order
		case method == "POST" && (strings.HasPrefix(path, "/api/v5/trade/order") || strings.HasPrefix(path, "/api/v5/trade/batch-orders")):
			return true
		// Fetch order
		case method == "GET" && strings.HasPrefix(path, "/api/v5/trade/order"):
			return true
		// Cancel order
		case method == "POST" && strings.HasPrefix(path, "/api/v5/trade/cancel-order"):
			return true
		// Fetch open orders
		case method == "GET" && strings.HasPrefix(path, "/api/v5/trade/orders-pending"):
			return true
		// Fetch positions (exact match: /api/v5/account/positions, not /api/v5/account/positions-history)
		case method == "GET" && path == "/api/v5/account/positions":
			return true
		// Fetch balance
		case method == "GET" && strings.HasPrefix(path, "/api/v5/account/balance"):
			return true
		// Set leverage
		case method == "POST" && strings.HasPrefix(path, "/api/v5/account/set-leverage"):
			return true
		default:
			return false
		}

	default:
		return false
	}
}
