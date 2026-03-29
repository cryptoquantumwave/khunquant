package bitkub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
)

// Name is the canonical identifier for this exchange.
const Name = "bitkub"

// BitkubExchange implements exchanges.PricedExchange using Bitkub REST API.
type BitkubExchange struct {
	apiKey    string
	apiSecret string
	client    *http.Client
}

// NewBitkubExchange creates a new BitkubExchange using resolved credentials.
func NewBitkubExchange(creds config.ExchangeAccount) (*BitkubExchange, error) {
	if creds.APIKey == "" || creds.Secret == "" {
		return nil, fmt.Errorf("bitkub: api_key and secret are required")
	}
	return &BitkubExchange{
		apiKey:    creds.APIKey,
		apiSecret: creds.Secret,
		client:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Name returns the exchange identifier.
func (b *BitkubExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (b *BitkubExchange) SupportedWalletTypes() []string {
	return []string{"spot"}
}

// SupportedQuotes implements exchanges.QuoteLister.
func (b *BitkubExchange) SupportedQuotes() []string {
	return []string{"THB", "USDT"}
}

// GetBalances implements the basic Exchange interface.
func (b *BitkubExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
	wb, err := b.getSpotBalances(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]exchanges.Balance, len(wb))
	for i, w := range wb {
		out[i] = w.Balance
	}
	return out, nil
}

// GetWalletBalances implements WalletExchange.
func (b *BitkubExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	switch walletType {
	case "spot":
		return b.getSpotBalances(ctx)
	default:
		return nil, fmt.Errorf("bitkub: unsupported wallet type %q (supported: %v)", walletType, b.SupportedWalletTypes())
	}
}

// FetchPrice implements PricedExchange.
// Bitkub only lists pairs against THB (e.g. BTC_THB). When a different quote
// currency is requested (e.g. USDT), the price is converted via THB as an
// intermediate: price_in_quote = asset_THB / quote_THB.
// Returns (0, nil) when asset == quote (face value).
func (b *BitkubExchange) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	upper := strings.ToUpper(asset)
	upperQuote := strings.ToUpper(quote)

	if upper == upperQuote {
		return 0, nil
	}

	tickers, err := b.fetchTickers(ctx)
	if err != nil {
		return 0, err
	}

	// Fast path: direct pair exists (e.g. BTC_THB when quote is THB).
	if price, ok := tickers[upper+"_"+upperQuote]; ok {
		return price, nil
	}

	// Bitkub only quotes in THB — convert via THB as intermediate.
	// Special case: asset IS THB → 1 THB = 1/quote_THB_rate quote units.
	if upper == "THB" {
		if quoteTHBRate, ok := tickers[upperQuote+"_THB"]; ok && quoteTHBRate > 0 {
			return 1.0 / quoteTHBRate, nil
		}
		return 0, fmt.Errorf("bitkub: no %s_THB pair to convert THB to %s", upperQuote, upperQuote)
	}

	// General case: asset_THB / quote_THB = asset price in requested quote.
	assetTHB, hasAsset := tickers[upper+"_THB"]
	if !hasAsset {
		return 0, fmt.Errorf("bitkub: no %s_THB pair available", upper)
	}

	// If quote IS THB no conversion needed.
	if upperQuote == "THB" {
		return assetTHB, nil
	}

	quoteTHB, hasQuote := tickers[upperQuote+"_THB"]
	if !hasQuote || quoteTHB == 0 {
		return 0, fmt.Errorf("bitkub: no %s_THB pair to convert from THB to %s", upperQuote, upperQuote)
	}

	return assetTHB / quoteTHB, nil
}

// ---- Response types ----

type balancesResponse struct {
	Error  int                    `json:"error"`
	Result map[string]assetBalance `json:"result"`
}

type assetBalance struct {
	Available float64 `json:"available"`
	Reserved  float64 `json:"reserved"`
}

// tickerEntry holds all fields from GET /api/v3/market/ticker.
// The endpoint returns a plain JSON array (no envelope) when sym is omitted.
type tickerEntry struct {
	Symbol        string `json:"symbol"`
	Last          string `json:"last"`
	LowestAsk     string `json:"lowest_ask"`
	HighestBid    string `json:"highest_bid"`
	PercentChange string `json:"percent_change"`
	BaseVolume    string `json:"base_volume"`
	QuoteVolume   string `json:"quote_volume"`
	High24Hr      string `json:"high_24_hr"`
	Low24Hr       string `json:"low_24_hr"`
}

type depthResponse struct {
	Error  int `json:"error"`
	Result struct {
		Asks [][]float64 `json:"asks"`
		Bids [][]float64 `json:"bids"`
	} `json:"result"`
}

type symbolEntry struct {
	Symbol     string  `json:"symbol"`
	Name       string  `json:"name"`
	BaseAsset  string  `json:"base_asset"`
	QuoteAsset string  `json:"quote_asset"`
	MinQuote   float64 `json:"min_quote_size"`
	Status     string  `json:"status"`
}

type symbolsResponse struct {
	Error  int           `json:"error"`
	Result []symbolEntry `json:"result"`
}

// bitkubOrder is the unified order shape returned by open-orders, order-history, order-info,
// place-bid, place-ask. Field names match Bitkub's compact API keys.
type bitkubOrder struct {
	ID  string `json:"id"`
	Sym string `json:"sym"`
	Sd  string `json:"sd"`  // "buy" | "sell"
	Typ string `json:"typ"` // "limit" | "market"
	Rat string `json:"rat"` // limit price
	Amt string `json:"amt"` // amount remaining (THB for buy, base for sell)
	Rec string `json:"rec"` // received (base for buy, THB for sell)
	Fee string `json:"fee"`
	Ts  string `json:"ts"` // Unix seconds as string
	St  string `json:"st"` // "open" | "filled" | "cancelled" (only in order-info)
	Ci  string `json:"ci"` // client_id
}

type bitkubFill struct {
	Amount    string `json:"amount"`
	Fee       string `json:"fee"`
	ID        string `json:"id"`
	Rate      string `json:"rate"`
	Timestamp int64  `json:"timestamp"`
}

type openOrdersResponse struct {
	Error  int           `json:"error"`
	Result []bitkubOrder `json:"result"`
}

type orderHistoryResponse struct {
	Error  int           `json:"error"`
	Result []bitkubOrder `json:"result"`
}

type orderInfoResponse struct {
	Error  int `json:"error"`
	Result struct {
		bitkubOrder
		History []bitkubFill `json:"history"`
	} `json:"result"`
}

type placeOrderResponse struct {
	Error  int         `json:"error"`
	Result bitkubOrder `json:"result"`
}

// ---- Internal fetch methods ----

// getSpotBalances fetches all spot balances from POST /api/v3/market/balances.
func (b *BitkubExchange) getSpotBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	var resp balancesResponse
	if err := b.privatePost(ctx, endpointBalances, nil, &resp); err != nil {
		return nil, fmt.Errorf("spot: %w", err)
	}
	if resp.Error != 0 {
		return nil, fmt.Errorf("spot: bitkub error code %d", resp.Error)
	}

	var out []exchanges.WalletBalance
	for asset, bal := range resp.Result {
		if bal.Available == 0 && bal.Reserved == 0 {
			continue
		}
		out = append(out, exchanges.WalletBalance{
			Balance:    exchanges.Balance{Asset: asset, Free: bal.Available, Locked: bal.Reserved},
			WalletType: "spot",
		})
	}
	return out, nil
}

