package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
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
	prices       PriceSource      // optional; guarded by mu
	now          func() time.Time // order and fill times; nil is time.Now; guarded by mu

	// A locked lab sets these (sim_effects.go); a bare simulator has none. Guarded by mu.
	effects OrderEffects
	journal func(kind, subject string, payload any)
	wake    func(at time.Time)
	wakeAt  time.Time // the wake armed and not yet due
}

// SetClock makes order, fill and account timestamps read now (the lab passes its sim
// clock, so they match the candle times). A nil now restores time.Now.
func (sim *StatefulSimulator) SetClock(now func() time.Time) {
	sim.mu.Lock()
	sim.now = now
	sim.mu.Unlock()
}

// clock is the simulator's current time. Callers hold sim.mu.
func (sim *StatefulSimulator) clock() time.Time {
	if sim.now != nil {
		return sim.now()
	}
	//simclock:wall the default when no clock is injected
	return time.Now()
}

// PriceSource supplies the price a venue's wire symbol has now (SimMarket's Book). The
// simulator pulls it while holding sim.mu, so an implementation must never call back
// into the simulator: the lock order is sim.mu then the source's own.
type PriceSource interface {
	MarkPrice(venue, wireSymbol string) (float64, bool)
}

// SetPriceSource makes fills and positions use src's prices where it has them; nil
// restores the seeded state.MarkPrices alone.
func (sim *StatefulSimulator) SetPriceSource(src PriceSource) {
	sim.mu.Lock()
	sim.prices = src
	sim.mu.Unlock()
}

// markPrice is the price of symbol: the source's when it has one (written through to
// state.MarkPrices), else state.MarkPrices. Callers hold sim.mu.
func (sim *StatefulSimulator) markPrice(venue, symbol string, state *VenueState) float64 {
	if sim.prices != nil {
		if p, ok := sim.prices.MarkPrice(venue, symbol); ok && p > 0 {
			state.MarkPrices[symbol] = p
			return p
		}
	}
	return state.MarkPrices[symbol]
}

// refreshMarks brings each open position's mark price up to date.
func (sim *StatefulSimulator) refreshMarks(venue string, state *VenueState) {
	for symbol, pos := range state.Positions {
		if p := sim.markPrice(venue, symbol, state); p > 0 {
			pos.MarkPrice = p
		}
	}
}

// Mutate runs fn on the venue's state under the lock Respond takes, so a lab shortcut
// never races an order in flight. It looks the state up on every call (Reset swaps it),
// and fn must neither keep the *VenueState nor call back into the simulator.
func (sim *StatefulSimulator) Mutate(venue string, fn func(*VenueState) error) error {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sim.advanceLocked(time.Time{}) // what is due happens before the change, not after it
	err := fn(sim.stateManager.GetState(venue))
	sim.reschedule(sim.clock()) // a position the change added settles funding
	return err
}

// Read runs fn on the venue's state under the read lock. fn must copy what it keeps:
// the maps hold pointers Respond mutates once the lock is released.
func (sim *StatefulSimulator) Read(venue string, fn func(*VenueState)) {
	sim.mu.RLock()
	defer sim.mu.RUnlock()
	fn(sim.stateManager.GetState(venue))
}

// Rebaseline makes the venue's current state the one a reset returns to. It holds the
// simulator lock because SnapshotAsSeed copies the maps Respond writes under it.
func (sim *StatefulSimulator) Rebaseline(venue string) {
	sim.mu.Lock()
	defer sim.mu.Unlock()
	sim.stateManager.SnapshotAsSeed(venue)
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
		sim.advanceLocked(time.Time{}) // what fell due since the last request
		return sim.respondBinance(method, path, r)
	case "okx":
		sim.advanceLocked(time.Time{})
		return sim.respondOKX(method, path, r)
	default:
		return nil, false // fall through to fixture store
	}
}

// -- Binance Futures Endpoints (/fapi/v1/ and /fapi/v3/) --

