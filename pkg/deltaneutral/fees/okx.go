package fees

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

const okxBaseURL = "https://www.okx.com"

type okxFeesFetcher struct {
	apiKey     string
	apiSecret  string
	passphrase string
	client     *http.Client
}

func newOKXFeesFetcher(creds config.OKXExchangeAccount) (*okxFeesFetcher, error) {
	if creds.APIKey.String() == "" || creds.Secret.String() == "" || creds.Passphrase.String() == "" {
		return nil, fmt.Errorf("okx fees: api_key, secret, and passphrase are required")
	}
	client, err := utils.CreateExchangeHTTPClient("okx", creds.Proxy, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("okx fees: %w", err)
	}
	return &okxFeesFetcher{
		apiKey:     creds.APIKey.String(),
		apiSecret:  creds.Secret.String(),
		passphrase: creds.Passphrase.String(),
		client:     client,
	}, nil
}

// FetchFuturesPositionFees returns accumulated trading and funding fees from OKX.
// Open positions are fetched from GET /api/v5/account/positions (fee + fundingFee fields).
// Closed positions in the [Since, Until] window are fetched from GET /api/v5/account/positions-history.
// Both are summed so the result is correct regardless of whether the position is open or closed.
func (f *okxFeesFetcher) FetchFuturesPositionFees(ctx context.Context, req FetchFeesRequest) (*PositionFees, error) {
	instID := okxInstID(req.FuturesSymbol)

	var tradingFee, fundingFee float64

	// Open positions — carries accumulated fee + fundingFee since entry.
	{
		params := url.Values{}
		params.Set("instType", "SWAP")
		params.Set("instId", instID)

		var resp okxPositionsResponse
		if err := f.signedGet(ctx, "/api/v5/account/positions", params, &resp); err != nil {
			return nil, fmt.Errorf("okx fees: %w", err)
		}
		if resp.Code != "0" {
			return nil, fmt.Errorf("okx fees: API error %s: %s", resp.Code, resp.Msg)
		}
		for _, r := range resp.Data {
			tradingFee += parseFloat(r.Fee)
			fundingFee += parseFloat(r.FundingFee)
		}
	}

	// Closed positions in the time window.
	startMs := strconv.FormatInt(req.Since.UnixMilli(), 10)
	endMs := strconv.FormatInt(req.Until.UnixMilli(), 10)
	after := ""
	for {
		params := url.Values{}
		params.Set("instType", "SWAP")
		params.Set("instId", instID)
		params.Set("startTime", startMs)
		params.Set("endTime", endMs)
		params.Set("limit", "100")
		if after != "" {
			params.Set("after", after)
		}

		var resp okxPositionsHistoryResponse
		if err := f.signedGet(ctx, "/api/v5/account/positions-history", params, &resp); err != nil {
			return nil, fmt.Errorf("okx fees: %w", err)
		}
		if resp.Code != "0" {
			return nil, fmt.Errorf("okx fees: API error %s: %s", resp.Code, resp.Msg)
		}

		for _, r := range resp.Data {
			tradingFee += parseFloat(r.Fee)
			fundingFee += parseFloat(r.FundingFee)
		}

		if len(resp.Data) < 100 {
			break
		}
		after = resp.Data[len(resp.Data)-1].UTime
	}

	return &PositionFees{
		TradingFeeUSDT: tradingFee,
		FundingFeeUSDT: fundingFee,
		PeriodStart:    req.Since,
		PeriodEnd:      req.Until,
		FetchedAt:      time.Now().UTC(),
	}, nil
}

// signedGet sends a signed GET request to an authenticated OKX REST v5 endpoint.
// OKX signs: HMAC-SHA256(secret, timestamp+"GET"+requestPath+"?"+queryString)
// and base64-encodes the result.
func (f *okxFeesFetcher) signedGet(ctx context.Context, path string, params url.Values, out interface{}) error {
	queryString := params.Encode()
	timestamp := time.Now().UTC().Format(time.RFC3339)
	prehash := timestamp + "GET" + path + "?" + queryString

	mac := hmac.New(sha256.New, []byte(f.apiSecret))
	mac.Write([]byte(prehash))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, okxBaseURL+path+"?"+queryString, nil)
	if err != nil {
		return err
	}
	req.Header.Set("OK-ACCESS-KEY", f.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", f.passphrase)
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
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

// okxInstID converts a CCXT futures symbol to an OKX instrument ID.
// "CHZ/USDT:USDT" → "CHZ-USDT-SWAP"
func okxInstID(ccxtSymbol string) string {
	parts := strings.SplitN(ccxtSymbol, "/", 2)
	if len(parts) != 2 {
		return ccxtSymbol
	}
	base := parts[0]
	quoteParts := strings.SplitN(parts[1], ":", 2)
	quote := quoteParts[0]
	return base + "-" + quote + "-SWAP"
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

type okxPositionsResponse struct {
	Code string           `json:"code"`
	Msg  string           `json:"msg"`
	Data []okxPositionRow `json:"data"`
}

type okxPositionRow struct {
	Fee        string `json:"fee"`
	FundingFee string `json:"fundingFee"`
}

type okxPositionsHistoryResponse struct {
	Code string                  `json:"code"`
	Msg  string                  `json:"msg"`
	Data []okxPositionHistoryRow `json:"data"`
}

type okxPositionHistoryRow struct {
	Fee        string `json:"fee"`
	FundingFee string `json:"fundingFee"`
	UTime      string `json:"uTime"` // update time in ms, used for pagination
}
