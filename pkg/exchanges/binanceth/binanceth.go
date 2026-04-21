package binanceth

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

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// Name is the canonical identifier for this exchange.
const Name = "binanceth"

// BinanceTHExchange implements exchanges.PricedExchange using the Binance Thailand REST API.
type BinanceTHExchange struct {
	apiKey    string
	apiSecret string
	client    *http.Client
}

// NewBinanceTHExchange creates a new BinanceTHExchange using resolved credentials.
func NewBinanceTHExchange(creds config.ExchangeAccount) (*BinanceTHExchange, error) {
	if creds.APIKey.String() == "" || creds.Secret.String() == "" {
		return nil, fmt.Errorf("binanceth: api_key and secret are required")
	}
	return &BinanceTHExchange{
		apiKey:    creds.APIKey.String(),
		apiSecret: creds.Secret.String(),
		client:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Name returns the exchange identifier.
func (b *BinanceTHExchange) Name() string { return Name }

// SupportedWalletTypes returns all wallet types this exchange supports.
func (b *BinanceTHExchange) SupportedWalletTypes() []string {
	return []string{"spot"}
}

// GetBalances implements the basic Exchange interface.
func (b *BinanceTHExchange) GetBalances(ctx context.Context) ([]exchanges.Balance, error) {
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
func (b *BinanceTHExchange) GetWalletBalances(ctx context.Context, walletType string) ([]exchanges.WalletBalance, error) {
	switch walletType {
	case "spot":
		return b.getSpotBalances(ctx)
	default:
		return nil, fmt.Errorf("binanceth: unsupported wallet type %q (supported: %v)", walletType, b.SupportedWalletTypes())
	}
}

// usdLike is the set of stablecoins treated as 1:1 with USD/USDT for valuation.
var usdLike = map[string]bool{
	"USDT": true, "USDC": true, "BUSD": true, "FDUSD": true,
	"TUSD": true, "DAI": true, "USD": true, "USDP": true, "GUSD": true,
}

// FetchPrice implements PricedExchange.
// Binance TH pairs are primarily quoted in THB and USDT (e.g. BTCTHB, BTCUSDT).
func (b *BinanceTHExchange) FetchPrice(ctx context.Context, asset, quote string) (float64, error) {
	upper := strings.ToUpper(asset)
	upperQuote := strings.ToUpper(quote)

	if upper == upperQuote || (usdLike[upperQuote] && usdLike[upper]) {
		return 0, nil
	}

	// Try asset+quote directly (Binance TH uses no separator, e.g. BTCTHB)
	symbol := upper + upperQuote
	price, err := b.fetchTickerPrice(ctx, symbol)
	if err == nil {
		return price, nil
	}

	// Fallback: try asset+USDT then treat as equivalent if quote is a stablecoin
	if upperQuote != "USDT" {
		if price, err := b.fetchTickerPrice(ctx, upper+"USDT"); err == nil && usdLike[upperQuote] {
			return price, nil
		}
	}

	return 0, fmt.Errorf("binanceth: cannot determine price for %s in %s", asset, quote)
}

// accountResponse is the response from GET /api/v1/accountV2.
type accountResponse struct {
	Balances []struct {
		Asset  string `json:"asset"`
		Free   string `json:"free"`
		Locked string `json:"locked"`
	} `json:"balances"`
}

// tickerPriceResponse is the response from GET /api/v1/ticker/price for a single symbol.
type tickerPriceResponse struct {
	Symbol string `json:"symbol"`
	Price  string `json:"price"`
}

// getSpotBalances fetches spot balances from GET /api/v1/accountV2.
func (b *BinanceTHExchange) getSpotBalances(ctx context.Context) ([]exchanges.WalletBalance, error) {
	var resp accountResponse
	if err := b.signedGet(ctx, endpointAccount, nil, &resp); err != nil {
		return nil, fmt.Errorf("spot: %w", err)
	}

	var out []exchanges.WalletBalance
	for _, bal := range resp.Balances {
		free, _ := strconv.ParseFloat(bal.Free, 64)
		locked, _ := strconv.ParseFloat(bal.Locked, 64)
		if free == 0 && locked == 0 {
			continue
		}
		out = append(out, exchanges.WalletBalance{
			Balance:    exchanges.Balance{Asset: bal.Asset, Free: free, Locked: locked},
			WalletType: "spot",
		})
	}
	return out, nil
}

// fetchTickerPrice fetches the last price for a single symbol from GET /api/v1/ticker/price.
func (b *BinanceTHExchange) fetchTickerPrice(ctx context.Context, symbol string) (float64, error) {
	params := url.Values{}
	params.Set("symbol", symbol)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpointTicker+"?"+params.Encode(), nil)
	if err != nil {
		return 0, err
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("binanceth: ticker request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("binanceth: ticker HTTP %d for symbol %s", resp.StatusCode, symbol)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var ticker tickerPriceResponse
	if err := json.Unmarshal(body, &ticker); err != nil {
		return 0, err
	}

	return strconv.ParseFloat(ticker.Price, 64)
}

// depthResponse is the response from GET /api/v1/depth.
// Bids and asks are returned as [price, qty] string pairs.
type depthResponse struct {
	LastUpdateID int64      `json:"lastUpdateId"`
	Bids         [][]string `json:"bids"`
	Asks         [][]string `json:"asks"`
}

// exchangeInfoResponse is the response from GET /api/v1/exchangeInfo.
type exchangeInfoResponse struct {
	Symbols []struct {
		Symbol             string `json:"symbol"`
		Status             string `json:"status"`
		BaseAsset          string `json:"baseAsset"`
		BaseAssetPrecision int    `json:"baseAssetPrecision"`
		QuoteAsset         string `json:"quoteAsset"`
		QuotePrecision     int    `json:"quotePrecision"`
	} `json:"symbols"`
}

// fetchKlines fetches OHLCV bars from GET /api/v1/klines.
// Binance returns each bar as a 12-element JSON array; indices 0-5 are used.
func (b *BinanceTHExchange) fetchKlines(ctx context.Context, symbol, interval string, since *int64, limit int) ([]ccxt.OHLCV, error) {
	if limit <= 0 {
		limit = 100
	}
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("interval", interval)
	params.Set("limit", strconv.Itoa(limit))
	if since != nil {
		params.Set("startTime", strconv.FormatInt(*since, 10))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+endpointKlines+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binanceth: klines request failed: %w", err)
	}
	defer resp.Body.Close()

	// Each row: [openTime, open, high, low, close, vol, closeTime, ...]
	var raw [][]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("binanceth: parsing klines: %w", err)
	}

	parseStr := func(r json.RawMessage) float64 {
		var s string
		if json.Unmarshal(r, &s) == nil {
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		var f float64
		json.Unmarshal(r, &f)
		return f
	}

	out := make([]ccxt.OHLCV, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		var openTime int64
		json.Unmarshal(row[0], &openTime)
		out = append(out, ccxt.OHLCV{
			Timestamp: openTime,
			Open:      parseStr(row[1]),
			High:      parseStr(row[2]),
			Low:       parseStr(row[3]),
			Close:     parseStr(row[4]),
			Volume:    parseStr(row[5]),
		})
	}
	return out, nil
}

// fetchDepth fetches the order book from GET /api/v1/depth.
func (b *BinanceTHExchange) fetchDepth(ctx context.Context, symbol string, limit int) (depthResponse, error) {
	if limit <= 0 {
		limit = 20
	}
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("limit", strconv.Itoa(limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		baseURL+endpointDepth+"?"+params.Encode(), nil)
	if err != nil {
		return depthResponse{}, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return depthResponse{}, fmt.Errorf("binanceth: depth request failed: %w", err)
	}
	defer resp.Body.Close()

	var out depthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return depthResponse{}, fmt.Errorf("binanceth: parsing depth: %w", err)
	}
	return out, nil
}

// parseOrderBookSide converts Binance string [price, qty] pairs to [][]float64.
func parseOrderBookSide(rows [][]string) [][]float64 {
	out := make([][]float64, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		price, err1 := strconv.ParseFloat(row[0], 64)
		qty, err2 := strconv.ParseFloat(row[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, []float64{price, qty})
	}
	return out
}

// fetchExchangeInfo fetches all listed markets from GET /api/v1/exchangeInfo.
func (b *BinanceTHExchange) fetchExchangeInfo(ctx context.Context) (exchangeInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+endpointExchangeInfo, nil)
	if err != nil {
		return exchangeInfoResponse{}, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return exchangeInfoResponse{}, fmt.Errorf("binanceth: exchangeInfo request failed: %w", err)
	}
	defer resp.Body.Close()

	var out exchangeInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return exchangeInfoResponse{}, fmt.Errorf("binanceth: parsing exchangeInfo: %w", err)
	}
	return out, nil
}

// signedGet sends a SIGNED GET request to a private Binance TH endpoint.
// Binance TH signs: HMAC-SHA256(secret, queryString)
// The signature and timestamp are appended to the query string.
func (b *BinanceTHExchange) signedGet(ctx context.Context, path string, extraParams url.Values, out interface{}) error {
	params := url.Values{}
	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

	queryString := params.Encode()
	sig := b.sign(queryString)
	queryString += "&signature=" + sig

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path+"?"+queryString, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return json.Unmarshal(body, out)
}

// sign computes the HMAC-SHA256 signature for a Binance TH API request.
// Payload is the full query string (including timestamp).
func (b *BinanceTHExchange) sign(queryString string) string {
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(queryString))
	return hex.EncodeToString(mac.Sum(nil))
}

// signedPost sends a SIGNED POST request to a private Binance TH endpoint.
// Parameters are form-encoded in the request body.
func (b *BinanceTHExchange) signedPost(ctx context.Context, path string, extraParams url.Values, out interface{}) error {
	params := url.Values{}
	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

	queryString := params.Encode()
	sig := b.sign(queryString)
	queryString += "&signature=" + sig

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path+"?"+queryString, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return json.Unmarshal(body, out)
}

// signedDelete sends a SIGNED DELETE request to a private Binance TH endpoint.
func (b *BinanceTHExchange) signedDelete(ctx context.Context, path string, extraParams url.Values, out interface{}) error {
	params := url.Values{}
	for k, vs := range extraParams {
		for _, v := range vs {
			params.Set(k, v)
		}
	}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))

	queryString := params.Encode()
	sig := b.sign(queryString)
	queryString += "&signature=" + sig

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+path+"?"+queryString, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return json.Unmarshal(body, out)
}

