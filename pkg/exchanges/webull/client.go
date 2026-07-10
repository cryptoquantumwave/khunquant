package webull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// Client is an authenticated HTTP client for the Webull OpenAPI.
// It handles request signing, token management, and automatic retries.
type Client struct {
	baseURL    string
	httpClient *http.Client
	signer     *Signer
	accountID  string

	// Token management
	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time

	// Category cache for ETF fallback: maps symbol to resolved category (US_STOCK or US_ETF)
	categoryMu    sync.Mutex
	categoryCache map[string]string // symbol -> category
}

// Option is a functional option for Client configuration.
type Option func(*Client) error

// WithBaseURL sets a custom base URL (useful for testing with httptest.Server).
// When set, the signing host is derived from this URL.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) error {
		c.baseURL = baseURL
		return nil
	}
}

// WithHTTPClient sets a custom HTTP client (useful for testing).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		c.httpClient = hc
		return nil
	}
}

// NewClient creates a new Webull API client.
// Credentials are taken from acc (APIKey = app key, Secret = app secret).
func NewClient(acc config.WebullExchangeAccount, opts ...Option) (*Client, error) {
	if acc.APIKey.String() == "" {
		return nil, fmt.Errorf("webull: api_key (app key) is required")
	}
	if acc.Secret.String() == "" {
		return nil, fmt.Errorf("webull: secret (app secret) is required")
	}
	if acc.AccountID == "" {
		return nil, fmt.Errorf("webull: account_id is required")
	}

	httpClient, err := utils.CreateHTTPClient(acc.Proxy, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("webull: %w", err)
	}

	// Determine base URL from environment
	environment := acc.Environment
	if environment == "" {
		environment = "prod"
	}
	baseURL := BaseURLForEnvironment(environment)

	c := &Client{
		baseURL:       baseURL,
		httpClient:    httpClient,
		signer:        NewSigner(acc.APIKey.String(), acc.Secret.String()),
		accountID:     acc.AccountID,
		categoryCache: make(map[string]string),
	}

	// Register secrets for redaction in logs
	logger.RegisterSecret(acc.APIKey.String())
	logger.RegisterSecret(acc.Secret.String())

	// Apply functional options
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// doRequest performs a low-level HTTP request with signing, token management, and automatic retry on 401.
// body can be nil or any value that will be JSON-marshaled.
// out is the target type for JSON unmarshaling the response.
// skipToken=true is used for token creation itself to avoid recursion.
// On auth errors (401 or error_code contains TOKEN/UNAUTHORIZED), retries exactly once with a fresh token.
func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, body, out interface{}, skipToken bool) error {
	// Marshal body once (if provided) to compute MD5 for signature
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("webull: marshal request body: %w", err)
		}
	}

	// Extract host from baseURL for signing
	parsedURL, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("webull: parse base url: %w", err)
	}
	host := parsedURL.Host
	if host == "" {
		host = "api.webull.com"
	}

	// Build full URL
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	// Retry loop: attempt 0 = initial, attempt 1 = retry after token invalidation
	for attempt := 0; attempt < 2; attempt++ {
		// Sign the request (fresh timestamp/nonce for each attempt)
		headers := c.signer.SignRequest(path, method, host, query, bodyBytes)

		// Create request with fresh body reader for each attempt
		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
		if err != nil {
			return fmt.Errorf("webull: create request: %w", err)
		}

		// Set headers
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-App-Key", headers.XAppKey)
		req.Header.Set("X-Timestamp", headers.XTimestamp)
		req.Header.Set("X-Signature", headers.XSignature)
		req.Header.Set("X-Signature-Algorithm", headers.XSignatureAlgorithm)
		req.Header.Set("X-Signature-Version", headers.XSignatureVersion)
		req.Header.Set("X-Signature-Nonce", headers.XSignatureNonce)
		req.Header.Set("X-Version", headers.XVersion)

		// Attach access token if available and not skipping (e.g., token creation)
		// Fetch token INSIDE loop so fresh token is picked up after invalidation
		if !skipToken {
			token, err := c.getOrRefreshToken(ctx)
			if err != nil {
				return fmt.Errorf("webull: get token: %w", err)
			}
			req.Header.Set("X-Access-Token", token)
		}

		// Send request
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("webull: %s %s: %w", method, path, err)
		}
		defer resp.Body.Close()

		// Read response body
		respBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("webull: read response body: %w", err)
		}

		// Check HTTP status
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// Success: unmarshal response and return
			if out != nil {
				if err := json.Unmarshal(respBytes, out); err != nil {
					return fmt.Errorf("webull: decode response: %w", err)
				}
			}
			return nil
		}

		// Parse error response
		var apiErr ErrorResponse
		json.Unmarshal(respBytes, &apiErr) // best effort, may fail

		// Check if this is an auth error and we haven't retried yet
		if !skipToken && attempt == 0 && isAuthError(resp.StatusCode, apiErr) {
			// Invalidate token and retry
			c.invalidateToken()
			continue
		}

		// Non-auth error or second attempt: return error
		if apiErr.Message != "" {
			return fmt.Errorf("webull: %s %s: [%d] %s", method, path, resp.StatusCode, apiErr.Error())
		}
		return fmt.Errorf("webull: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(respBytes))
	}

	// Should not reach here
	return fmt.Errorf("webull: %s %s: exceeded retry limit", method, path)
}