func (sim *StatefulSimulator) respondBinance(method, path string, r *http.Request) (*Response, bool) {
	state := sim.stateManager.GetState("binance")

	// The order endpoints match the whole path (v1 or v3, or the spot API); the rest by
	// prefix, as before. The futures wallet is the "binance" state, the spot one "binance.spot".
	key, ep := binanceEndpoint(path)
	switch {
	case method == "POST" && ep == "order":
		return sim.simBinanceCreateOrder(key, r)
	case method == "GET" && ep == "order":
		return sim.simBinanceFetchOrder(key, r)
	case method == "DELETE" && ep == "order":
		return sim.simBinanceCancelOrder(key, r)
	case method == "GET" && (ep == "openOrders" || ep == "allOrders"):
		return sim.simBinanceListOrders(key, r, ep == "allOrders")
	case method == "GET" && key == binanceSpot && ep == "myTrades":
		return sim.simBinanceMyTrades(r)
	// The spot account is the simulator's once its wallet has been seeded or traded; before
	// that the recorded fixture answers, as it always did.
	case method == "GET" && key == binanceSpot && ep == "account" && sim.spotLive():
		return sim.simBinanceSpotAccount(r)
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
	case method == "GET" && path == "/fapi/v1/income":
		return sim.simBinanceIncome(state, r)
	case method == "GET" && path == "/fapi/v1/forceOrders":
		return sim.simBinanceForceOrders(state, r)
	case method == "POST" && key == "binance" && ep == "marginType":
		return sim.simBinanceAccountSetting(state, r)
	case method == "GET" && key == "binance" && ep == "leverageBracket":
		return sim.simBinanceLeverageBracket(state, r)
	default:
		return nil, false // fall through to fixture store
	}
}