// fetchRichTickers returns all tickers with full market data keyed by symbol (e.g. "BTC_THB").
func (b *BitkubExchange) fetchRichTickers(ctx context.Context) (map[string]tickerEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpointTicker, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitkub: ticker request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bitkub: reading ticker response: %w", err)
	}

	var entries []tickerEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("bitkub: parsing ticker response: %w", err)
	}

	out := make(map[string]tickerEntry, len(entries))
	for _, e := range entries {
		out[e.Symbol] = e
	}
	return out, nil
}

// fetchTickers returns a map of symbol → last price (used by FetchPrice).
func (b *BitkubExchange) fetchTickers(ctx context.Context) (map[string]float64, error) {
	rich, err := b.fetchRichTickers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rich))
	for sym, e := range rich {
		last, err := strconv.ParseFloat(e.Last, 64)
		if err != nil {
			continue
		}
		out[sym] = last
	}
	return out, nil
}

// fetchDepth returns the order book for sym with the given number of levels per side.
func (b *BitkubExchange) fetchDepth(ctx context.Context, sym string, limit int) (depthResponse, error) {
	params := url.Values{}
	params.Set("sym", strings.ToLower(sym))
	params.Set("lmt", strconv.Itoa(limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+endpointDepth+"?"+params.Encode(), nil)
	if err != nil {
		return depthResponse{}, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return depthResponse{}, fmt.Errorf("bitkub: depth request failed: %w", err)
	}
	defer resp.Body.Close()

	var out depthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return depthResponse{}, fmt.Errorf("bitkub: parsing depth response: %w", err)
	}
	return out, nil
}

