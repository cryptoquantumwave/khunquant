package market

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// binanceIntervals are the kline intervals ccxt sends (1M is approximated as 30 days).
var binanceIntervals = map[string]time.Duration{
	"1s": time.Second, "1m": time.Minute, "3m": 3 * time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"30m": 30 * time.Minute, "1h": time.Hour, "2h": 2 * time.Hour, "4h": 4 * time.Hour, "6h": 6 * time.Hour,
	"8h": 8 * time.Hour, "12h": 12 * time.Hour, "1d": 24 * time.Hour, "3d": 72 * time.Hour, "1w": 168 * time.Hour,
	"1M": 720 * time.Hour,
}

// binTicker is the part of /ticker/24hr that ccxt's parser reads. Spot rows carry the
// bid/ask quantities (ccxt tells spot from futures by their presence); volume is synthetic.
type binTicker struct {
	Symbol             string `json:"symbol"`
	PriceChange        string `json:"priceChange"`
	PriceChangePercent string `json:"priceChangePercent"`
	WeightedAvgPrice   string `json:"weightedAvgPrice"`
	LastPrice          string `json:"lastPrice"`
	BidPrice           string `json:"bidPrice"`
	BidQty             string `json:"bidQty,omitempty"`
	AskPrice           string `json:"askPrice"`
	AskQty             string `json:"askQty,omitempty"`
	OpenPrice          string `json:"openPrice"`
	HighPrice          string `json:"highPrice"`
	LowPrice           string `json:"lowPrice"`
	Volume             string `json:"volume"`
	QuoteVolume        string `json:"quoteVolume"`
	OpenTime           int64  `json:"openTime"`
	CloseTime          int64  `json:"closeTime"`
}

// binanceTickers serves /api/v3/ticker/24hr (spot) and /fapi/v1/ticker/24hr: one symbol
// as an object, `symbols` (a JSON array) or none as an array of the modelled pairs.
func (b *Book) binanceTickers(spot bool, q url.Values) (int, []byte, bool) {
	var wires []string
	switch single, many := q.Get("symbol"), q.Get("symbols"); {
	case single != "":
		wires = []string{single}
	case many != "":
		if json.Unmarshal([]byte(many), &wires) != nil {
			return 0, nil, false
		}
	default:
		for _, p := range b.Pairs("binance") {
			wires = append(wires, Wire("binance", p, false))
		}
	}
	now, out := b.Now(), []binTicker{}
	for _, w := range wires {
		p, _, ok := ParseWire("binance", w)
		if !ok {
			continue
		}
		qt, ok := b.quote("binance", p)
		if !ok {
			continue
		}
		t := binTicker{
			Symbol: w, PriceChange: num(qt.Last - qt.Open), PriceChangePercent: num((qt.Last - qt.Open) / qt.Open * 100),
			WeightedAvgPrice: num(qt.Last), LastPrice: num(qt.Last), BidPrice: num(qt.bid), AskPrice: num(qt.ask),
			OpenPrice: num(qt.Open), HighPrice: num(qt.High), LowPrice: num(qt.Low), Volume: "1", QuoteVolume: num(qt.Last),
			OpenTime: ms(now.Add(-24 * time.Hour)), CloseTime: ms(now),
		}
		if spot {
			t.BidQty, t.AskQty = "1", "1"
		}
		out = append(out, t)
	}
	switch {
	case len(out) == 0:
		return 0, nil, false
	case q.Get("symbol") != "":
		return reply(200, out[0])
	}
	return reply(200, out)
}

// binanceKlines serves /api/v3/klines and /fapi/v1/klines: oldest first, the forming
// bar last. Without startTime it is the `limit` bars ending at endTime (default now).
func (b *Book) binanceKlines(q url.Values) (int, []byte, bool) {
	p, _, ok := ParseWire("binance", q.Get("symbol"))
	if !ok {
		return 0, nil, false
	}
	tf, ok := binanceIntervals[q.Get("interval")]
	if !ok {
		return reply(400, map[string]any{"code": -1120, "msg": "Invalid interval."})
	}
	now := b.Now()
	if _, ok := b.Price("binance", p, now); !ok {
		return 0, nil, false
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		limit = 500
	}
	limit = min(limit, 1000)
	to := now
	if e, ok := queryMS(q, "endTime"); ok && e.Before(now) {
		to = e
	}
	from, hasStart := queryMS(q, "startTime")
	if !hasStart {
		from = alignDown(to, tf).Add(-time.Duration(limit-1) * tf)
	}
	var rows [][]any
	for _, bar := range b.Bars("binance", p, tf, from, to) {
		if hasStart && bar.Open.Before(from) {
			continue
		}
		rows = append(rows, []any{ms(bar.Open), num(bar.O), num(bar.H), num(bar.L), num(bar.C), num(bar.V),
			ms(bar.Open.Add(tf)) - 1, "0", 0, "0", "0", "0"})
	}
	if len(rows) > limit {
		if hasStart {
			rows = rows[:limit]
		} else {
			rows = rows[len(rows)-limit:]
		}
	}
	if rows == nil {
		rows = [][]any{}
	}
	return reply(200, rows)
}