// simBinanceFetchPositions handles GET /fapi/v1/positionRisk and /fapi/v3/positionRisk for Binance.
func (sim *StatefulSimulator) simBinanceFetchPositions(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}

	rs := sim.risksNow("binance", state)
	var positions []map[string]interface{}
	for _, sym := range slices.Sorted(maps.Keys(state.Positions)) {
		if pos := state.Positions[sym]; pos.Contracts > 0 {
			positions = append(positions, sim.binancePositionToWireFormat(pos, rs[sym]))
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

	// Every position's profit and maintenance margin come from the same maths as positionRisk
	// (margin.go); the USDT wallet is the settlement asset.
	rs := sim.risksNow("binance", state)
	var positions []FuturesPosition
	upnl, maint := 0.0, 0.0
	for _, sym := range slices.Sorted(maps.Keys(state.Positions)) {
		if pos := state.Positions[sym]; pos.Contracts > 0 {
			r := rs[sym]
			upnl, maint = upnl+r.UPnL, maint+r.Maint
			margin := formatNumber(math.Abs(pos.base()) * pos.MarkPrice / float64(max(pos.Leverage, 1)))
			positions = append(positions, FuturesPosition{
				Symbol:                   pos.Symbol,
				InitialMargin:            margin,
				MaintMargin:              formatNumber(r.Maint),
				OpenOrderInitialMargin:   "0",
				PositionInitialMargin:    margin,
				Positionamt:              signedAmt(pos), // contracts x contractSize, in base currency
				SymbolPrice:              formatNumber(pos.MarkPrice),
				UnrealizedProfit:         formatNumber(r.UPnL),
				MicroUnitAmountRemaining: 0,
				BracketPosition:          0,
			})
		}
	}

	var assets []FuturesAsset
	totalWalletBalance := 0.0
	availableBalance := 0.0
	for _, asset := range slices.Sorted(maps.Keys(state.Balances)) {
		bal := state.Balances[asset]
		total := bal.Free + bal.Locked
		totalWalletBalance += total
		availableBalance += bal.Free
		a := FuturesAsset{
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
		}
		if asset == "USDT" {
			a.MarginBalance, a.MaintenanceMargin = formatNumber(total+upnl), formatNumber(maint)
			a.CrossUnPnl, a.UnrealizedProfit = formatNumber(upnl), formatNumber(upnl)
		}
		assets = append(assets, a)
	}

	// Binance futures account response
	resp := map[string]interface{}{
		"assets":                       assets,
		"positions":                    positions,
		"totalWalletBalance":           formatNumber(totalWalletBalance),
		"availableBalance":             formatNumber(availableBalance),
		"totalUnrealizedProfit":        formatNumber(upnl),
		"totalMaintMargin":             formatNumber(maint),
		"totalMarginBalance":           formatNumber(totalWalletBalance + upnl),
		"totalMarginRequired":          "0",
		"totalPositionInitialMargin":   "0",
		"totalOpenOrderInitialMargin":  "0",
		"totalCrossWalletBalance":      formatNumber(totalWalletBalance),
		"totalCrossUnPnl":              formatNumber(upnl),
		"availableBalanceForOpenOrder": formatNumber(availableBalance),
		"canDeposit":                   true,
		"canTrade":                     true,
		"canWithdraw":                  true,
		"feeTier":                      0,
		"updateTime":                   sim.clock().UnixMilli(),
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

// okxOrderNotFound is the code ccxt maps to OrderNotFound (broker.IsOrderNotFound); the
// 51014 this used to answer is no such class.
const okxOrderNotFound = "51603"

func (sim *StatefulSimulator) respondOKX(method, path string, r *http.Request) (*Response, bool) {
	state := sim.stateManager.GetState("okx")

	// OKX uses query params for GET and body for POST.
	switch {
	// The order routes match the whole path: a prefix match also took orders-pending,
	// orders-history and order-algo.
	// Create futures order (single or batch)
	case method == "POST" && (path == "/api/v5/trade/order" || path == "/api/v5/trade/batch-orders"):
		return sim.simOKXCreateOrder(state, r)
	// Conditional orders are not simulated: the venue's own refusal, not a fixture miss.
	case method == "POST" && path == "/api/v5/trade/order-algo":
		return sim.okxErrorResponse("51000", "Conditional (algo) orders are not supported by the sandbox"), true
	// Fetch order
	case method == "GET" && path == "/api/v5/trade/order":
		return sim.simOKXFetchOrder(state, r)
	// Cancel order
	case method == "POST" && path == "/api/v5/trade/cancel-order":
		return sim.simOKXCancelOrder(state, r)
	// Fetch open orders
	case method == "GET" && (path == "/api/v5/trade/orders-pending" || path == "/api/v5/trade/orders-history"):
		return sim.simOKXListOrders(state, r, path == "/api/v5/trade/orders-history")
	// Fetch positions (exact match: /api/v5/account/positions, not /api/v5/account/positions-history)
	case method == "GET" && path == "/api/v5/account/positions":
		return sim.simOKXFetchPositions(state, r)
	// Fetch balance
	case method == "GET" && strings.HasPrefix(path, "/api/v5/account/balance"):
		return sim.simOKXFetchBalance(state, r)
	// ccxt's fetchFundingHistory reads the archive; the bills are the same ledger
	case method == "GET" && (path == "/api/v5/account/bills" || path == "/api/v5/account/bills-archive"):
		return sim.simOKXBills(state, r)
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

// simOKXFetchPositions handles GET /api/v5/account/positions for OKX.
func (sim *StatefulSimulator) simOKXFetchPositions(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}

	rs := sim.risksNow("okx", state)
	var positions []map[string]interface{}
	for _, sym := range slices.Sorted(maps.Keys(state.Positions)) {
		if pos := state.Positions[sym]; pos.Contracts > 0 {
			positions = append(positions, sim.okxPositionToWireFormat(pos, rs[sym]))
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
	// The simulator's markets are the recorded swaps; spot instruments are not its to
	// answer, so they come from the fixture. Without this the swap-only answer shadowed
	// the recorded spot rows and ccxt could not load an OKX spot market.
	if r.URL.Query().Get("instType") == "SPOT" {
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

// positionSideOf is the order's Binance positionSide: what the request carried, BOTH
// (one-way mode) when it carried none. A stored order may have no params at all.
func positionSideOf(order *Order) string {
	for _, k := range [...]string{"posSide", "positionSide"} { // the ledger's key, or a hand-built order's
		if s, _ := order.Params[k].(string); s != "" {
			return strings.ToUpper(s)
		}
	}
	return "BOTH"
}

// binancePositionToWireFormat converts a Position to Binance wire format. r is its risk
// (margin.go); the zero value, for a position with no mark, shows nothing known.
func (sim *StatefulSimulator) binancePositionToWireFormat(pos *Position, r legRisk) map[string]interface{} {
	mode, iso, notional := "cross", "0", math.Abs(pos.base())*pos.MarkPrice
	if pos.isolated() {
		mode, iso = "isolated", formatNumber(r.Base)
	}
	return map[string]interface{}{
		"symbol":       pos.Symbol,
		"positionSide": strings.ToUpper(pos.Side),
		"positionAmt":  signedAmt(pos),
		// ccxt keeps a position only when its entryPrice is above zero, so a wire without
		// one reads back as no position at all (a short left open after a failed hedge
		// looked flat to the recovery that was meant to close it).
		"entryPrice": formatNumber(pos.EntryPrice),
		"markPrice":  formatNumber(pos.MarkPrice),
		"notional":   signedNotional(pos),
		// ccxt reads unRealizedProfit (capital R) and drops a liquidationPrice of 0; it derives
		// the margin ratio itself from the leverageBracket's rate and this liquidation price.
		"liquidationPrice": formatNumber(r.Liq),
		"initialMargin":    formatNumber(notional / float64(max(pos.Leverage, 1))),
		"maintMargin":      formatNumber(r.Maint),
		"unRealizedProfit": formatNumber(r.UPnL),
		"isolatedMargin":   iso,
		"isolatedWallet":   iso,
		"realizedProfit":   "0",
		"leverage":         pos.Leverage,
		"maxNotionalValue": "0",
		"marginType":       mode,
		"isolatedCreated":  pos.isolated(),
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

// okxPositionToWireFormat converts a Position to OKX wire format. r is its risk
// (margin.go). ccxt (v4.5.85) ignores mgnRatio and derives the ratio itself as mmr over
// the position's collateral: imr + upl for cross, margin for isolated. imr and margin
// therefore carry the cushion that backs the position (floored above zero), so that
// ratio is the fraction of it that maintenance takes, 1 at the liquidation price.
// mgnRatio is OKX's own convention, equity over maintenance, for a reader of the wire.
func (sim *StatefulSimulator) okxPositionToWireFormat(pos *Position, r legRisk) map[string]interface{} {
	mode, margin, imr := "cross", "", formatNumber(r.cushion()-r.UPnL)
	if pos.isolated() {
		mode, margin, imr = "isolated", formatNumber(r.cushion()), ""
	}
	liq, coverage := "", ""
	if r.Liq > 0 {
		liq = formatNumber(r.Liq)
	}
	if r.Maint > 0 {
		coverage = formatNumber(r.Equity() / r.Maint)
	}
	return map[string]interface{}{
		"instId":      pos.Symbol,
		"instType":    "SWAP",
		"ccy":         "USDT",
		"posSide":     pos.Side,
		"pos":         formatNumber(pos.Contracts),
		"avgPx":       formatNumber(pos.EntryPrice),
		"markPx":      formatNumber(pos.MarkPrice),
		"lever":       pos.Leverage,
		"upl":         formatNumber(r.UPnL),
		"liqPx":       liq,
		"mgnMode":     mode,
		"mgnRatio":    coverage,
		"mmr":         formatNumber(r.Maint),
		"imr":         imr,
		"margin":      margin,
		"notionalUsd": formatNumber(math.Abs(pos.base()) * pos.MarkPrice),
		"cTime":       strconv.FormatInt(pos.UpdatedAt.UnixMilli(), 10),
		"uTime":       strconv.FormatInt(pos.UpdatedAt.UnixMilli(), 10),
	}
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
		key, ep := binanceEndpoint(path)
		switch {
		// The order endpoints, by exact path (as respondBinance matches them)
		case (method == "POST" || method == "GET" || method == "DELETE") && ep == "order":
			return true
		case method == "GET" && (ep == "openOrders" || ep == "allOrders" || (key == binanceSpot && ep == "myTrades")):
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
		// The liquidation orders (income is not listed: it falls through while the ledger is empty)
		case method == "GET" && path == "/fapi/v1/forceOrders":
			return true
		default:
			return false
		}

	case "okx":
		// OKX perpetuals endpoints: /api/v5/
		switch {
		// The order routes, by exact path (as respondOKX matches them)
		case method == "POST" && (path == "/api/v5/trade/order" || path == "/api/v5/trade/batch-orders" || path == "/api/v5/trade/order-algo"):
			return true
		case method == "GET" && (path == "/api/v5/trade/order" || path == "/api/v5/trade/orders-pending" || path == "/api/v5/trade/orders-history"):
			return true
		case method == "POST" && path == "/api/v5/trade/cancel-order":
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