// fetchSymbols returns all listed trading pairs.
func (b *BitkubExchange) fetchSymbols(ctx context.Context) ([]symbolEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpointSymbols, nil)
	if err != nil {
		return nil, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bitkub: symbols request failed: %w", err)
	}
	defer resp.Body.Close()

	var out symbolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("bitkub: parsing symbols response: %w", err)
	}
	if out.Error != 0 {
		return nil, fmt.Errorf("bitkub: symbols error code %d", out.Error)
	}
	return out.Result, nil
}

// fetchMyOpenOrders returns all open orders for the given symbol (authenticated).
func (b *BitkubExchange) fetchMyOpenOrders(ctx context.Context, sym string) ([]bitkubOrder, error) {
	params := map[string]string{"sym": strings.ToLower(sym)}
	var resp openOrdersResponse
	if err := b.privateGet(ctx, endpointMyOpenOrders, params, &resp); err != nil {
		return nil, fmt.Errorf("bitkub: open orders: %w", err)
	}
	if resp.Error != 0 {
		return nil, fmt.Errorf("bitkub: open orders error code %d", resp.Error)
	}
	return resp.Result, nil
}

// fetchOrderHistory returns filled/cancelled orders for the given symbol (authenticated).
// page is 1-based; limit defaults to 20 if ≤0.
func (b *BitkubExchange) fetchOrderHistory(ctx context.Context, sym string, page, limit int) ([]bitkubOrder, error) {
	if limit <= 0 {
		limit = 20
	}
	params := map[string]string{
		"sym": strings.ToLower(sym),
		"p":   strconv.Itoa(page),
		"lmt": strconv.Itoa(limit),
	}
	var resp orderHistoryResponse
	if err := b.privateGet(ctx, endpointOrderHistory, params, &resp); err != nil {
		return nil, fmt.Errorf("bitkub: order history: %w", err)
	}
	if resp.Error != 0 {
		return nil, fmt.Errorf("bitkub: order history error code %d", resp.Error)
	}
	return resp.Result, nil
}

// fetchOrderInfo returns details for a single order (authenticated).
func (b *BitkubExchange) fetchOrderInfo(ctx context.Context, sym, id, side string) (bitkubOrder, []bitkubFill, error) {
	params := map[string]string{
		"sym": strings.ToLower(sym),
		"id":  id,
		"sd":  side,
	}
	var resp orderInfoResponse
	if err := b.privateGet(ctx, endpointOrderInfo, params, &resp); err != nil {
		return bitkubOrder{}, nil, fmt.Errorf("bitkub: order info: %w", err)
	}
	if resp.Error != 0 {
		return bitkubOrder{}, nil, fmt.Errorf("bitkub: order info error code %d", resp.Error)
	}
	return resp.Result.bitkubOrder, resp.Result.History, nil
}

// placeBid submits a buy order. amt is in THB (quote currency), rat is the price.
// For market orders pass rat=0.
func (b *BitkubExchange) placeBid(ctx context.Context, sym string, amt, rat float64, orderType string) (bitkubOrder, error) {
	payload := map[string]interface{}{
		"sym": strings.ToLower(sym),
		"amt": amt,
		"rat": rat,
		"typ": orderType,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return bitkubOrder{}, err
	}
	var resp placeOrderResponse
	if err := b.privatePost(ctx, endpointPlaceBid, body, &resp); err != nil {
		return bitkubOrder{}, fmt.Errorf("bitkub: place bid: %w", err)
	}
	if resp.Error != 0 {
		return bitkubOrder{}, fmt.Errorf("bitkub: place bid error code %d", resp.Error)
	}
	resp.Result.Sd = "buy"
	resp.Result.Sym = sym
	return resp.Result, nil
}

// placeAsk submits a sell order. amt is in base currency (e.g. BTC), rat is the price in THB.
// For market orders pass rat=0.
func (b *BitkubExchange) placeAsk(ctx context.Context, sym string, amt, rat float64, orderType string) (bitkubOrder, error) {
	payload := map[string]interface{}{
		"sym": strings.ToLower(sym),
		"amt": amt,
		"rat": rat,
		"typ": orderType,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return bitkubOrder{}, err
	}
	var resp placeOrderResponse
	if err := b.privatePost(ctx, endpointPlaceAsk, body, &resp); err != nil {
		return bitkubOrder{}, fmt.Errorf("bitkub: place ask: %w", err)
	}
	if resp.Error != 0 {
		return bitkubOrder{}, fmt.Errorf("bitkub: place ask error code %d", resp.Error)
	}
	resp.Result.Sd = "sell"
	resp.Result.Sym = sym
	return resp.Result, nil
}

