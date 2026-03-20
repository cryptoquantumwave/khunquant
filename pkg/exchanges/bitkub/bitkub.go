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
	"strconv"
	"strings"
	"time"

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

// balancesResponse is the response structure for POST /api/v3/market/balances.
type balancesResponse struct {
	Error  int                        `json:"error"`
	Result map[string]assetBalance    `json:"result"`
}

type assetBalance struct {
	Available float64 `json:"available"`
	Reserved  float64 `json:"reserved"`
}

// tickerEntry is one entry from GET /api/v3/market/ticker.
type tickerEntry struct {
	Symbol string `json:"symbol"`
	Last   string `json:"last"`
}

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

// fetchTickers returns a map of symbol → last price from GET /api/v3/market/ticker.
func (b *BitkubExchange) fetchTickers(ctx context.Context) (map[string]float64, error) {
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

	out := make(map[string]float64, len(entries))
	for _, e := range entries {
		last, err := strconv.ParseFloat(e.Last, 64)
		if err != nil {
			continue
		}
		out[e.Symbol] = last
	}
	return out, nil
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