// isAuthError returns true if the response indicates an authentication/authorization error
// that should trigger a token refresh. Subscription errors (401 + "Insufficient permission")
// are NOT retried as they are data-permission issues, not token issues.
func isAuthError(statusCode int, apiErr ErrorResponse) bool {
	if statusCode == 401 {
		// Check if this is a subscription permission error (not a token error)
		if strings.Contains(strings.ToLower(apiErr.Message), "insufficient permission") {
			return false // Don't retry subscription errors
		}
		return true
	}
	upperCode := strings.ToUpper(apiErr.ErrorCode)
	return strings.Contains(upperCode, "TOKEN") || strings.Contains(upperCode, "UNAUTHORIZED")
}

// getOrRefreshToken returns a cached access token, or creates/refreshes one if needed.
// Thread-safe with mutex protection.
func (c *Client) getOrRefreshToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// If token exists and not expiring soon (60s buffer), return it
	if c.token != "" && time.Now().Add(60*time.Second).Before(c.tokenExpiry) {
		return c.token, nil
	}

	// Create new token
	resp := &TokenResponse{}
	if err := c.doRequest(ctx, http.MethodPost, endpointTokenCreate, nil, nil, resp, true); err != nil {
		return "", fmt.Errorf("webull: create token: %w", err)
	}

	// Store token and expiry
	c.token = resp.Token
	c.tokenExpiry = time.UnixMilli(resp.Expires)

	return c.token, nil
}

// invalidateToken clears the cached token, forcing refresh on next request.
func (c *Client) invalidateToken() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
}

// --- Category Resolution (ETF Fallback) ---

// resolveCategoryForSymbol resolves the market-data category for a symbol.
// Tries US_STOCK first; if the response is empty or INVALID_SYMBOL, tries US_ETF.
// Caches the resolved category per symbol to avoid repeated fallback calls.
// This is used by FetchSnapshot and FetchInstruments which support both categories.
func (c *Client) resolveCategoryForSymbol(ctx context.Context, symbol string) (string, error) {
	// Check cache first
	c.categoryMu.Lock()
	if cat, ok := c.categoryCache[symbol]; ok {
		c.categoryMu.Unlock()
		return cat, nil
	}
	c.categoryMu.Unlock()

	// Try US_STOCK first
	snapshots, err := c.fetchSnapshotWithCategory(ctx, []string{symbol}, "US_STOCK")
	if err == nil && len(snapshots) > 0 {
		// US_STOCK worked, cache it
		c.categoryMu.Lock()
		c.categoryCache[symbol] = "US_STOCK"
		c.categoryMu.Unlock()
		return "US_STOCK", nil
	}

	// Check if error indicates symbol not found (INVALID_SYMBOL or empty response)
	// Try US_ETF as fallback
	snapshots, err = c.fetchSnapshotWithCategory(ctx, []string{symbol}, "US_ETF")
	if err == nil && len(snapshots) > 0 {
		// US_ETF worked, cache it
		c.categoryMu.Lock()
		c.categoryCache[symbol] = "US_ETF"
		c.categoryMu.Unlock()
		return "US_ETF", nil
	}

	// Both failed; return the US_ETF error as it's the last attempt
	if err != nil {
		return "", err
	}
	// Empty response
	return "", fmt.Errorf("webull: no data for symbol %s (tried US_STOCK and US_ETF)", symbol)
}

