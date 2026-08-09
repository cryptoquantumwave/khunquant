package fees

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

	"golang.org/x/sync/errgroup"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/utils"
)

const binanceFuturesBaseURL = "https://fapi.binance.com"

type binanceFeesFetcher struct {
	apiKey    string
	apiSecret string
	client    *http.Client
}

func newBinanceFeesFetcher(creds config.ExchangeAccount) (*binanceFeesFetcher, error) {
	if creds.APIKey.String() == "" || creds.Secret.String() == "" {
		return nil, fmt.Errorf("binance fees: api_key and secret are required")
	}
	client, err := utils.CreateExchangeHTTPClient("binance", creds.Proxy, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("binance fees: %w", err)
	}
	return &binanceFeesFetcher{
		apiKey:    creds.APIKey.String(),
		apiSecret: creds.Secret.String(),
		client:    client,
	}, nil
}

// FetchFuturesPositionFees returns accumulated trading and funding fees from
// Binance USDM GET /fapi/v1/income, fetching COMMISSION and FUNDING_FEE in parallel.
func (f *binanceFeesFetcher) FetchFuturesPositionFees(ctx context.Context, req FetchFeesRequest) (*PositionFees, error) {
	symbol := binanceSymbol(req.FuturesSymbol)
	startMs := req.Since.UnixMilli()
	endMs := req.Until.UnixMilli()

	var tradingFee, fundingFee float64

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		v, err := f.fetchIncomeAll(gctx, symbol, "COMMISSION", startMs, endMs)
		if err != nil {
			return fmt.Errorf("COMMISSION: %w", err)
		}
		tradingFee = v
		return nil
	})
	g.Go(func() error {
		v, err := f.fetchIncomeAll(gctx, symbol, "FUNDING_FEE", startMs, endMs)
		if err != nil {
			return fmt.Errorf("FUNDING_FEE: %w", err)
		}
		fundingFee = v
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("binance fees: %w", err)
	}

	return &PositionFees{
		TradingFeeUSDT: tradingFee,
		FundingFeeUSDT: fundingFee,
		PeriodStart:    req.Since,
		PeriodEnd:      req.Until,
		FetchedAt:      time.Now().UTC(),
	}, nil
}

// fetchIncomeAll paginates through /fapi/v1/income for a given incomeType,
// accumulating the sum of all income values.
func (f *binanceFeesFetcher) fetchIncomeAll(ctx context.Context, symbol, incomeType string, startMs, endMs int64) (float64, error) {
	const pageSize = 1000
	var total float64
	cursor := startMs

	for {
		params := url.Values{}
		params.Set("symbol", symbol)
		params.Set("incomeType", incomeType)
		params.Set("startTime", strconv.FormatInt(cursor, 10))
		params.Set("endTime", strconv.FormatInt(endMs, 10))
		params.Set("limit", strconv.Itoa(pageSize))

		var records []binanceIncomeRecord
		if err := f.signedGet(ctx, "/fapi/v1/income", params, &records); err != nil {
			return 0, err
		}

		for _, r := range records {
			total += parseFloat(r.Income)
		}

		if len(records) < pageSize {
			break
		}
		// Advance cursor past the last record to avoid overlap.
		cursor = records[len(records)-1].Time + 1
	}

	return total, nil
}

// signedGet sends a HMAC-SHA256 signed GET request to a Binance USDM endpoint.
// Auth: timestamp+signature appended to query, API key in X-MBX-APIKEY header.
func (f *binanceFeesFetcher) signedGet(ctx context.Context, path string, params url.Values, out interface{}) error {
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	queryString := params.Encode()

	mac := hmac.New(sha256.New, []byte(f.apiSecret))
	mac.Write([]byte(queryString))
	queryString += "&signature=" + hex.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binanceFuturesBaseURL+path+"?"+queryString, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-MBX-APIKEY", f.apiKey)

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

// binanceSymbol converts a CCXT futures symbol to a Binance USDM symbol.
// "CHZ/USDT:USDT" → "CHZUSDT"
func binanceSymbol(ccxtSymbol string) string {
	parts := strings.SplitN(ccxtSymbol, "/", 2)
	if len(parts) != 2 {
		return ccxtSymbol
	}
	base := parts[0]
	quoteParts := strings.SplitN(parts[1], ":", 2)
	quote := quoteParts[0]
	return base + quote
}

type binanceIncomeRecord struct {
	Symbol     string `json:"symbol"`
	IncomeType string `json:"incomeType"`
	Income     string `json:"income"`
	Asset      string `json:"asset"`
	Time       int64  `json:"time"`
}
