package market

import (
	"net/url"
	"strings"
)

// webullSnapshot serves GET /openapi/market-data/stock/snapshot (a JSON array of Snapshot,
// every number a string) from the book, for the symbols it models, whatever the category.
// With none of them modelled it falls through, so the recorded fixture answers as before;
// a symbol the book does not model is left out of a mixed answer, as the venue omits one it
// does not know (the client then asks again under US_ETF).
func (b *Book) webullSnapshot(q url.Values) (int, []byte, bool) {
	rows := []map[string]any{}
	for _, sym := range strings.Split(q.Get("symbols"), ",") {
		p, _, ok := ParseWire("webull", sym)
		if !ok || b.IsDelisted("webull", p) {
			continue
		}
		qt, ok := b.quote("webull", p)
		if !ok {
			continue
		}
		pct := 0.0
		if qt.Open > 0 {
			pct = (qt.Last - qt.Open) / qt.Open
		}
		rows = append(rows, map[string]any{
			"symbol": sym, "instrument_id": "sbx-" + sym, "price": num(qt.Last), "pre_close": num(qt.Open), "open": num(qt.Open),
			"high": num(qt.High), "low": num(qt.Low), "close": num(qt.Last), "volume": "1", "change": num(qt.Last - qt.Open),
			"change_ratio": num(pct), "bid": num(qt.bid), "bid_size": "1", "ask": num(qt.ask), "ask_size": "1",
			"last_trade_time": ms(b.Now()), "quote_time": ms(b.Now()), "list_status": "NORMAL",
		})
	}
	if len(rows) == 0 {
		return 0, nil, false
	}
	return reply(200, rows)
}
