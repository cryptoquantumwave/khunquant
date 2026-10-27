package market

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

// bitkubResolutions are the TradingView resolutions the adapter sends.
var bitkubResolutions = map[string]time.Duration{
	"1": time.Minute, "3": 3 * time.Minute, "5": 5 * time.Minute, "15": 15 * time.Minute, "30": 30 * time.Minute,
	"60": time.Hour, "120": 2 * time.Hour, "240": 4 * time.Hour, "360": 6 * time.Hour, "720": 12 * time.Hour,
	"1D": 24 * time.Hour, "1W": 168 * time.Hour, "1M": 720 * time.Hour,
}

// maxBitkubBars bounds one history answer to the newest bars (the adapter asks for a week).
const maxBitkubBars = 1000

type bitkubTicker struct {
	Symbol        string      `json:"symbol"`
	Last          json.Number `json:"last"`
	LowestAsk     json.Number `json:"lowest_ask"`
	HighestBid    json.Number `json:"highest_bid"`
	PercentChange json.Number `json:"percent_change"`
	BaseVolume    json.Number `json:"base_volume"`
	QuoteVolume   json.Number `json:"quote_volume"`
	High24Hr      json.Number `json:"high_24_hr"`
	Low24Hr       json.Number `json:"low_24_hr"`
}

// bitkubTickers serves /api/v3/market/ticker: a plain array of the modelled THB pairs
// (those not delisted), or the one named by `sym`. Bitkub quotes only in THB.
func (b *Book) bitkubTickers(q url.Values) (int, []byte, bool) {
	var pairs []Pair
	if sym := q.Get("sym"); sym != "" {
		if p, _, ok := ParseWire("bitkub", sym); ok && !b.IsDelisted("bitkub", p) {
			pairs = []Pair{p}
		}
	} else {
		for _, p := range b.Pairs("bitkub") {
			if p.Quote == "THB" {
				pairs = append(pairs, p)
			}
		}
	}
	rows := []bitkubTicker{}
	for _, p := range pairs {
		qt, ok := b.quote("bitkub", p)
		if !ok || p.Quote != "THB" {
			continue
		}
		n := func(x float64) json.Number { return json.Number(num(x)) }
		rows = append(rows, bitkubTicker{
			Symbol: Wire("bitkub", p, false), Last: n(qt.Last), LowestAsk: n(qt.ask), HighestBid: n(qt.bid),
			PercentChange: n((qt.Last - qt.Open) / qt.Open * 100), BaseVolume: "1", QuoteVolume: n(qt.Last),
			High24Hr: n(qt.High), Low24Hr: n(qt.Low),
		})
	}
	if len(rows) == 0 {
		if b.symDelisted("bitkub", q.Get("sym")) {
			return reply(200, rows) // delisted: Bitkub omits it
		}
		return 0, nil, false
	}
	return reply(200, rows)
}

func (b *Book) symDelisted(venue, wire string) bool {
	p, _, ok := ParseWire(venue, wire)
	return ok && b.IsDelisted(venue, p)
}

// bitkubHistory serves /tradingview/history: parallel arrays, oldest first, the forming
// bar last. Delisted or unmodelled symbols get no_data / fall through respectively.
func (b *Book) bitkubHistory(q url.Values) (int, []byte, bool) {
	p, _, ok := ParseWire("bitkub", q.Get("symbol"))
	tf, tfOK := bitkubResolutions[q.Get("resolution")]
	if !ok || !tfOK {
		return 0, nil, false
	}
	if b.IsDelisted("bitkub", p) {
		return reply(200, map[string]string{"s": "no_data"})
	}
	now := b.Now()
	if _, ok := b.Price("bitkub", p, now); !ok {
		return 0, nil, false
	}
	from, to := now.Add(-24*time.Hour), now
	if s, err := strconv.ParseInt(q.Get("from"), 10, 64); err == nil {
		from = time.Unix(s, 0).UTC()
	}
	if s, err := strconv.ParseInt(q.Get("to"), 10, 64); err == nil && time.Unix(s, 0).Before(now) {
		to = time.Unix(s, 0).UTC()
	}
	bars := b.Bars("bitkub", p, tf, from, to)
	if len(bars) > maxBitkubBars {
		bars = bars[len(bars)-maxBitkubBars:]
	}
	if len(bars) == 0 {
		return reply(200, map[string]string{"s": "no_data"})
	}
	out := struct {
		S string    `json:"s"`
		T []int64   `json:"t"`
		O []float64 `json:"o"`
		H []float64 `json:"h"`
		L []float64 `json:"l"`
		C []float64 `json:"c"`
		V []float64 `json:"v"`
	}{S: "ok"}
	for _, bar := range bars {
		out.T = append(out.T, bar.Open.Unix())
		out.O, out.H, out.L, out.C, out.V = append(out.O, bar.O), append(out.H, bar.H), append(out.L, bar.L), append(out.C, bar.C), append(out.V, bar.V)
	}
	return reply(200, out)
}
