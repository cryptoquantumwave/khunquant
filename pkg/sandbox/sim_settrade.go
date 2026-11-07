package sandbox

import (
	"encoding/json"
	"io"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Settrade (SEOS v3) equity orders over the order ledger: board and odd lots, the at-the-open
// (ATO) queue, Day validity, and the portfolio and cash the orders move. Volumes are shares.
// The venue is simulated only in a lab (EnableEquityVenues); a gateway with no lab serves its
// fixtures as before. The error codes are the simulator's own (SBX-), not Settrade's.

const settradeBoardLot = 100 // setBoardLotSize of the adapter

// EnableEquityVenues makes the simulator answer the Settrade routes. Only the lab calls it.
func (sim *StatefulSimulator) EnableEquityVenues() {
	sim.mu.Lock()
	sim.equities = true
	sim.mu.Unlock()
}

// settradePath matches the account routes this file answers: accounts/<no>/<what>[/<order>[/cancel]].
var settradePath = regexp.MustCompile(`^/api/seos/v3/[^/]+/accounts/[^/]+/(orders|portfolios|account-info)(?:/([^/]+)(/cancel)?)?$`)

// settradeOrderPath reports whether path is an order route: owned by the simulator even
// before anything is seeded, unlike the portfolio and cash routes.
func settradeOrderPath(path string) bool {
	m := settradePath.FindStringSubmatch(path)
	return m != nil && m[1] == "orders"
}

func settradeErr(status int, code, msg string) *Response {
	return jsonResponse(status, map[string]string{"code": code, "message": msg})
}

// settradeLive says whether the venue's account is the simulator's: seeded with cash or
// holdings, or traded. Before that the recorded fixture answers the cash and portfolio routes.
func (sim *StatefulSimulator) settradeLive() bool {
	st := sim.stateManager.GetState("settrade")
	_, cash := st.Balances["THB"]
	return cash || len(st.OpenOrders)+len(st.ClosedOrders) > 0
}

func (sim *StatefulSimulator) respondSettrade(method, path string, r *http.Request) (*Response, bool) {
	m := settradePath.FindStringSubmatch(path)
	if m == nil {
		return nil, false
	}
	what, orderNo, cancel := m[1], m[2], m[3] != ""
	switch {
	case what == "orders" && orderNo == "" && method == http.MethodPost:
		return sim.settradeCreate(r), true
	case what == "orders" && orderNo == "" && method == http.MethodGet:
		return sim.settradeList(r), true
	case what == "orders" && orderNo != "" && !cancel && method == http.MethodGet:
		return sim.settradeFetch(orderNo), true
	case what == "orders" && orderNo != "" && cancel && method == http.MethodPatch:
		return sim.settradeCancel(orderNo), true
	case what == "account-info" && method == http.MethodGet && sim.settradeLive():
		return sim.settradeAccountInfo(), true
	case what == "portfolios" && method == http.MethodGet && sim.settradeLive():
		return sim.settradePortfolio(), true
	}
	return nil, false
}

// settradeOrderReq is the order body the client sends. A number is a pointer so a missing
// field is told from zero.
type settradeOrderReq struct {
	Side         string   `json:"side"`
	Symbol       string   `json:"symbol"`
	Volume       *float64 `json:"volume"`
	Price        *float64 `json:"price"`
	PriceType    string   `json:"priceType"`
	ValidityType string   `json:"validityType"`
}

var settradeSymbol = regexp.MustCompile(`^[A-Z0-9&.]{1,20}$`)

func (sim *StatefulSimulator) settradeCreate(r *http.Request) *Response {
	var req settradeOrderReq
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)); err != nil || json.Unmarshal(body, &req) != nil {
		return settradeErr(400, "SBX-BAD-REQUEST", "the order body is not valid JSON")
	}
	side := strings.ToLower(req.Side)
	switch {
	case side != "buy" && side != "sell":
		return settradeErr(400, "SBX-BAD-SIDE", "side must be Buy or Sell")
	case !settradeSymbol.MatchString(req.Symbol):
		return settradeErr(400, "SBX-BAD-SYMBOL", "symbol is missing or malformed")
	case req.Volume == nil || !finite(*req.Volume) || *req.Volume <= 0 || *req.Volume != math.Trunc(*req.Volume) || *req.Volume > 1e9:
		return settradeErr(400, "SBX-BAD-VOLUME", "volume must be a positive whole number of shares")
	case req.Price != nil && (!finite(*req.Price) || *req.Price < 0):
		return settradeErr(400, "SBX-BAD-PRICE", "price must be a finite number, not negative")
	case req.ValidityType != "" && req.ValidityType != "Day":
		return settradeErr(400, "SBX-VALIDITY", "only Day orders are simulated")
	}
	vol, atOpen := *req.Volume, false
	switch req.PriceType {
	case "Limit":
		if req.Price == nil || *req.Price <= 0 {
			return settradeErr(400, "SBX-BAD-PRICE", "a Limit order needs a positive price")
		}
	case "ATO", "ATC":
		if vol < settradeBoardLot {
			return settradeErr(400, "SBX-ODD-LOT-PRICETYPE", "odd lot must be Limit")
		}
		if req.PriceType == "ATC" {
			return settradeErr(400, "SBX-ATC-UNSUPPORTED", "at-the-close matching is not simulated")
		}
		atOpen = true
	default:
		return settradeErr(400, "SBX-PRICE-TYPE", "priceType must be Limit or ATO")
	}
	if vol >= settradeBoardLot && math.Mod(vol, settradeBoardLot) != 0 {
		return settradeErr(400, "SBX-BOARD-LOT", "a board lot order is a multiple of 100 shares")
	}

	now := sim.clock()
	day := sim.setDay()
	sp := orderSpec{Market: "spot", Symbol: req.Symbol, Side: side, Type: "limit", Amount: vol, CancelAt: dayEnd(now)}
	if atOpen {
		at, ok := day.nextAuction(now)
		if !ok {
			return settradeErr(400, "SBX-ATO-SESSION", "ATO is valid only into an opening auction; the last one of the day has run")
		}
		sp.Type, sp.AuctionAt, sp.ReserveUp = "market", at, 0.10 // a buy holds the last price + 10 % until the auction prices it
	} else {
		sp.Price = *req.Price
	}
	o, reported, rj := sim.book("settrade").place(sp)
	if rj != nil {
		return settradeReject(rj, side)
	}
	o.Params["priceType"] = map[bool]string{true: "ATO", false: "Limit"}[atOpen]
	wire := settradeOrderWire(o)
	if reported != nil { // create_reports_fill: filled, and the answer says it was not
		wire["matched"], wire["balance"], wire["status"] = *reported, o.Amount-*reported, "S"
	}
	return jsonResponse(200, map[string]any{"data": wire})
}