// orderResponse is the response from GET/POST/DELETE /api/v1/order and list endpoints.
type orderResponse struct {
	OrderID             int64  `json:"orderId"`
	ClientOrderID       string `json:"clientOrderId"`
	Symbol              string `json:"symbol"`
	Status              string `json:"status"`
	Side                string `json:"side"`
	Type                string `json:"type"`
	TimeInForce         string `json:"timeInForce"`
	Price               string `json:"price"`
	OrigQty             string `json:"origQty"`
	ExecutedQty         string `json:"executedQty"`
	CummulativeQuoteQty string `json:"cummulativeQuoteQty"`
	Time                int64  `json:"time"`
	UpdateTime          int64  `json:"updateTime"`
}

// tradeResponse is the response from GET /api/v1/userTrades.
type tradeResponse struct {
	ID              int64  `json:"id"`
	OrderID         int64  `json:"orderId"`
	Symbol          string `json:"symbol"`
	Price           string `json:"price"`
	Qty             string `json:"qty"`
	Commission      string `json:"commission"`
	CommissionAsset string `json:"commissionAsset"`
	Time            int64  `json:"time"`
	IsBuyer         bool   `json:"isBuyer"`
	IsMaker         bool   `json:"isMaker"`
}

// fetchOrder fetches a single order by ID from GET /api/v1/order.
func (b *BinanceTHExchange) fetchOrder(ctx context.Context, symbol, orderID string) (orderResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("orderId", orderID)

	var resp orderResponse
	if err := b.signedGet(ctx, endpointOrder, params, &resp); err != nil {
		return orderResponse{}, fmt.Errorf("binanceth: fetchOrder: %w", err)
	}
	return resp, nil
}

