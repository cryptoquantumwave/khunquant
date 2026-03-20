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

	"github.com/khunquant/khunquant/pkg/config"
	"github.com/khunquant/khunquant/pkg/exchanges"
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
	if creds.APIKey == "" || creds.Secret == "" {
		return nil, fmt.Errorf("binanceth: api_key and secret are required")
	}
	return &BinanceTHExchange{
		apiKey:    creds.APIKey,
		apiSecret: creds.Secret,
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