func settradeReject(rj *reject, side string) *Response {
	switch {
	case rj.kind == "funds" && side == "sell":
		return settradeErr(400, "SBX-INSUFFICIENT-HOLDINGS", "not enough shares to sell")
	case rj.kind == "funds":
		return settradeErr(400, "SBX-INSUFFICIENT-FUNDS", "not enough cash for this order")
	}
	return settradeErr(400, "SBX-UNKNOWN-SYMBOL", strings.TrimSpace("no such symbol "+rj.detail))
}

// settradeOrderWire is settradeOrder of pkg/exchanges/settrade: vol, matched and balance in
// shares, status S (queued or open), P (partly matched), M (matched) or C (cancelled).
func settradeOrderWire(o *Order) map[string]any {
	pt, _ := o.Params["priceType"].(string)
	if pt == "" {
		pt = map[bool]string{true: "ATO", false: "Limit"}[o.OrderType == "market"]
	}
	price, balance := o.Price, o.Amount-o.Filled
	if o.OrderType == "market" && o.Filled > 0 {
		price = o.Average
	}
	if !stateOf("settrade", o.Status).open() {
		balance = 0
	}
	return map[string]any{
		"orderNo": o.ID, "symbol": o.Symbol, "side": strings.ToUpper(o.Side[:1]) + o.Side[1:], "priceType": pt,
		"vol": o.Amount, "matched": o.Filled, "balance": balance, "price": price, "status": o.Status,
		"entryTime": o.CreatedAt.In(bangkok).Format(time.RFC3339),
	}
}

