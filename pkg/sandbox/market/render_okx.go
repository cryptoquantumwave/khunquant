package market

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// okxBars are the candle `bar` values ccxt sends; the utc suffix it adds to bars of six
// hours and up is stripped before the lookup.
var okxBars = map[string]time.Duration{
	"1m": time.Minute, "3m": 3 * time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute, "30m": 30 * time.Minute,
	"1H": time.Hour, "2H": 2 * time.Hour, "4H": 4 * time.Hour, "6H": 6 * time.Hour, "12H": 12 * time.Hour,
	"1D": 24 * time.Hour, "1W": 168 * time.Hour, "1M": 720 * time.Hour,
}

func okxReply(data any) (int, []byte, bool) {
	return reply(200, map[string]any{"code": "0", "msg": "", "data": data})
}

// okxTickers serves /api/v5/market/ticker (instId) and /tickers (instType SPOT or SWAP).
// OKX's spot and swap tickers differ only in the instId suffix and instType.
func (b *Book) okxTickers(all bool, q url.Values) (int, []byte, bool) {
	type pairSwap struct {
		p    Pair
		swap bool
	}
	var want []pairSwap
	if all {
		it := q.Get("instType")
		if it != "SPOT" && it != "SWAP" {
			return 0, nil, false
		}
		for _, p := range b.Pairs("okx") {
			want = append(want, pairSwap{p, it == "SWAP"})
		}
	} else if p, swap, ok := ParseWire("okx", q.Get("instId")); ok {
		want = []pairSwap{{p, swap}}
	}
	now, rows := b.Now(), []map[string]string{}
	for _, w := range want {
		qt, ok := b.quote("okx", w.p)
		if !ok {
			continue
		}
		instType := "SPOT"
		if w.swap {
			instType = "SWAP"
		}
		rows = append(rows, map[string]string{
			"instType": instType, "instId": Wire("okx", w.p, w.swap), "last": num(qt.Last), "lastSz": "1",
			"askPx": num(qt.ask), "askSz": "1", "bidPx": num(qt.bid), "bidSz": "1", "open24h": num(qt.Open),
			"high24h": num(qt.High), "low24h": num(qt.Low), "volCcy24h": num(qt.Last), "vol24h": "1", "ts": strconv.FormatInt(ms(now), 10),
		})
	}
	if len(rows) == 0 {
		return 0, nil, false
	}
	return okxReply(rows)
}

// okxCandles serves /api/v5/market/candles and /history-candles: newest first, the
// forming bar (confirm "0") first. `after` returns bars older than it and `before` newer,
// both exclusive; with neither, the newest `limit`.
func (b *Book) okxCandles(q url.Values) (int, []byte, bool) {
	p, _, ok := ParseWire("okx", q.Get("instId"))
	if !ok {
		return 0, nil, false
	}
	now := b.Now()
	if _, ok := b.Price("okx", p, now); !ok {
		return 0, nil, false
	}
	tf, ok := okxBars[strings.TrimSuffix(q.Get("bar"), "utc")]
	if !ok {
		return reply(200, map[string]any{"code": "51000", "msg": "Parameter bar error", "data": []any{}})
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		limit = 100
	}
	limit = min(limit, 300)
	to := now
	if a, ok := queryMS(q, "after"); ok && a.Before(now) {
		to = a.Add(-time.Millisecond)
	}
	from := alignDown(to, tf).Add(-time.Duration(limit-1) * tf)
	if bf, ok := queryMS(q, "before"); ok && bf.Add(time.Millisecond).After(from) {
		from = bf.Add(time.Millisecond)
	}
	bars := b.Bars("okx", p, tf, from, to)
	rows := [][]string{}
	for i := len(bars) - 1; i >= 0 && len(rows) < limit; i-- {
		bar, confirm := bars[i], "0"
		if bar.Open.Before(from) {
			break // the bar holding `before` itself is not newer than it
		}
		if bar.Closed {
			confirm = "1"
		}
		rows = append(rows, []string{strconv.FormatInt(ms(bar.Open), 10), num(bar.O), num(bar.H), num(bar.L), num(bar.C),
			num(bar.V), num(bar.V), num(bar.V), confirm})
	}
	return okxReply(rows)
}
