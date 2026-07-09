package webull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

// Client is an authenticated HTTP client for the Webull OpenAPI.
// It handles request signing, error parsing, and automatic retries.
type Client struct {
	host       string
	httpClient *http.Client
	signer     *Signer
	accountID  string
}

// Option is a functional option for Client configuration.
type Option func(*Client) error

// WithBaseURL sets a custom base URL (useful for testing with httptest.Server).
func WithBaseURL(url string) Option {
	return func(c *Client) error {
		c.host = url
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

	c := &Client{
		host:       defaultHost,
		httpClient: httpClient,
		signer:     NewSigner(acc.APIKey.String(), acc.Secret.String()),
		accountID:  acc.AccountID,
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

// doRequest performs a low-level HTTP request with signing.
// body can be nil or any value that will be JSON-marshaled once and reused.
// out is the target type for JSON unmarshaling the response.
func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, body, out interface{}) error {
	// Marshal body once (if provided) to compute MD5 for signature
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("webull: marshal request body: %w", err)
		}
	}

	// Extract host from c.host for signing
	parsedURL, err := url.Parse(c.host)
	if err != nil {
		parsedURL = &url.URL{Host: "api.webull.com"} // fallback
	}
	host := parsedURL.Host
	if host == "" {
		host = "api.webull.com"
	}

	// Sign the request
	headers := c.signer.SignRequest(path, method, host, query, bodyBytes)

	// Build URL
	u := c.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	// Create request
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse Webull error response
		var apiErr ErrorResponse
		if jerr := json.Unmarshal(respBytes, &apiErr); jerr == nil && apiErr.Message != "" {
			return fmt.Errorf("webull: %s %s: [%d] %s", method, path, resp.StatusCode, apiErr.Message)
		}
		return fmt.Errorf("webull: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(respBytes))
	}

	// Unmarshal response
	if out != nil {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("webull: decode response: %w", err)
		}
	}
	return nil
}

// --- High-level API methods ---

// FetchAccount fetches account info.
func (c *Client) FetchAccount(ctx context.Context) (*AccountResponse, error) {
	path := fmt.Sprintf(endpointAccount, c.accountID)
	var resp AccountResponse
	if err := c.doRequest(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("webull: FetchAccount: %w", err)
	}
	return &resp, nil
}

// FetchBalances fetches all balances for the account.
func (c *Client) FetchBalances(ctx context.Context) (*BalancesResponse, error) {
	path := fmt.Sprintf(endpointBalances, c.accountID)
	var resp BalancesResponse
	if err := c.doRequest(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("webull: FetchBalances: %w", err)
	}
	return &resp, nil
}

// FetchPositions fetches all positions for the account.
func (c *Client) FetchPositions(ctx context.Context) (*PositionsResponse, error) {
	path := fmt.Sprintf(endpointPositions, c.accountID)
	var resp PositionsResponse
	if err := c.doRequest(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("webull: FetchPositions: %w", err)
	}
	return &resp, nil
}

// FetchQuote fetches a quote for a single symbol (e.g., "AAPL").
func (c *Client) FetchQuote(ctx context.Context, symbol string) (*QuoteResponse, error) {
	path := fmt.Sprintf(endpointQuote, symbol)
	var resp QuoteResponse
	if err := c.doRequest(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, fmt.Errorf("webull: FetchQuote %s: %w", symbol, err)
	}
	return &resp, nil
}

// FetchBars fetches candlestick data for a symbol.
// interval: timeframe string (e.g., "1m", "1h", "1d")
// count: number of bars to fetch (limit)
func (c *Client) FetchBars(ctx context.Context, symbol, interval string, count int) (*BarsResponse, error) {
	query := url.Values{}
	query.Set("symbol", symbol)
	query.Set("interval", interval)
	if count > 0 {
		query.Set("limit", fmt.Sprintf("%d", count))
	}

	var resp BarsResponse
	if err := c.doRequest(ctx, http.MethodGet, endpointBars, query, nil, &resp); err != nil {
		return nil, fmt.Errorf("webull: FetchBars %s: %w", symbol, err)
	}
	return &resp, nil
}
