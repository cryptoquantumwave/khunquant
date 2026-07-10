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

// retryDelayFn computes the backoff before a transient (429/5xx/transport) retry.
// It's a package var so tests can shrink the delay; production uses the shared
// utils implementation which honors Retry-After.
var retryDelayFn = utils.RetryDelayForAttempt

// Option is a functional option for Client configuration.
type Option func(*Client) error

// WithBaseURL sets a custom base URL (useful for testing with httptest.Server).
// When set, the signing host is derived from this URL, so the URL must parse and
// carry a host — otherwise construction fails rather than silently mis-signing
// against a fallback host later.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) error {
		u, err := url.Parse(baseURL)
		if err != nil {
			return fmt.Errorf("webull: invalid base URL %q: %w", baseURL, err)
		}
		if u.Host == "" {
			return fmt.Errorf("webull: base URL %q has no host", baseURL)
		}
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

// doRequest performs a low-level HTTP request with signing, token management, and automatic retries.
// body can be nil or any value that will be JSON-marshaled.
// out is the target type for JSON unmarshaling the response.
// skipToken=true is used for token creation itself to avoid recursion.
// On auth errors (401 or error_code contains TOKEN/UNAUTHORIZED) it re-authenticates once with a
// fresh token; on transient errors (429/5xx or transport failures) it backs off and retries up to
// maxTransientRetries times, honoring Retry-After. Each attempt is re-signed with a fresh nonce.
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
		// Should be unreachable: WithBaseURL validates the host and
		// BaseURLForEnvironment always yields one. Surface it rather than
		// silently sign against a fallback host.
		return fmt.Errorf("webull: base URL %q has no host", c.baseURL)
	}

	// Build full URL
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	// Retry loop with two independent policies:
	//   - auth retry: on a token/401 error, re-authenticate once, immediately (no backoff);
	//   - transient retry: on 429/5xx (or a transport error), back off and retry up to
	//     maxTransientRetries times, honoring any Retry-After header.
	// Signing happens inside the loop so every attempt gets a fresh timestamp/nonce
	// (a re-signed request is required for HMAC auth), and the body reader is rebuilt
	// each iteration.
	const maxTransientRetries = 3
	reAuthed := false
	transient := 0
	for {
		// Sign the request (fresh timestamp/nonce for each attempt)
		headers, err := c.signer.SignRequest(path, method, host, query, bodyBytes)
		if err != nil {
			return fmt.Errorf("webull: %s %s: %w", method, path, err)
		}

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
			// Transport error: retry with backoff up to the transient cap.
			if transient < maxTransientRetries {
				if serr := utils.SleepWithCtx(ctx, retryDelayFn(nil, transient)); serr != nil {
					return fmt.Errorf("webull: %s %s: %w", method, path, serr)
				}
				transient++
				continue
			}
			return fmt.Errorf("webull: %s %s: %w", method, path, err)
		}

		// Read and close the body for this attempt (no deferred accumulation in a loop).
		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("webull: read response body: %w", readErr)
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

		// Auth error: re-authenticate once, immediately (no backoff). Subscription
		// 401s ("insufficient permission") are excluded by isAuthError.
		if !skipToken && !reAuthed && isAuthError(resp.StatusCode, apiErr) {
			c.invalidateToken()
			reAuthed = true
			continue
		}

		// Transient error (429/5xx): back off and retry up to the cap.
		if utils.ShouldRetry(resp.StatusCode) && transient < maxTransientRetries {
			if serr := utils.SleepWithCtx(ctx, retryDelayFn(resp, transient)); serr != nil {
				return fmt.Errorf("webull: %s %s: %w", method, path, serr)
			}
			transient++
			continue
		}

		// Terminal error.
		if apiErr.Message != "" {
			return fmt.Errorf("webull: %s %s: [%d] %s", method, path, resp.StatusCode, apiErr.Error())
		}
		return fmt.Errorf("webull: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(respBytes))
	}
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

// maxSnapshotSymbols caps how many symbols are sent in a single snapshot request
// (the endpoint accepts up to 100). Larger sets are chunked automatically.
const maxSnapshotSymbols = 100