// fetchSnapshotWithCategory is a private helper that fetches snapshots with an explicit category.
func (c *Client) fetchSnapshotWithCategory(ctx context.Context, symbols []string, category string) ([]Snapshot, error) {
	query := url.Values{}
	query.Set("category", category)
	query.Set("symbols", strings.Join(symbols, ","))
	var resp []Snapshot
	if err := c.doRequest(ctx, http.MethodGet, endpointSnapshot, query, nil, &resp, false); err != nil {
		return nil, err
	}
	return resp, nil
}

// fetchBarsWithCategory is a private helper that fetches bars with an explicit category.
func (c *Client) fetchBarsWithCategory(ctx context.Context, symbol, timespan, category string, count int) ([]Bar, error) {
	query := url.Values{}
	query.Set("symbol", symbol)
	query.Set("category", category)
	query.Set("timespan", timespan)
	query.Set("real_time_required", "false")
	if count > 0 {
		query.Set("count", fmt.Sprintf("%d", count))
	}
	var resp []Bar
	if err := c.doRequest(ctx, http.MethodGet, endpointBars, query, nil, &resp, false); err != nil {
		return nil, err
	}
	return resp, nil
}

// resolveCategoryForBars resolves the market-data category for bars.
// Similar to resolveCategoryForSymbol, uses cached resolution for efficiency.
func (c *Client) resolveCategoryForBars(ctx context.Context, symbol string) (string, error) {
	// Check cache first
	c.categoryMu.Lock()
	if cat, ok := c.categoryCache[symbol]; ok {
		c.categoryMu.Unlock()
		return cat, nil
	}
	c.categoryMu.Unlock()

	// Try US_STOCK first
	bars, err := c.fetchBarsWithCategory(ctx, symbol, "D", "US_STOCK", 1)
	if err == nil && len(bars) > 0 {
		c.categoryMu.Lock()
		c.categoryCache[symbol] = "US_STOCK"
		c.categoryMu.Unlock()
		return "US_STOCK", nil
	}

	// Try US_ETF fallback
	bars, err = c.fetchBarsWithCategory(ctx, symbol, "D", "US_ETF", 1)
	if err == nil && len(bars) > 0 {
		c.categoryMu.Lock()
		c.categoryCache[symbol] = "US_ETF"
		c.categoryMu.Unlock()
		return "US_ETF", nil
	}

	// Both failed
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("webull: no bar data for symbol %s (tried US_STOCK and US_ETF)", symbol)
}

// --- High-level API methods ---

// CreateToken creates a new access token.
func (c *Client) CreateToken(ctx context.Context) (*TokenResponse, error) {
	var resp TokenResponse
	if err := c.doRequest(ctx, http.MethodPost, endpointTokenCreate, nil, nil, &resp, true); err != nil {
		return nil, fmt.Errorf("webull: CreateToken: %w", err)
	}
	c.tokenMu.Lock()
	c.token = resp.Token
	c.tokenExpiry = time.UnixMilli(resp.Expires)
	c.tokenMu.Unlock()
	return &resp, nil
}

// FetchAccountList fetches all accounts.
func (c *Client) FetchAccountList(ctx context.Context) ([]AccountListItem, error) {
	var resp []AccountListItem
	if err := c.doRequest(ctx, http.MethodGet, endpointAccountList, nil, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchAccountList: %w", err)
	}
	return resp, nil
}

// FetchBalance fetches the balance for the configured account.
func (c *Client) FetchBalance(ctx context.Context) (*BalanceResponse, error) {
	query := url.Values{}
	query.Set("account_id", c.accountID)
	var resp BalanceResponse
	if err := c.doRequest(ctx, http.MethodGet, endpointBalance, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchBalance: %w", err)
	}
	return &resp, nil
}

// FetchPositions fetches all positions for the configured account.
func (c *Client) FetchPositions(ctx context.Context) ([]Position, error) {
	query := url.Values{}
	query.Set("account_id", c.accountID)
	var resp []Position
	if err := c.doRequest(ctx, http.MethodGet, endpointPositions, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchPositions: %w", err)
	}
	return resp, nil
}

