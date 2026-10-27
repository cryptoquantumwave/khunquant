package market

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// Delist takes a pair off a venue ("*": every venue). On the wire, market data and order
// creation for it get the venue's invalid-symbol error, and the listings (exchangeInfo,
// instruments, symbols) lose it. A spot market and its swap share the pair, so both go.
// A client that has already loaded markets keeps the symbol until it reloads them.
func (b *Book) Delist(venue, symbol string) error { return b.setDelisted(venue, symbol, true) }

// Relist undoes Delist for the same venue.
func (b *Book) Relist(venue, symbol string) error { return b.setDelisted(venue, symbol, false) }

func (b *Book) setDelisted(venue, symbol string, on bool) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	p, err := ParseSymbol(venue, symbol)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !on {
		delete(b.delisted[venue], p)
		return nil
	}
	if b.delisted[venue] == nil {
		b.delisted[venue] = map[Pair]bool{}
	}
	b.delisted[venue][p] = true
	return nil
}

// IsDelisted reports whether p is off venue.
func (b *Book) IsDelisted(venue string, p Pair) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.delistedLocked(venue, p)
}

func (b *Book) delistedLocked(venue string, p Pair) bool {
	return b.delisted[venue][p] || b.delisted[Wildcard][p]
}

// DelistedEntry is one delisted pair of a venue.
type DelistedEntry struct {
	Venue  string `json:"venue"`
	Symbol string `json:"symbol"`
}

// Delisted lists the delisted pairs of the venues (all when empty), sorted.
func (b *Book) Delisted(venue string) []DelistedEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := []DelistedEntry{}
	for v, ps := range b.delisted {
		for p := range ps {
			if venue == "" || v == venue {
				out = append(out, DelistedEntry{v, p.String()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Venue+out[i].Symbol < out[j].Venue+out[j].Symbol })
	return out
}

// invalidSymbol is the venue's answer for a delisted symbol.
func invalidSymbol(venue string) (int, []byte, bool) {
	switch venue {
	case "binance":
		return reply(400, map[string]any{"code": -1121, "msg": "Invalid symbol."})
	case "okx":
		return reply(200, map[string]any{"code": "51001", "msg": "Instrument ID, Instrument ID code, or Spread ID doesn't exist.", "data": []any{}})
	}
	return 0, nil, false
}

// requestedWires are the symbols a market-data request names (none for a request for all).
func requestedWires(venue, path string, q url.Values) []string {
	switch venue + path {
	case "binance/api/v3/ticker/24hr", "binance/fapi/v1/ticker/24hr", "binance/api/v3/klines", "binance/fapi/v1/klines":
		var many []string
		if s := q.Get("symbols"); s != "" && json.Unmarshal([]byte(s), &many) != nil {
			return nil
		}
		return append(many, q.Get("symbol"))
	case "binance/fapi/v1/premiumIndex":
		return []string{q.Get("symbol")}
	case "okx/api/v5/market/ticker", "okx/api/v5/market/candles", "okx/api/v5/market/history-candles",
		"okx/api/v5/public/mark-price", "okx/api/v5/public/funding-rate":
		return []string{q.Get("instId")}
	}
	return nil
}

// delistedReply answers a market-data request that names a delisted symbol.
func (b *Book) delistedReply(venue, path string, q url.Values) (int, []byte, bool) {
	for _, w := range requestedWires(venue, path, q) {
		if p, _, ok := ParseWire(venue, w); ok && b.IsDelisted(venue, p) {
			return invalidSymbol(venue)
		}
	}
	return 0, nil, false
}

// refusesOrder reports whether an order-create request is for a delisted symbol. It reads
// the body and puts it back, so the orders stage still sees it.
func (b *Book) refusesOrder(venue, path string, r *http.Request) bool {
	var wires []string
	switch {
	case venue == "binance" && (strings.HasPrefix(path, "/api/v3/order") || strings.HasPrefix(path, "/fapi/v1/order") || strings.HasPrefix(path, "/fapi/v3/order")):
		body, ok := peekBody(r)
		if !ok {
			return false
		}
		form, _ := url.ParseQuery(string(body))
		wires = []string{r.URL.Query().Get("symbol"), form.Get("symbol")}
	case venue == "okx" && (strings.HasPrefix(path, "/api/v5/trade/order") || strings.HasPrefix(path, "/api/v5/trade/batch-orders")):
		body, ok := peekBody(r)
		if !ok {
			return false
		}
		var one struct {
			InstID string `json:"instId"`
		}
		var many []struct {
			InstID string `json:"instId"`
		}
		if json.Unmarshal(body, &one) == nil {
			wires = append(wires, one.InstID)
		} else if json.Unmarshal(body, &many) == nil {
			for _, o := range many {
				wires = append(wires, o.InstID)
			}
		}
	default:
		return false
	}
	for _, w := range wires {
		if p, _, ok := ParseWire(venue, w); ok && b.IsDelisted(venue, p) {
			return true
		}
	}
	return false
}

const maxOrderBody = 1 << 20

func peekBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil {
		return nil, true
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOrderBody))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	return body, err == nil
}

// listings are the routes that enumerate a venue's markets: the JSON array to filter and
// the field of each row that names its symbol.
var listings = map[string]struct{ list, field string }{
	"binance/api/v3/exchangeInfo":   {"symbols", "symbol"},
	"binance/fapi/v1/exchangeInfo":  {"symbols", "symbol"},
	"binance/fapi/v3/exchangeInfo":  {"symbols", "symbol"},
	"okx/api/v5/public/instruments": {"data", "instId"},
	"bitkub/api/v3/market/symbols":  {"result", "symbol"},
}

// filterTickerArray drops delisted symbols from a plain-array Bitkub ticker next produced.
func (b *Book) filterTickerArray(resp *sandbox.Response) *sandbox.Response {
	var rows []json.RawMessage
	if resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &rows) != nil {
		return resp
	}
	kept := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var f struct {
			Symbol string `json:"symbol"`
		}
		if json.Unmarshal(row, &f) == nil && b.symDelisted("bitkub", f.Symbol) {
			continue
		}
		kept = append(kept, row)
	}
	body, err := json.Marshal(kept)
	if err != nil {
		return resp
	}
	out := *resp
	out.Body = body
	return &out
}

// filterListing drops delisted symbols from a listing next produced and, for OKX, the rows
// whose instType is not the one asked for (the fixture lists every type). Anything it
// cannot read is returned unchanged.
func (b *Book) filterListing(venue, path string, q url.Values, resp *sandbox.Response) *sandbox.Response {
	ls := listings[venue+path]
	wantType := q.Get("instType")
	var doc map[string]json.RawMessage
	var rows []json.RawMessage
	if resp.Status != http.StatusOK || json.Unmarshal(resp.Body, &doc) != nil || json.Unmarshal(doc[ls.list], &rows) != nil {
		return resp
	}
	kept := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		var f map[string]any
		if json.Unmarshal(row, &f) == nil {
			sym, _ := f[ls.field].(string)
			typ, _ := f["instType"].(string)
			if venue == "okx" && wantType != "" && typ != "" && typ != wantType {
				continue
			}
			if p, _, ok := ParseWire(venue, sym); ok && b.IsDelisted(venue, p) {
				continue
			}
		}
		kept = append(kept, row)
	}
	list, err := json.Marshal(kept)
	if err != nil {
		return resp
	}
	doc[ls.list] = list
	body, err := json.Marshal(doc)
	if err != nil {
		return resp
	}
	out := *resp
	out.Body = body
	return &out
}