// fetchSnapshotWithCategory fetches snapshots with an explicit category, chunking
// the symbol list to stay within the per-request limit.
func (c *Client) fetchSnapshotWithCategory(ctx context.Context, symbols []string, category string) ([]Snapshot, error) {
	var all []Snapshot
	for start := 0; start < len(symbols); start += maxSnapshotSymbols {
		end := min(start+maxSnapshotSymbols, len(symbols))
		query := url.Values{}
		query.Set("category", category)
		query.Set("symbols", strings.Join(symbols[start:end], ","))
		var resp []Snapshot
		if err := c.doRequest(ctx, http.MethodGet, endpointSnapshot, query, nil, &resp, false); err != nil {
			return nil, err
		}
		all = append(all, resp...)
	}
	return all, nil
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

// FetchInstruments fetches instrument metadata. When symbols are given it does a
// single bounded lookup (the endpoint accepts up to 100 symbols). When symbols is
// empty it fetches the full tradable list, cursor-paginating with
// last_instrument_id until a short/empty page (the endpoint pages at page_size,
// default 1000).
//
// The endpoint only supports category=US_STOCK per the API spec.
// TODO(webull-multiasset): Webull also exposes crypto/futures/option markets;
// parameterize `category` when those land (see docs/webull-api-spec.md).
func (c *Client) FetchInstruments(ctx context.Context, symbols []string) ([]Instrument, error) {
	// Symbol-scoped lookup: bounded set, single request.
	if len(symbols) > 0 {
		query := url.Values{}
		query.Set("category", "US_STOCK")
		query.Set("symbols", strings.Join(symbols, ","))
		var resp []Instrument
		if err := c.doRequest(ctx, http.MethodGet, endpointInstrumentStockList, query, nil, &resp, false); err != nil {
			return nil, fmt.Errorf("webull: FetchInstruments: %w", err)
		}
		return resp, nil
	}

	// Full listing: cursor-paginate until a page shorter than page_size (or empty).
	const pageSize = 1000
	const maxPages = 100 // safety cap (≤100k instruments)
	var all []Instrument
	lastID := ""
	for range maxPages {
		query := url.Values{}
		query.Set("category", "US_STOCK")
		query.Set("page_size", fmt.Sprintf("%d", pageSize))
		if lastID != "" {
			query.Set("last_instrument_id", lastID)
		}
		var resp []Instrument
		if err := c.doRequest(ctx, http.MethodGet, endpointInstrumentStockList, query, nil, &resp, false); err != nil {
			return nil, fmt.Errorf("webull: FetchInstruments: %w", err)
		}
		all = append(all, resp...)
		if len(resp) < pageSize {
			break
		}
		lastID = resp[len(resp)-1].InstrumentID
		if lastID == "" {
			break // no cursor available; stop rather than loop forever
		}
	}
	return all, nil
}

// FetchSnapshot fetches snapshot (price) data for multiple symbols, resolving the
// US_STOCK vs US_ETF category automatically and caching it per symbol.
//
// Uncached symbols are resolved with a batched probe: one US_STOCK request for the
// whole set, then a single US_ETF request for whichever symbols the first response
// omitted (2 calls for any N, versus N sequential probes previously). If the batch
// request instead errors — which is how the endpoint behaves when it rejects a
// category-mismatched symbol rather than omitting it (the sandbox is AAPL-only, so
// this cannot be distinguished there) — it falls back to correct per-symbol
// resolution. Either way the result is right; the batch path is the fast case.
func (c *Client) FetchSnapshot(ctx context.Context, symbols []string) ([]Snapshot, error) {
	if len(symbols) == 0 {
		return []Snapshot{}, nil
	}

	// Partition into symbols with a cached category and uncached ones.
	cachedByCat := make(map[string][]string)
	var uncached []string
	c.categoryMu.Lock()
	for _, sym := range symbols {
		if cat, ok := c.categoryCache[sym]; ok {
			cachedByCat[cat] = append(cachedByCat[cat], sym)
		} else {
			uncached = append(uncached, sym)
		}
	}
	c.categoryMu.Unlock()

	var out []Snapshot

	// Serve cached symbols in one batched request per category.
	for cat, syms := range cachedByCat {
		snaps, err := c.fetchSnapshotWithCategory(ctx, syms, cat)
		if err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: %w", err)
		}
		out = append(out, snaps...)
	}

	if len(uncached) == 0 {
		return out, nil
	}

	// Batch-probe uncached symbols under US_STOCK.
	stockSnaps, err := c.fetchSnapshotWithCategory(ctx, uncached, "US_STOCK")
	if err != nil {
		// Endpoint rejected the whole batch (likely an ETF/invalid symbol under
		// US_STOCK). Fall back to correct-but-slower per-symbol resolution.
		return c.fetchSnapshotPerSymbol(ctx, out, uncached)
	}

	present := c.cacheResolvedSymbols(stockSnaps, "US_STOCK")
	out = append(out, stockSnaps...)

	// Symbols the US_STOCK response omitted are retried once as US_ETF.
	var missing []string
	for _, sym := range uncached {
		if !present[strings.ToUpper(sym)] {
			missing = append(missing, sym)
		}
	}
	if len(missing) > 0 {
		etfSnaps, err := c.fetchSnapshotWithCategory(ctx, missing, "US_ETF")
		if err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: US_ETF fallback for %v: %w", missing, err)
		}
		c.cacheResolvedSymbols(etfSnaps, "US_ETF")
		out = append(out, etfSnaps...)
		// NOTE: a symbol still absent after both categories is dropped from the
		// result here (the batch path does not error on it). This differs from the
		// old per-symbol path, which errored on a fully-unresolved symbol. The
		// difference is inert for current callers: single-symbol callers
		// (FetchTicker/FetchPrice) still error via their len==0 guard, and
		// FetchTickers re-fetches any missing symbol through FetchTicker, which
		// surfaces a genuinely-unknown symbol as an error.
	}

	return out, nil
}

// cacheResolvedSymbols records the resolved category for every symbol present in
// snaps and returns the set of present symbols (upper-cased) for miss detection.
func (c *Client) cacheResolvedSymbols(snaps []Snapshot, category string) map[string]bool {
	present := make(map[string]bool, len(snaps))
	c.categoryMu.Lock()
	for _, s := range snaps {
		c.categoryCache[s.Symbol] = category
		present[strings.ToUpper(s.Symbol)] = true
	}
	c.categoryMu.Unlock()
	return present
}

// fetchSnapshotPerSymbol is the fallback path used when a batched category probe
// errors: it resolves and fetches each symbol individually (caching as it goes),
// appending to acc. Correct for any endpoint behavior, at N calls.
func (c *Client) fetchSnapshotPerSymbol(ctx context.Context, acc []Snapshot, symbols []string) ([]Snapshot, error) {
	for _, sym := range symbols {
		cat, err := c.resolveCategoryForSymbol(ctx, sym)
		if err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: resolve category for %s: %w", sym, err)
		}
		snaps, err := c.fetchSnapshotWithCategory(ctx, []string{sym}, cat)
		if err != nil {
			return nil, fmt.Errorf("webull: FetchSnapshot: %w", err)
		}
		acc = append(acc, snaps...)
	}
	return acc, nil
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
