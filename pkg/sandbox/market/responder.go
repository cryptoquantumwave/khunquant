package market

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Middleware is the book's `market` stage. It answers the venues' price routes from the
// models and calls next for everything else, including any symbol or batch the book has
// no model for: the fixture or the router's 404 then answers, so it fails closed. It also
// refuses orders for, and filters the listings of, delisted symbols (delist.go).
func (b *Book) Middleware() sandbox.Middleware {
	return func(next sandbox.Responder) sandbox.Responder {
		return sandbox.ResponderFunc(func(venue, method, path string, r *http.Request) (*sandbox.Response, bool) {
			if r == nil || r.URL == nil {
				return next.Respond(venue, method, path, r)
			}
			answer := func(status int, body []byte) (*sandbox.Response, bool) {
				return &sandbox.Response{Status: status, Body: body, Headers: map[string]string{"Content-Type": "application/json"}}, true
			}
			switch {
			case method == http.MethodGet:
				if status, body, ok := b.render(venue, path, r.URL.Query()); ok {
					return answer(status, body)
				}
				if venue+path == "bitkub/api/v3/market/ticker" {
					if resp, ok := next.Respond(venue, method, path, r); ok {
						return b.filterTickerArray(resp), true
					}
					return nil, false
				}
				if _, ok := listings[venue+path]; ok {
					if resp, ok := next.Respond(venue, method, path, r); ok {
						return b.filterListing(venue, path, r.URL.Query(), resp), true
					}
					return nil, false
				}
			case method == http.MethodPost && b.refusesOrder(venue, path, r):
				if status, body, ok := invalidSymbol(venue); ok {
					return answer(status, body)
				}
			}
			return next.Respond(venue, method, path, r)
		})
	}
}

func (b *Book) render(venue, path string, q url.Values) (int, []byte, bool) {
	if status, body, ok := b.delistedReply(venue, path, q); ok {
		return status, body, true
	}
	switch venue + path {
	case "binance/api/v3/ticker/24hr", "binance/fapi/v1/ticker/24hr":
		return b.binanceTickers(path == "/api/v3/ticker/24hr", q)
	case "binance/api/v3/klines", "binance/fapi/v1/klines":
		return b.binanceKlines(q)
	case "okx/api/v5/market/ticker", "okx/api/v5/market/tickers":
		return b.okxTickers(path == "/api/v5/market/tickers", q)
	case "okx/api/v5/market/candles", "okx/api/v5/market/history-candles":
		return b.okxCandles(q)
	case "binance/fapi/v1/premiumIndex":
		return b.binancePremium(q)
	case "okx/api/v5/public/mark-price":
		return b.okxMarkPrice(q)
	case "okx/api/v5/public/funding-rate":
		return b.okxFundingRate(q)
	case "bitkub/api/v3/market/ticker":
		return b.bitkubTickers(q)
	case "bitkub/tradingview/history":
		return b.bitkubHistory(q)
	}
	return 0, nil, false
}

func reply(status int, v any) (int, []byte, bool) {
	body, err := json.Marshal(v)
	return status, body, err == nil
}

// num formats a price exactly: the wire carries what the model says, to the last digit.
func num(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

func ms(t time.Time) int64 { return t.UnixMilli() }

// queryMS reads a millisecond timestamp parameter.
func queryMS(q url.Values, key string) (time.Time, bool) {
	n, err := strconv.ParseInt(q.Get(key), 10, 64)
	return time.UnixMilli(n).UTC(), err == nil && n > 0
}

// quote is a pair's 24 h statistics with a 1 bp spread, as the tickers need them.
type quote struct {
	Stats24h
	bid, ask float64
}

func (b *Book) quote(venue string, p Pair) (quote, bool) {
	s, ok := b.Stats24h(venue, p)
	return quote{s, s.Last * (1 - 1e-4), s.Last * (1 + 1e-4)}, ok
}