// FetchInstruments fetches instrument metadata by symbols.
// If symbols is empty, fetches all tradable instruments (pagination required).
// Supports both US_STOCK and US_ETF categories via fallback resolution.
//
// TODO(webull-multiasset): instrument endpoint is pinned to US_STOCK category here.
// Webull also exposes crypto/futures/option markets; parameterize `category`
// when those land (see docs/webull-api-spec.md).
func (c *Client) FetchInstruments(ctx context.Context, symbols []string) ([]Instrument, error) {
	query := url.Values{}
	// Instruments endpoint only supports US_STOCK category per API spec;
	// ETF instruments may need to be fetched separately or via category parameterization in the future.
	query.Set("category", "US_STOCK")
	if len(symbols) > 0 {
		query.Set("symbols", strings.Join(symbols, ","))
	}
	var resp []Instrument
	if err := c.doRequest(ctx, http.MethodGet, endpointInstrumentStockList, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchInstruments: %w", err)
	}
	return resp, nil
}

// FetchSnapshot fetches snapshot (price) data for multiple symbols.
// symbols: comma-separated or array of symbols (max 100)
// Supports both US_STOCK and US_ETF categories via automatic fallback resolution.
// The resolved category is cached per symbol for efficiency.
func (c *Client) FetchSnapshot(ctx context.Context, symbols []string) ([]Snapshot, error) {
	if len(symbols) == 0 {
		return []Snapshot{}, nil
	}

	// Group symbols by resolved category
	categoryMap := make(map[string][]string)
	for _, sym := range symbols {
		cat, err := c.resolveCategoryForSymbol(ctx, sym)
		if err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: resolve category for %s: %w", sym, err)
		}
		categoryMap[cat] = append(categoryMap[cat], sym)
	}

	// Fetch snapshots for each category
	var allSnapshots []Snapshot
	for category, syms := range categoryMap {
		query := url.Values{}
		query.Set("category", category)
		query.Set("symbols", strings.Join(syms, ","))
		var resp []Snapshot
		if err := c.doRequest(ctx, http.MethodGet, endpointSnapshot, query, nil, &resp, false); err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: %w", err)
		}
		allSnapshots = append(allSnapshots, resp...)
	}
	return allSnapshots, nil
}

// FetchBars fetches candlestick bars for a symbol.
// timespan: M1, M5, M15, M30, M60, M120, M240, D, W, M, Y
// count: number of bars (1-1200, or 1-1650 for M1; default 200)
// Supports both US_STOCK and US_ETF categories via automatic fallback resolution.
// The resolved category is cached per symbol for efficiency.
func (c *Client) FetchBars(ctx context.Context, symbol, timespan string, count int) ([]Bar, error) {
	category, err := c.resolveCategoryForBars(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("webull: FetchBars %s: resolve category: %w", symbol, err)
	}

	query := url.Values{}
	query.Set("symbol", symbol)
	query.Set("category", category)
	query.Set("timespan", timespan)
	query.Set("real_time_required", "false")
	if count > 0 {
		query.Set("count", fmt.Sprintf("%d", count))
	}
	var resp []Bar
	if err := c.doRequest(ctx, http.MethodGet, endpointBars, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchBars %s: %w", symbol, err)
	}
	return resp, nil
}

// --- Options Market Data ---

// FetchOptionSnapshot fetches snapshot (quote) data for multiple option contracts.
// encodedSymbols: OCC-encoded symbols (e.g., AAPL260821C00320000)
// Returns option snapshot DTOs with price and greeks.
// Note: option market data requires a US_OPTION quote subscription in production.
// A 401 error with "Insufficient permission, please subscribe to US_OPTION quotes"
// indicates the subscription is required; this is NOT a token auth failure.
func (c *Client) FetchOptionSnapshot(ctx context.Context, encodedSymbols []string) ([]OptionSnapshotDTO, error) {
	if len(encodedSymbols) == 0 {
		return []OptionSnapshotDTO{}, nil
	}

	query := url.Values{}
	query.Set("category", "US_OPTION")
	query.Set("symbols", strings.Join(encodedSymbols, ","))
	var resp []OptionSnapshotDTO
	if err := c.doRequest(ctx, http.MethodGet, endpointOptionSnapshot, query, nil, &resp, false); err != nil {
		// Check if error is a subscription error (401 + Insufficient permission message)
		var apiErr ErrorResponse
		json.Unmarshal([]byte(err.Error()), &apiErr) // best effort
		if strings.Contains(err.Error(), "Insufficient permission") && strings.Contains(err.Error(), "US_OPTION") {
			return nil, fmt.Errorf("webull: option market data requires a US_OPTION quote subscription: %w", err)
		}
		return nil, fmt.Errorf("webull: FetchOptionSnapshot: %w", err)
	}
	return resp, nil
}

