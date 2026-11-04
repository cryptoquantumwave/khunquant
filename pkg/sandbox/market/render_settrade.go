package market

import (
	"net/url"
	"strings"
)

// settradeQuote serves GET /api/marketdata/v3/<broker>/quote/<symbol> (quoteResponse of the
// Settrade client, flat) from the book: the last price and the 24 h statistics. A symbol the
// book does not model falls through, so the recorded fixture or the router's 404 answers.
func (b *Book) settradeQuote(path string, _ url.Values) (int, []byte, bool) {
	rest, ok := strings.CutPrefix(path, "/api/marketdata/v3/")
	broker, tail, found := strings.Cut(rest, "/quote/")
	if !ok || !found || broker == "" || tail == "" || strings.Contains(tail, "/") {
		return 0, nil, false
	}
	p, _, ok := ParseWire("settrade", tail)
	if !ok || b.IsDelisted("settrade", p) {
		return 0, nil, false
	}
	s, ok := b.Stats24h("settrade", p)
	if !ok {
		return 0, nil, false
	}
	pct := 0.0
	if s.Open > 0 {
		pct = (s.Last - s.Open) / s.Open * 100
	}
	return reply(200, map[string]any{"symbol": tail, "last": s.Last, "high": s.High, "low": s.Low,
		"change": s.Last - s.Open, "percentChange": pct, "totalVolume": 1})
}