// fetchOpenOrders fetches all open orders from GET /api/v1/openOrders.
// Pass empty symbol to fetch all open orders (higher weight: 40 vs 3).
func (b *BinanceTHExchange) fetchOpenOrders(ctx context.Context, symbol string) ([]orderResponse, error) {
	params := url.Values{}
	if symbol != "" {
		params.Set("symbol", symbol)
	}

	var resp []orderResponse
	if err := b.signedGet(ctx, endpointOpenOrders, params, &resp); err != nil {
		return nil, fmt.Errorf("binanceth: fetchOpenOrders: %w", err)
	}
	return resp, nil
}

// fetchAllOrders fetches order history from GET /api/v1/allOrders.
func (b *BinanceTHExchange) fetchAllOrders(ctx context.Context, symbol string, since *int64, limit int) ([]orderResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	if since != nil {
		params.Set("startTime", strconv.FormatInt(*since, 10))
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	var resp []orderResponse
	if err := b.signedGet(ctx, endpointAllOrders, params, &resp); err != nil {
		return nil, fmt.Errorf("binanceth: fetchAllOrders: %w", err)
	}
	return resp, nil
}

// fetchUserTrades fetches trade history from GET /api/v1/userTrades.
func (b *BinanceTHExchange) fetchUserTrades(ctx context.Context, symbol string, since *int64, limit int) ([]tradeResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	if since != nil {
		params.Set("startTime", strconv.FormatInt(*since, 10))
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}

	var resp []tradeResponse
	if err := b.signedGet(ctx, endpointUserTrades, params, &resp); err != nil {
		return nil, fmt.Errorf("binanceth: fetchUserTrades: %w", err)
	}
	return resp, nil
}

// createOrder places a new order via POST /api/v1/order.
func (b *BinanceTHExchange) createOrder(ctx context.Context, symbol, side, orderType string, amount float64, price *float64) (orderResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", strings.ToUpper(side))
	params.Set("type", strings.ToUpper(orderType))
	params.Set("quantity", strconv.FormatFloat(amount, 'f', -1, 64))
	if price != nil {
		params.Set("price", strconv.FormatFloat(*price, 'f', -1, 64))
		params.Set("timeInForce", "GTC")
	}

	var resp orderResponse
	if err := b.signedPost(ctx, endpointOrder, params, &resp); err != nil {
		return orderResponse{}, fmt.Errorf("binanceth: createOrder: %w", err)
	}
	return resp, nil
}

// cancelOrder cancels an open order via DELETE /api/v1/order.
func (b *BinanceTHExchange) cancelOrder(ctx context.Context, symbol, orderID string) (orderResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("orderId", orderID)

	var resp orderResponse
	if err := b.signedDelete(ctx, endpointOrder, params, &resp); err != nil {
		return orderResponse{}, fmt.Errorf("binanceth: cancelOrder: %w", err)
	}
	return resp, nil
}