// cancelBitkubOrder cancels an open order. side must be "buy" or "sell".
func (b *BitkubExchange) cancelBitkubOrder(ctx context.Context, sym, id, side string) error {
	payload := map[string]interface{}{
		"sym": strings.ToLower(sym),
		"id":  id,
		"sd":  side,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var resp struct {
		Error int `json:"error"`
	}
	if err := b.privatePost(ctx, endpointCancelOrder, body, &resp); err != nil {
		return fmt.Errorf("bitkub: cancel order: %w", err)
	}
	if resp.Error != 0 {
		return fmt.Errorf("bitkub: cancel order error code %d", resp.Error)
	}
	return nil
}

// orderToCCXT converts a Bitkub order to a ccxt.Order.
// Bitkub amount semantics differ by side:
//   - buy: amt = THB remaining, rec = base received → base_amount ≈ rec + amt/rat
//   - sell: amt = base remaining, rec = THB received → base_amount ≈ amt + rec/rat
func (b *BitkubExchange) orderToCCXT(o bitkubOrder) ccxt.Order {
	id := o.ID
	sym := strings.ReplaceAll(strings.ToUpper(o.Sym), "_", "/")
	side := o.Sd
	typ := o.Typ

	rat, _ := strconv.ParseFloat(o.Rat, 64)
	amt, _ := strconv.ParseFloat(o.Amt, 64)
	rec, _ := strconv.ParseFloat(o.Rec, 64)
	feeAmt, _ := strconv.ParseFloat(o.Fee, 64)

	tsInt, _ := strconv.ParseInt(o.Ts, 10, 64)
	tsMs := tsInt * 1000

	var amount, filled float64
	if side == "buy" {
		// amt = THB remaining, rec = base received
		if rat > 0 {
			amount = rec + amt/rat
		} else {
			amount = rec
		}
		filled = rec
	} else {
		// amt = base remaining, rec = THB received
		if rat > 0 {
			amount = amt + rec/rat
		} else {
			amount = amt
		}
		filled = amount - amt
		if filled < 0 {
			filled = 0
		}
	}

	remaining := amount - filled
	if remaining < 0 {
		remaining = 0
	}

	status := o.St
	switch status {
	case "filled":
		status = "closed"
	case "cancelled":
		status = "canceled"
	case "":
		if filled > 0 && remaining < 1e-10 {
			status = "closed"
		} else {
			status = "open"
		}
	}

	return ccxt.Order{
		Id:        &id,
		Symbol:    &sym,
		Type:      &typ,
		Side:      &side,
		Price:     &rat,
		Amount:    &amount,
		Filled:    &filled,
		Remaining: &remaining,
		Status:    &status,
		Timestamp: &tsMs,
		Fee:       ccxt.Fee{Cost: &feeAmt},
		Info: map[string]interface{}{
			"id": o.ID, "sym": o.Sym, "sd": o.Sd, "typ": o.Typ,
			"rat": o.Rat, "amt": o.Amt, "rec": o.Rec, "fee": o.Fee,
			"ts": o.Ts, "st": o.St, "ci": o.Ci,
		},
	}
}

// ---- HTTP helpers ----

// privateGet sends a signed GET request with query parameters to a private Bitkub endpoint.
func (b *BitkubExchange) privateGet(ctx context.Context, path string, params map[string]string, out interface{}) error {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := b.sign(ts, http.MethodGet, path, "")

	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	urlStr := baseURL + path
	if len(q) > 0 {
		urlStr += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-BTK-APIKEY", b.apiKey)
	req.Header.Set("X-BTK-TIMESTAMP", ts)
	req.Header.Set("X-BTK-SIGN", sig)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.Unmarshal(respBody, out)
}

// privatePost sends a signed POST request to a private Bitkub endpoint.
// body may be nil for requests with no payload.
func (b *BitkubExchange) privatePost(ctx context.Context, path string, body []byte, out interface{}) error {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)

	bodyStr := ""
	if len(body) > 0 {
		bodyStr = string(body)
	}

	sig := b.sign(ts, http.MethodPost, path, bodyStr)

	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BTK-APIKEY", b.apiKey)
	req.Header.Set("X-BTK-TIMESTAMP", ts)
	req.Header.Set("X-BTK-SIGN", sig)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return json.Unmarshal(respBody, out)
}

// sign computes the HMAC-SHA256 signature for a Bitkub API request.
// Payload: timestamp + METHOD + path + body
func (b *BitkubExchange) sign(timestamp, method, path, body string) string {
	payload := timestamp + method + path + body
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
