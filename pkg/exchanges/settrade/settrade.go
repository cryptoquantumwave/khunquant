// Package settrade provides a broker adapter for the SETTRADE Open API (SDK v2).
package settrade

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/logger"
)

// Name is the canonical provider identifier used in config and tool calls.
const Name = "settrade"

// internal auth types — not part of the public domain model
type loginRequest struct {
	APIKey    string `json:"apiKey"`
	Params    string `json:"params"`
	Signature string `json:"signature"`
	Timestamp string `json:"timestamp"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	APIKey       string `json:"apiKey"`
}

// SettradeClient is an authenticated HTTP client for the SETTRADE Open API.
type SettradeClient struct {
	cfg        config.SettradeExchangeAccount
	httpClient *http.Client
	privateKey *ecdsa.PrivateKey

	mu           sync.Mutex
	accessToken  string
	refreshToken string
	tokenExpiry  time.Time
}

// NewSettradeClient creates a new client and decodes the ECDSA private key.
func NewSettradeClient(cfg config.SettradeExchangeAccount) (*SettradeClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("settrade: api_key (App ID) is required")
	}
	if cfg.Secret == "" {
		return nil, fmt.Errorf("settrade: secret (App Secret) is required")
	}
	if cfg.BrokerID == "" {
		return nil, fmt.Errorf("settrade: broker_id is required")
	}
	if cfg.AppCode == "" {
		return nil, fmt.Errorf("settrade: app_code is required")
	}
	if cfg.AccountNo == "" {
		return nil, fmt.Errorf("settrade: account_no is required")
	}

	key, err := loadPrivateKey(cfg.Secret)
	if err != nil {
		return nil, err
	}

	return &SettradeClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		privateKey: key,
	}, nil
}

// --- Token lifecycle ---

func (c *SettradeClient) login(ctx context.Context) error {
	const params = ""
	sigHex, tsMs, err := sign(c.privateKey, c.cfg.APIKey, params)
	if err != nil {
		return err
	}

	reqBody := loginRequest{
		APIKey:    c.cfg.APIKey,
		Params:    params,
		Signature: sigHex,
		Timestamp: fmt.Sprintf("%d", tsMs),
	}

	path := fmt.Sprintf("/api/oam/v1/%s/broker-apps/%s/login", c.cfg.BrokerID, c.cfg.AppCode)
	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, baseURL, path, nil, reqBody, &tok, false); err != nil {
		return fmt.Errorf("settrade: login: %w", err)
	}

	c.accessToken = tok.AccessToken
	c.refreshToken = tok.RefreshToken
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	logger.DebugCF("settrade", "login successful", map[string]any{"expires_in": tok.ExpiresIn})
	return nil
}

func (c *SettradeClient) refreshTokens(ctx context.Context) error {
	path := fmt.Sprintf("/api/oam/v1/%s/broker-apps/%s/refresh-token", c.cfg.BrokerID, c.cfg.AppCode)
	body := refreshRequest{RefreshToken: c.refreshToken, APIKey: c.cfg.APIKey}

	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, baseURL, path, nil, body, &tok, false); err != nil {
		return fmt.Errorf("settrade: refresh token: %w", err)
	}

	c.accessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		c.refreshToken = tok.RefreshToken
	}
	c.tokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return nil
}

func (c *SettradeClient) ensureToken(ctx context.Context) error {
	if c.accessToken != "" && time.Now().Before(c.tokenExpiry.Add(-30*time.Second)) {
		return nil
	}
	if c.refreshToken != "" {
		if err := c.refreshTokens(ctx); err == nil {
			return nil
		}
		logger.WarnCF("settrade", "refresh token failed, re-logging in", nil)
	}
	return c.login(ctx)
}

// --- Core HTTP helpers ---

func (c *SettradeClient) doRequest(ctx context.Context, method, host, path string, query url.Values, body, out interface{}, auth bool) error {
	u := host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("settrade: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("settrade: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("settrade: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("settrade: read response body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("settrade: HTTP 401 unauthorized")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Parse Settrade structured error: {"code":"OA-LOGIN-U-591","message":"..."}
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if jerr := json.Unmarshal(respBytes, &apiErr); jerr == nil && apiErr.Code != "" {
			if apiErr.Code == "OA-LOGIN-U-591" || strings.Contains(apiErr.Message, "System Unavailable") {
				return fmt.Errorf("settrade: SET market is closed — Settrade auth service is only available during trading hours (Mon–Fri 10:00–12:30 and 14:00–16:30 ICT)")
			}
			return fmt.Errorf("settrade: %s %s: %s — %s", method, path, apiErr.Code, apiErr.Message)
		}
		return fmt.Errorf("settrade: %s %s: HTTP %d: %s", method, path, resp.StatusCode, string(respBytes))
	}

	if out != nil {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("settrade: decode response: %w", err)
		}
	}
	return nil
}

func (c *SettradeClient) doAuth(ctx context.Context, method, host, path string, query url.Values, body, out interface{}) error {
	c.mu.Lock()
	if err := c.ensureToken(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	err := c.doRequest(ctx, method, host, path, query, body, out, true)
	if err != nil && isUnauthorized(err) {
		c.mu.Lock()
		loginErr := c.login(ctx)
		c.mu.Unlock()
		if loginErr != nil {
			return loginErr
		}
		return c.doRequest(ctx, method, host, path, query, body, out, true)
	}
	return err
}

func (c *SettradeClient) get(ctx context.Context, path string, query url.Values, out interface{}) error {
	return c.doAuth(ctx, http.MethodGet, baseURL, path, query, nil, out)
}

func (c *SettradeClient) marketGet(ctx context.Context, path string, query url.Values, out interface{}) error {
	return c.doAuth(ctx, http.MethodGet, marketBaseURL, path, query, nil, out)
}

func (c *SettradeClient) post(ctx context.Context, path string, body, out interface{}) error {
	return c.doAuth(ctx, http.MethodPost, baseURL, path, nil, body, out)
}

func (c *SettradeClient) patch(ctx context.Context, path string, body, out interface{}) error {
	return c.doAuth(ctx, http.MethodPatch, baseURL, path, nil, body, out)
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) >= 22 && err.Error()[:22] == "settrade: HTTP 401 una"
}

// --- SET price tier tick size ---

func roundToTickSize(price float64) float64 {
	var tick float64
	switch {
	case price < 2.00:
		tick = 0.01
	case price < 5.00:
		tick = 0.02
	case price < 10.00:
		tick = 0.05
	case price < 25.00:
		tick = 0.10
	case price < 50.00:
		tick = 0.25
	case price < 100.00:
		tick = 0.50
	default:
		tick = 1.00
	}
	return float64(int(price/tick)) * tick
}

// --- Domain methods ---

// FetchAccountInfo returns the account's cash summary.
func (c *SettradeClient) FetchAccountInfo(ctx context.Context) (accountInfoResponse, error) {
	path := fmt.Sprintf(endpointEQAccountInfo, c.cfg.BrokerID, c.cfg.AccountNo)
	var resp accountInfoResponse
	return resp, c.get(ctx, path, nil, &resp)
}

// FetchPortfolio returns stock holdings for the account.
func (c *SettradeClient) FetchPortfolio(ctx context.Context) (portfolioResponse, error) {
	path := fmt.Sprintf(endpointEQPortfolio, c.cfg.BrokerID, c.cfg.AccountNo)
	var resp portfolioResponse
	return resp, c.get(ctx, path, nil, &resp)
}

// FetchQuote returns the latest price quote for a symbol (e.g. "PTT").
func (c *SettradeClient) FetchQuote(ctx context.Context, symbol string) (quoteResponse, error) {
	path := fmt.Sprintf(endpointMarketQuote, c.cfg.BrokerID, symbol)
	var resp quoteResponse
	return resp, c.marketGet(ctx, path, nil, &resp)
}

// CreateEQOrder places an equity order. PIN is sent inline (SDK v2).
func (c *SettradeClient) CreateEQOrder(ctx context.Context, symbol, side, priceType string, volume int, price float64) (settradeOrder, error) {
	if c.cfg.PIN == "" {
		return settradeOrder{}, fmt.Errorf("settrade: pin is required for order placement")
	}
	if priceType == "Limit" {
		price = roundToTickSize(price)
	}

	req := createOrderRequest{
		PIN:           c.cfg.PIN,
		Side:          side,
		Symbol:        symbol,
		TrusteeIDType: "Local",
		Volume:        volume,
		QtyOpen:       0,
		Price:         price,
		PriceType:     priceType,
		ValidityType:  "Day",
		ClientType:    "Individual",
	}

	path := fmt.Sprintf(endpointEQOrders, c.cfg.BrokerID, c.cfg.AccountNo)
	var resp orderResponse
	return resp.Data, c.post(ctx, path, req, &resp)
}

// CancelEQOrder cancels an order by order number (PATCH + pin).
func (c *SettradeClient) CancelEQOrder(ctx context.Context, orderNo string) (settradeOrder, error) {
	if c.cfg.PIN == "" {
		return settradeOrder{}, fmt.Errorf("settrade: pin is required to cancel orders")
	}
	path := fmt.Sprintf(endpointEQOrderCancel, c.cfg.BrokerID, c.cfg.AccountNo, orderNo)
	body := cancelOrderRequest{PIN: c.cfg.PIN}
	var resp orderResponse
	return resp.Data, c.patch(ctx, path, body, &resp)
}

// FetchEQOrder returns a single order's details.
func (c *SettradeClient) FetchEQOrder(ctx context.Context, orderNo string) (settradeOrder, error) {
	path := fmt.Sprintf(endpointEQOrder, c.cfg.BrokerID, c.cfg.AccountNo, orderNo)
	var resp orderResponse
	return resp.Data, c.get(ctx, path, nil, &resp)
}

// FetchOpenEQOrders returns all open orders, optionally filtered by symbol.
func (c *SettradeClient) FetchOpenEQOrders(ctx context.Context, symbol string) ([]settradeOrder, error) {
	path := fmt.Sprintf(endpointEQOrders, c.cfg.BrokerID, c.cfg.AccountNo)
	q := url.Values{}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	var resp []settradeOrder
	return resp, c.get(ctx, path, q, &resp)
}

// FetchClosedEQOrders returns matched/cancelled orders.
func (c *SettradeClient) FetchClosedEQOrders(ctx context.Context, symbol string, limit int) ([]settradeOrder, error) {
	path := fmt.Sprintf(endpointEQOrders, c.cfg.BrokerID, c.cfg.AccountNo)
	q := url.Values{"status": {"matched"}}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp []settradeOrder
	return resp, c.get(ctx, path, q, &resp)
}

// ChangeEQOrder modifies price or volume of a pending order.
// newVolume is the new volume to set; pass 0 to leave unchanged.
func (c *SettradeClient) ChangeEQOrder(ctx context.Context, orderNo string, newPrice float64, newVolume int) (settradeOrder, error) {
	if c.cfg.PIN == "" {
		return settradeOrder{}, fmt.Errorf("settrade: pin is required to change orders")
	}
	req := changeOrderRequest{PIN: c.cfg.PIN}
	if newPrice > 0 {
		p := roundToTickSize(newPrice)
		req.NewPrice = &p
	}
	if newVolume > 0 {
		req.NewVolume = &newVolume
	}
	path := fmt.Sprintf(endpointEQOrderChange, c.cfg.BrokerID, c.cfg.AccountNo, orderNo)
	var resp orderResponse
	return resp.Data, c.patch(ctx, path, req, &resp)
}

// CancelAllEQOrders cancels multiple orders by order number in one request.
func (c *SettradeClient) CancelAllEQOrders(ctx context.Context, orderNos []string) error {
	if c.cfg.PIN == "" {
		return fmt.Errorf("settrade: pin is required to cancel orders")
	}
	path := fmt.Sprintf(endpointEQCancelOrders, c.cfg.BrokerID, c.cfg.AccountNo)
	body := cancelOrdersRequest{PIN: c.cfg.PIN, Orders: orderNos}
	return c.patch(ctx, path, body, nil)
}

// FetchCandlestick returns OHLCV bars for a symbol.
// interval: '1m','3m','5m','10m','15m','30m','60m','120m','240m','1d','1w','1M'
// limit: number of bars (0 = server default); start/end: "YYYY-mm-ddTHH:MM" or empty.
func (c *SettradeClient) FetchCandlestick(ctx context.Context, symbol, interval string, limit int, start, end string, normalized bool) ([]OHLCV, error) {
	path := fmt.Sprintf(endpointMarketCandlestick, c.cfg.BrokerID)
	q := url.Values{
		"symbol":   {symbol},
		"interval": {interval},
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if start != "" {
		q.Set("start", start)
	}
	if end != "" {
		q.Set("end", end)
	}
	if normalized {
		q.Set("normalized", "true")
	}

	// The API returns a single object where each field is an array of values.
	var raw candlestickBar
	if err := c.marketGet(ctx, path, q, &raw); err != nil {
		return nil, err
	}

	out := make([]OHLCV, 0, len(raw.Time))
	for i, t := range raw.Time {
		o := OHLCV{Time: t}
		if i < len(raw.Open) {
			o.Open = raw.Open[i]
		}
		if i < len(raw.High) {
			o.High = raw.High[i]
		}
		if i < len(raw.Low) {
			o.Low = raw.Low[i]
		}
		if i < len(raw.Close) {
			o.Close = raw.Close[i]
		}
		if i < len(raw.Volume) {
			o.Volume = raw.Volume[i]
		}
		if i < len(raw.Value) {
			o.Value = raw.Value[i]
		}
		out = append(out, o)
	}
	return out, nil
}

// FetchTrades returns trade history for the account (SEOS v4).
func (c *SettradeClient) FetchTrades(ctx context.Context, symbol string, limit int) ([]tradeRecord, error) {
	path := fmt.Sprintf(endpointEQTrades, c.cfg.BrokerID, c.cfg.AccountNo)
	q := url.Values{}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	var resp tradesResponse
	return resp.Data, c.get(ctx, path, q, &resp)
}