// FetchOptionBars fetches candlestick bars for an option contract.
// encodedSymbol: OCC-encoded symbol (e.g., AAPL260821C00320000)
// timespan: M1, M5, M15, M30, M60, M120, M240, D, W, M, Y
// count: number of bars (1-1200 or 1-1650 for M1; default 200)
func (c *Client) FetchOptionBars(ctx context.Context, encodedSymbol, timespan string, count int) ([]OptionBarDTO, error) {
	query := url.Values{}
	query.Set("symbol", encodedSymbol)
	query.Set("category", "US_OPTION")
	query.Set("timespan", timespan)
	query.Set("real_time_required", "false")
	if count > 0 {
		query.Set("count", fmt.Sprintf("%d", count))
	}
	var resp []OptionBarDTO
	if err := c.doRequest(ctx, http.MethodGet, endpointOptionBars, query, nil, &resp, false); err != nil {
		// Check if error is a subscription error
		if strings.Contains(err.Error(), "Insufficient permission") && strings.Contains(err.Error(), "US_OPTION") {
			return nil, fmt.Errorf("webull: option market data requires a US_OPTION quote subscription: %w", err)
		}
		return nil, fmt.Errorf("webull: FetchOptionBars %s: %w", encodedSymbol, err)
	}
	return resp, nil
}

// --- Trading (Orders) ---

// PlaceOrder submits a new order.
func (c *Client) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (*PlaceOrderResponse, error) {
	var resp PlaceOrderResponse
	if err := c.doRequest(ctx, http.MethodPost, endpointOrderPlace, nil, &req, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: PlaceOrder: %w", err)
	}
	return &resp, nil
}

// CancelOrder cancels an open order by client_order_id.
func (c *Client) CancelOrder(ctx context.Context, req CancelOrderRequest) (*PlaceOrderResponse, error) {
	var resp PlaceOrderResponse
	if err := c.doRequest(ctx, http.MethodPost, endpointOrderCancel, nil, &req, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: CancelOrder: %w", err)
	}
	return &resp, nil
}

// FetchOpenOrders fetches all open orders for the account.
func (c *Client) FetchOpenOrders(ctx context.Context) ([]ComboOrder, error) {
	query := url.Values{}
	query.Set("account_id", c.accountID)
	var resp []ComboOrder
	if err := c.doRequest(ctx, http.MethodGet, endpointOrderOpen, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchOpenOrders: %w", err)
	}
	return resp, nil
}

// FetchOrderHistory fetches closed orders within a date range.
// startDate and endDate are optional yyyy-MM-dd format strings; if empty, defaults to last 7 days.
func (c *Client) FetchOrderHistory(ctx context.Context, startDate, endDate string) ([]ComboOrder, error) {
	query := url.Values{}
	query.Set("account_id", c.accountID)
	if startDate != "" {
		query.Set("start_date", startDate)
	}
	if endDate != "" {
		query.Set("end_date", endDate)
	}
	var resp []ComboOrder
	if err := c.doRequest(ctx, http.MethodGet, endpointOrderHistory, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchOrderHistory: %w", err)
	}
	return resp, nil
}

// FetchOrderDetail fetches details for a single order by client_order_id.
func (c *Client) FetchOrderDetail(ctx context.Context, clientOrderID string) (*ComboOrder, error) {
	query := url.Values{}
	query.Set("account_id", c.accountID)
	query.Set("client_order_id", clientOrderID)
	var resp ComboOrder
	if err := c.doRequest(ctx, http.MethodGet, endpointOrderDetail, query, nil, &resp, false); err != nil {
		return nil, fmt.Errorf("webull: FetchOrderDetail: %w", err)
	}
	return &resp, nil
}