func (sim *StatefulSimulator) settradeFetch(no string) *Response {
	o := sim.book("settrade").find(no, "", "")
	if o == nil || !o.visible(sim.clock()) {
		return settradeErr(404, "SBX-ORDER-NOT-FOUND", "no such order")
	}
	return jsonResponse(200, map[string]any{"data": settradeOrderWire(o)})
}

func (sim *StatefulSimulator) settradeCancel(no string) *Response {
	b := sim.book("settrade")
	o := b.find(no, "", "")
	switch {
	case o == nil || !o.visible(sim.clock()):
		return settradeErr(404, "SBX-ORDER-NOT-FOUND", "no such order")
	case !stateOf("settrade", o.Status).open():
		return settradeErr(400, "SBX-NOT-CANCELLABLE", "the order is not open")
	}
	b.close(o, stCanceled, sim.clock())
	sim.reschedule(sim.clock())
	return jsonResponse(200, map[string]any{"data": settradeOrderWire(o)})
}

// settradeList answers GET orders: the open orders, or with status=matched the matched ones
// (newest limit of them), for one symbol or all.
func (sim *StatefulSimulator) settradeList(r *http.Request) *Response {
	q, b, now := r.URL.Query(), sim.book("settrade"), sim.clock()
	matched := q.Get("status") == "matched"
	var list []*Order
	for _, o := range b.list(matched, q.Get("symbol"), now) {
		if !matched || stateOf("settrade", o.Status) == stFilled {
			list = append(list, o)
		}
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && len(list) > n {
		list = list[len(list)-n:]
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, settradeOrderWire(o))
	}
	return jsonResponse(200, out)
}

// settradeAccountInfo is accountInfoResponse: the cash held (reserved or not) and what is
// free to buy with.
func (sim *StatefulSimulator) settradeAccountInfo() *Response {
	cash := sim.stateManager.GetState("settrade").Balances["THB"]
	return jsonResponse(200, map[string]any{"cashBalance": cash.Free + cash.Locked, "lineAvailable": cash.Free,
		"creditLimit": 0, "canBuy": true, "canSell": true})
}

// settradePortfolio is portfolioResponse: each held symbol, its volume (total, and what is
// free to sell), the average cost of the buys on record (the market price for a seeded
// holding) and the profit at the market price.
func (sim *StatefulSimulator) settradePortfolio() *Response {
	st := sim.stateManager.GetState("settrade")
	cost, vol := map[string]float64{}, map[string]float64{}
	for _, t := range st.Trades {
		if t.Side == "buy" {
			cost[t.Symbol], vol[t.Symbol] = cost[t.Symbol]+t.Price*t.Amount, vol[t.Symbol]+t.Amount
		}
	}
	items := []map[string]any{}
	for _, sym := range slices.Sorted(maps.Keys(st.Balances)) {
		held := st.Balances[sym]
		if sym == "THB" || held.Free+held.Locked <= 0 || !settradeSymbol.MatchString(sym) {
			continue
		}
		mark := sim.markPrice("settrade", sym, st)
		avg := mark
		if vol[sym] > 0 {
			avg = cost[sym] / vol[sym]
		}
		total := held.Free + held.Locked
		pct := 0.0
		if avg > 0 {
			pct = (mark - avg) / avg * 100
		}
		items = append(items, map[string]any{"symbol": sym, "currentVolume": total, "actualVolume": held.Free, "averagePrice": avg,
			"marketPrice": mark, "marketValue": mark * total, "profit": (mark - avg) * total, "percentProfit": pct,
			"amount": avg * total, "commissionRate": 0, "flag": "N", "liabilities": 0, "marginRate": 0})
	}
	return jsonResponse(200, map[string]any{"portfolioList": items})
}
