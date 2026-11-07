package sandbox

import (
	"encoding/json"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Webull OpenAPI equity orders over the order ledger. An order is keyed by its
// client_order_id, which the venue may or may not refuse to see twice (an option: the real
// behaviour is not established), and its listings return a page of 10 by default in an order
// that is not time's: a permutation seeded by (list_seed, how many first pages were asked
// before), so the same scenario sees the same pages. Orders fill at any sim time (no session
// rules). A lab only (EnableEquityVenues); every error code is the simulator's own (SBX-).

const (
	webullDefaultPage = 10
	webullMaxPage     = 100
)

func webullErr(status int, code, msg string) *Response {
	return jsonResponse(status, map[string]string{"message": msg, "error_code": code})
}

var (
	webullClientID = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
	webullSymbol   = regexp.MustCompile(`^[A-Z0-9.]{1,10}$`)
)

// webullOrderPath says whether path is an order route: owned by the simulator even before
// anything is seeded, unlike the balance and positions routes.
func webullOrderPath(method, path string) bool {
	switch path {
	case "/openapi/trade/order/place", "/openapi/trade/order/cancel":
		return method == http.MethodPost
	case "/openapi/trade/order/open", "/openapi/trade/order/history", "/openapi/trade/order/detail":
		return method == http.MethodGet
	}
	return false
}

// webullLive says whether the account is the simulator's: seeded with cash or holdings, or
// traded. Before that the recorded fixtures answer the balance and positions routes.
func (sim *StatefulSimulator) webullLive() bool {
	st := sim.stateManager.GetState("webull")
	_, cash := st.Balances["USD"]
	return cash || len(st.OpenOrders)+len(st.ClosedOrders) > 0
}

func (sim *StatefulSimulator) respondWebull(method, path string, r *http.Request) (*Response, bool) {
	switch {
	case webullOrderPath(method, path):
		switch path {
		case "/openapi/trade/order/place":
			return sim.webullPlace(r), true
		case "/openapi/trade/order/cancel":
			return sim.webullCancel(r), true
		case "/openapi/trade/order/detail":
			return sim.webullDetail(r), true
		}
		return sim.webullList(r, path == "/openapi/trade/order/history"), true
	case method == http.MethodGet && path == "/openapi/assets/balance" && sim.webullLive():
		return sim.webullBalance(), true
	case method == http.MethodGet && path == "/openapi/assets/positions" && sim.webullLive():
		return sim.webullPositions(), true
	}
	return nil, false
}

// webullNewOrder is the part of a NewOrder the ledger reads; the numbers arrive as strings.
type webullNewOrder struct {
	ClientOrderID  string `json:"client_order_id"`
	InstrumentType string `json:"instrument_type"`
	OrderType      string `json:"order_type"`
	Side           string `json:"side"`
	Symbol         string `json:"symbol"`
	TimeInForce    string `json:"time_in_force"`
	Quantity       string `json:"quantity"`
	LimitPrice     string `json:"limit_price"`
}

func (sim *StatefulSimulator) webullPlace(r *http.Request) *Response {
	var req struct {
		AccountID string           `json:"account_id"`
		NewOrders []webullNewOrder `json:"new_orders"`
	}
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err != nil || json.Unmarshal(body, &req) != nil || req.AccountID == "" || len(req.NewOrders) != 1 {
		return webullErr(400, "SBX-BAD-REQUEST", "want an account_id and exactly one order in new_orders")
	}
	n, side := req.NewOrders[0], strings.ToLower(req.NewOrders[0].Side)
	qty, okQ := numArg(n.Quantity)
	switch {
	case n.InstrumentType != "EQUITY":
		return webullErr(400, "SBX-UNSUPPORTED", "only EQUITY orders are simulated")
	case !webullClientID.MatchString(n.ClientOrderID):
		return webullErr(400, "SBX-BAD-CLIENT-ID", "client_order_id is 1 to 32 letters and digits")
	case side != "buy" && side != "sell":
		return webullErr(400, "SBX-BAD-SIDE", "side must be BUY or SELL")
	case !webullSymbol.MatchString(n.Symbol):
		return webullErr(400, "SBX-BAD-SYMBOL", "symbol is missing or malformed")
	case n.OrderType != "MARKET" && n.OrderType != "LIMIT":
		return webullErr(400, "SBX-ORDER-TYPE", "only MARKET and LIMIT orders are simulated")
	case !okQ || qty <= 0:
		return webullErr(400, "SBX-BAD-QUANTITY", "quantity must be a positive number")
	case n.TimeInForce != "DAY" && n.TimeInForce != "GTC":
		return webullErr(400, "SBX-TIME-IN-FORCE", "time_in_force must be DAY or GTC")
	}
	sp := orderSpec{Market: "spot", Symbol: n.Symbol, Side: side, Type: "market", Amount: qty, ClientID: n.ClientOrderID, DupClientOK: true}
	if n.OrderType == "LIMIT" {
		px, ok := numArg(n.LimitPrice)
		if !ok || px <= 0 {
			return webullErr(400, "SBX-BAD-PRICE", "a LIMIT order needs a positive limit_price")
		}
		if qty != float64(int64(qty)) {
			return webullErr(400, "SBX-FRACTIONAL-LIMIT", "fractional shares are not accepted on a LIMIT order")
		}
		sp.Type, sp.Price = "limit", px
	}
	b := sim.book("webull")
	if sim.options["webull"].RejectDuplicateClientID && b.find("", n.ClientOrderID, "") != nil {
		return webullErr(400, "SBX-DUP-CLIENT-ID", "client_order_id was used before")
	}
	o, _, rj := b.place(sp)
	if rj != nil {
		code, msg := "SBX-UNKNOWN-SYMBOL", strings.TrimSpace("no such symbol "+rj.detail)
		if rj.kind == "funds" {
			code, msg = "SBX-INSUFFICIENT-FUNDS", "not enough cash for this order"
			if side == "sell" {
				code, msg = "SBX-INSUFFICIENT-HOLDINGS", "not enough shares to sell"
			}
		}
		return webullErr(400, code, msg)
	}
	return jsonResponse(200, map[string]string{"client_order_id": o.ClientID, "order_id": o.ID})
}

// webullQuery is the query of a GET as a map, and whether it carries an account_id.
func webullQuery(r *http.Request) (q map[string]string, ok bool) {
	q = map[string]string{}
	for k, v := range r.URL.Query() {
		q[k] = v[0]
	}
	return q, q["account_id"] != ""
}

func (sim *StatefulSimulator) webullCancel(r *http.Request) *Response {
	var req struct {
		AccountID     string `json:"account_id"`
		ClientOrderID string `json:"client_order_id"`
	}
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err != nil || json.Unmarshal(body, &req) != nil || req.AccountID == "" || req.ClientOrderID == "" {
		return webullErr(400, "SBX-BAD-REQUEST", "want an account_id and a client_order_id")
	}
	b := sim.book("webull")
	o := b.st.openByClient(req.ClientOrderID)
	if o == nil {
		if b.find("", req.ClientOrderID, "") != nil {
			return webullErr(400, "SBX-NOT-CANCELLABLE", "the order is not open")
		}
		return webullErr(404, "SBX-NOT-FOUND", "order not found")
	}
	b.close(o, stCanceled, sim.clock())
	sim.reschedule(sim.clock())
	return jsonResponse(200, map[string]string{"client_order_id": o.ClientID, "order_id": o.ID})
}

// webullItem is OrderItem: money and quantities are strings, the status the venue's.
func webullItem(o *Order) map[string]string {
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	num := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	item := map[string]string{
		"client_order_id": o.ClientID, "order_id": o.ID, "symbol": o.Symbol, "side": strings.ToUpper(o.Side), "status": o.Status,
		"order_type": strings.ToUpper(o.OrderType), "instrument_type": "EQUITY", "entrust_type": "QTY", "time_in_force": "DAY",
		"total_quantity": num(o.Amount), "filled_quantity": num(o.Filled), "limit_price": "", "stop_price": "", "filled_price": "",
		"place_time": ms(o.CreatedAt), "place_time_at": o.CreatedAt.UTC().Format(time.RFC3339),
	}
	if o.Price > 0 {
		item["limit_price"] = num(o.Price)
	}
	if o.Filled > 0 {
		item["filled_price"] = num(o.Average)
		item["filled_time_at"] = o.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return item
}

func webullCombo(o *Order) map[string]any {
	return map[string]any{"combo_type": "NORMAL", "combo_order_id": o.ID, "client_order_id": o.ClientID, "orders": []map[string]string{webullItem(o)}}
}

func (sim *StatefulSimulator) webullDetail(r *http.Request) *Response {
	q, ok := webullQuery(r)
	if !ok || q["client_order_id"] == "" {
		return webullErr(400, "SBX-BAD-REQUEST", "want an account_id and a client_order_id")
	}
	o := sim.book("webull").find("", q["client_order_id"], "")
	if o == nil || !o.visible(sim.clock()) {
		return webullErr(404, "SBX-NOT-FOUND", "order not found")
	}
	return jsonResponse(200, webullCombo(o))
}

// webullList answers the open and history listings. The matching set (the open orders, or
// the closed ones inside the date window) is put in the permutation of its kind and cut
// to page_size after last_client_order_id. A first page (no cursor) takes the next
// permutation; a page with a cursor continues the one the last first page used.
func (sim *StatefulSimulator) webullList(r *http.Request, history bool) *Response {
	q, ok := webullQuery(r)
	if !ok {
		return webullErr(400, "SBX-BAD-REQUEST", "want an account_id")
	}
	size := webullDefaultPage
	if s, set := q["page_size"]; set {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > webullMaxPage {
			return webullErr(400, "SBX-PAGE-SIZE", "page_size is a whole number from 1 to 100")
		}
		size = n
	}
	now := sim.clock()
	from, to := now.AddDate(0, 0, -7), now
	if history {
		var err1, err2 error
		if s := q["start_date"]; s != "" {
			from, err1 = time.Parse("2006-01-02", s)
		}
		if s := q["end_date"]; s != "" {
			var d time.Time
			d, err2 = time.Parse("2006-01-02", s)
			to = d.AddDate(0, 0, 1).Add(-time.Nanosecond)
		}
		if err1 != nil || err2 != nil {
			return webullErr(400, "SBX-BAD-DATE", "start_date and end_date are yyyy-MM-dd")
		}
	}
	var set []*Order
	for _, o := range sim.book("webull").list(history, "", now) {
		if !history || (!o.UpdatedAt.Before(from) && !o.UpdatedAt.After(to)) {
			set = append(set, o)
		}
	}
	page := sim.webullPage(map[bool]string{true: "history", false: "open"}[history], set, size, q["last_client_order_id"])
	out := make([]map[string]any, 0, len(page))
	for _, o := range page {
		out = append(out, webullCombo(o))
	}
	return jsonResponse(200, out)
}

// webullPage orders set by the permutation seeded by (list_seed, the kind's first-page
// count), and returns size orders after the cursor (none when the cursor names no order).
func (sim *StatefulSimulator) webullPage(kind string, set []*Order, size int, after string) []*Order {
	st := sim.stateManager.GetState("webull")
	if st.ListSeq == nil {
		st.ListSeq = map[string]int64{}
	}
	n := st.ListSeq[kind]
	if after == "" {
		st.ListSeq[kind] = n + 1
	} else if n > 0 {
		n--
	}
	seed := uint64(1)
	if s := sim.options["webull"].ListSeed; s != nil {
		seed = *s
	}
	slices.SortFunc(set, func(a, b *Order) int { return strings.Compare(a.ClientID+"/"+a.ID, b.ClientID+"/"+b.ID) })
	rand.New(rand.NewPCG(seed, uint64(n))).Shuffle(len(set), func(i, j int) { set[i], set[j] = set[j], set[i] })
	start := 0
	if after != "" {
		if start = slices.IndexFunc(set, func(o *Order) bool { return o.ClientID == after }); start < 0 {
			return nil
		}
		start++
	}
	return set[start:min(start+size, len(set))]
}

// webullHolding is one held symbol: its quantity, average cost and the market price.
type webullHolding struct {
	symbol         string
	qty, avg, mark float64
}

func (sim *StatefulSimulator) webullHoldings() []webullHolding {
	st := sim.stateManager.GetState("webull")
	cost, vol := avgCost(st)
	var out []webullHolding
	for _, sym := range slices.Sorted(maps.Keys(st.Balances)) {
		b := st.Balances[sym]
		if sym == "USD" || b.Free+b.Locked <= 0 || !webullSymbol.MatchString(sym) {
			continue
		}
		mark := sim.markPrice("webull", sym, st)
		avg := mark
		if vol[sym] > 0 {
			avg = cost[sym] / vol[sym]
		}
		out = append(out, webullHolding{sym, b.Free + b.Locked, avg, mark})
	}
	return out
}

func (sim *StatefulSimulator) webullBalance() *Response {
	num := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	cash := sim.stateManager.GetState("webull").Balances["USD"]
	total := cash.Free + cash.Locked
	var mv, pl float64
	for _, h := range sim.webullHoldings() {
		mv, pl = mv+h.qty*h.mark, pl+(h.mark-h.avg)*h.qty
	}
	return jsonResponse(200, map[string]any{
		"total_asset_currency": "USD", "total_net_liquidation_value": num(total + mv), "total_market_value": num(mv),
		"total_cash_balance": num(total), "total_unrealized_profit_loss": num(pl), "total_day_profit_loss": "0",
		"account_currency_assets": []map[string]string{{"currency": "USD", "cash_balance": num(total), "settled_cash": num(total),
			"unsettled_cash": "0", "market_value": num(mv), "buying_power": num(cash.Free), "unrealized_profit_loss": num(pl),
			"net_liquidation_value": num(total + mv), "day_profit_loss": "0"}},
	})
}

func (sim *StatefulSimulator) webullPositions() *Response {
	num := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	out := []map[string]string{}
	for _, h := range sim.webullHoldings() {
		pct := 0.0
		if h.avg > 0 {
			pct = (h.mark - h.avg) / h.avg
		}
		out = append(out, map[string]string{"currency": "USD", "quantity": num(h.qty), "cost": num(h.avg * h.qty), "cost_price": num(h.avg),
			"position_id": "sbx-" + h.symbol, "symbol": h.symbol, "instrument_type": "EQUITY", "last_price": num(h.mark),
			"market_value": num(h.mark * h.qty), "unrealized_profit_loss": num((h.mark - h.avg) * h.qty),
			"unrealized_profit_loss_rate": num(pct), "day_profit_loss": "0", "day_realized_profit_loss": "0"})
	}
	return jsonResponse(200, out)
}
