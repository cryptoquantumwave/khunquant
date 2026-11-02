package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The venue-facing half of the order ledger: Binance and OKX order endpoints parsed into
// orderSpecs and answered from the ledger's orders in each venue's wire format.

// fapiEndpoint is the endpoint name of a Binance futures path (/fapi/v1|v3/<ep>), or "".
// The spot API (/api/v3/) is not the futures wallet's and is not routed here.
func fapiEndpoint(path string) string {
	for _, p := range [...]string{"/fapi/v1/", "/fapi/v3/"} {
		if ep, ok := strings.CutPrefix(path, p); ok {
			return ep
		}
	}
	return ""
}

// numArg reads a request number: finite and not negative, else not ok (NaN and Inf parse).
func numArg(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && finite(f) && f >= 0
}

func jsonResponse(status int, v any) *Response {
	body, _ := json.Marshal(v)
	return &Response{Status: status, Body: body, Headers: map[string]string{"Content-Type": "application/json"}}
}

// binanceReject and okxReject render a ledger refusal as the venue writes it.
func (sim *StatefulSimulator) binanceReject(rj *reject) *Response {
	r := rejects[rj.kind]
	msg := r.bnMsg
	if rj.detail != "" {
		msg += " " + rj.detail
	}
	return sim.binanceErrorResponse(400, r.bn, msg)
}

func (sim *StatefulSimulator) okxReject(rj *reject, clOrdID string) *Response {
	r := rejects[rj.kind]
	msg := r.om
	if rj.detail != "" {
		msg += ": " + rj.detail
	}
	return jsonResponse(200, map[string]any{"code": "1", "msg": "", "data": []map[string]any{
		{"clOrdId": clOrdID, "ordId": "", "tag": "", "sCode": r.ok, "sMsg": msg}}})
}

// conditional says whether a Binance order type needs a trigger, which is not simulated.
func conditional(t string) bool {
	return strings.HasPrefix(t, "stop") || strings.HasPrefix(t, "take_profit") || strings.HasPrefix(t, "trailing") || t == "limit_maker"
}

// simBinanceCreateOrder handles POST /fapi/v1|v3/order (parameters in the query, the body or both).
func (sim *StatefulSimulator) simBinanceCreateOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid request"), true
	}
	f := r.FormValue
	sp := orderSpec{Market: "futures", Symbol: f("symbol"), Side: strings.ToLower(f("side")), Type: strings.ToLower(f("type")),
		ClientID: f("newClientOrderId"), PosSide: strings.ToLower(f("positionSide")), ReduceOnly: strings.EqualFold(f("reduceOnly"), "true")}
	if sp.PosSide == "both" {
		sp.PosSide = ""
	}
	switch {
	case sp.Symbol == "":
		return sim.binanceErrorResponse(400, -1102, "Mandatory parameter 'symbol' was not sent, was empty/null, or malformed."), true
	case sp.Side != "buy" && sp.Side != "sell":
		return sim.binanceErrorResponse(400, -1117, "Invalid side."), true
	case conditional(sp.Type):
		return sim.binanceErrorResponse(400, -4120, "Order type not supported for this endpoint. Please use the Algo Order API endpoints instead."), true
	case sp.Type != "market" && sp.Type != "limit":
		return sim.binanceErrorResponse(400, -1116, "Invalid orderType."), true
	case sp.PosSide != "" && sp.PosSide != "long" && sp.PosSide != "short":
		return sim.binanceReject(&reject{kind: "side"}), true
	}
	var ok bool
	if sp.Amount, ok = numArg(f("quantity")); !ok || sp.Amount <= 0 {
		return sim.binanceErrorResponse(400, -1013, "Invalid quantity."), true
	}
	if sp.Type == "limit" {
		if sp.Price, ok = numArg(f("price")); !ok || sp.Price <= 0 {
			return sim.binanceErrorResponse(400, -1013, "Invalid price."), true
		}
	}
	o, reported, rj := sim.book("binance").place(sp)
	if rj != nil {
		return sim.binanceReject(rj), true
	}
	wire := sim.binanceOrderToWireFormat(o)
	if reported != nil { // create_reports_fill: the order is filled, the answer says otherwise
		wire["executedQty"], wire["cumQuote"], wire["status"], wire["avgPrice"] = formatNumber(*reported), "0", "NEW", "0"
	}
	return jsonResponse(200, wire), true
}

// binanceLookup finds the order a fetch or cancel names, by orderId or origClientOrderId.
func (sim *StatefulSimulator) binanceLookup(b book, r *http.Request) (o *Order, missing bool) {
	id, cid := r.FormValue("orderId"), r.FormValue("origClientOrderId")
	return b.find(id, cid, r.FormValue("symbol")), id == "" && cid == ""
}

const binanceNeedID = "Mandatory parameter 'orderId' or 'origClientOrderId' must be sent."

// simBinanceFetchOrder handles GET /fapi/v1|v3/order.
func (sim *StatefulSimulator) simBinanceFetchOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	o, missing := sim.binanceLookup(sim.book("binance"), r)
	switch {
	case missing:
		return sim.binanceErrorResponse(400, -1102, binanceNeedID), true
	case o == nil || !o.visible(sim.clock()):
		return sim.binanceErrorResponse(400, -2013, "Order does not exist."), true
	}
	return jsonResponse(200, sim.binanceOrderToWireFormat(o)), true
}

// simBinanceCancelOrder handles DELETE /fapi/v1|v3/order.
func (sim *StatefulSimulator) simBinanceCancelOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	b := sim.book("binance")
	o, missing := sim.binanceLookup(b, r)
	switch {
	case missing:
		return sim.binanceErrorResponse(400, -1102, binanceNeedID), true
	case o == nil || !stateOf("binance", o.Status).open():
		return sim.binanceErrorResponse(400, -2011, "Unknown order sent."), true
	}
	b.close(o, stCanceled, sim.clock())
	sim.reschedule(sim.clock())
	return jsonResponse(200, sim.binanceOrderToWireFormat(o)), true
}

// simBinanceListOrders handles GET openOrders, and allOrders (open and closed).
func (sim *StatefulSimulator) simBinanceListOrders(state *VenueState, r *http.Request, all bool) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	b, now, symbol := sim.book("binance"), sim.clock(), r.FormValue("symbol")
	list := b.list(false, symbol, now)
	if all {
		list = append(list, b.list(true, symbol, now)...)
		sortOrders(list)
	}
	out := make([]map[string]any, 0, len(list))
	for _, o := range list {
		out = append(out, sim.binanceOrderToWireFormat(o))
	}
	return jsonResponse(200, out), true
}

// binanceOrderToWireFormat converts an Order to Binance wire format (map).
// CRITICAL: orderId is a JSON NUMBER (not string), status/side/type are uppercase strings,
// all prices/quantities are strings. This format must match exactly for CCXT to parse.
func (sim *StatefulSimulator) binanceOrderToWireFormat(order *Order) map[string]interface{} {
	orderID, _ := strconv.ParseInt(order.ID, 10, 64) // Binance's orderId is a number
	return map[string]interface{}{
		"orderId":       orderID,
		"clientOrderId": order.ClientID,
		"symbol":        order.Symbol,
		"status":        order.Status, // "FILLED", "NEW", "CANCELED", etc.
		"price":         formatNumber(order.Price),
		"origQty":       formatNumber(order.Amount),
		"executedQty":   formatNumber(order.Filled),
		"cumQuote":      formatNumber(order.Cost),
		"avgPrice":      formatNumber(order.Average),
		"positionSide":  positionSideOf(order),
		"side":          strings.ToUpper(order.Side),      // "BUY" or "SELL"
		"type":          strings.ToUpper(order.OrderType), // "MARKET" or "LIMIT"
		"timeInForce":   "GTC",
		"reduceOnly":    order.ReduceOnly,
		"time":          order.CreatedAt.UnixMilli(),
		"updateTime":    order.UpdatedAt.UnixMilli(),
	}
}

// okxOrderReq is the part of an OKX order request the ledger reads. Numbers and booleans
// arrive as strings.
type okxOrderReq struct {
	InstID     string `json:"instId"`
	Side       string `json:"side"`
	PosSide    string `json:"posSide"`
	OrdType    string `json:"ordType"`
	Sz         string `json:"sz"` // contracts for a swap
	Px         string `json:"px"`
	ClOrdID    string `json:"clOrdId"`
	OrdID      string `json:"ordId"`
	ReduceOnly any    `json:"reduceOnly"`
}

// readOKXBody reads the order request of a POST: an object, or the first of a batch (the
// simulator answers one order per request).
func readOKXBody(r *http.Request) (okxOrderReq, bool) {
	var req okxOrderReq
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return req, false
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) > 0 && body[0] == '[' {
		var batch []okxOrderReq
		if json.Unmarshal(body, &batch) != nil || len(batch) == 0 {
			return req, false
		}
		return batch[0], true
	}
	return req, json.Unmarshal(body, &req) == nil
}

// simOKXCreateOrder handles POST /api/v5/trade/order and /batch-orders.
func (sim *StatefulSimulator) simOKXCreateOrder(state *VenueState, r *http.Request) (*Response, bool) {
	req, ok := readOKXBody(r)
	if !ok {
		return sim.okxErrorResponse("51000", "invalid JSON"), true
	}
	sp := orderSpec{Market: "futures", Symbol: req.InstID, Side: req.Side, Type: req.OrdType, ClientID: req.ClOrdID, PosSide: req.PosSide,
		ReduceOnly: req.ReduceOnly == true || req.ReduceOnly == "true"}
	if sp.PosSide == "net" {
		sp.PosSide = ""
	}
	bad := func(msg string) (*Response, bool) {
		return sim.okxReject(&reject{kind: "param", detail: msg}, req.ClOrdID), true
	}
	switch {
	case sp.Symbol == "":
		return bad("instId is missing")
	case sp.Side != "buy" && sp.Side != "sell":
		return bad("side is buy or sell")
	case sp.PosSide != "" && sp.PosSide != "long" && sp.PosSide != "short":
		return sim.okxReject(&reject{kind: "side"}, req.ClOrdID), true
	case sp.Type != "market" && sp.Type != "limit":
		return bad("ordType is market or limit (conditional and post-only types are not simulated)")
	}
	if sp.Amount, ok = numArg(req.Sz); !ok || sp.Amount <= 0 {
		return bad("sz is not a positive number")
	}
	if sp.Type == "limit" {
		if sp.Price, ok = numArg(req.Px); !ok || sp.Price <= 0 {
			return bad("px is not a positive number")
		}
	}
	o, _, rj := sim.book("okx").place(sp)
	if rj != nil {
		return sim.okxReject(rj, req.ClOrdID), true
	}
	// An OKX create answer carries no fill and no state: create_reports_fill has nothing to lie about.
	return jsonResponse(200, map[string]any{"code": "0", "msg": "", "data": []map[string]any{
		{"ordId": o.ID, "clOrdId": o.ClientID, "tag": "", "sCode": "0", "sMsg": "", "ts": strconv.FormatInt(o.CreatedAt.UnixMilli(), 10)}}}), true
}

func (sim *StatefulSimulator) okxOrderList(os ...*Order) *Response {
	data := make([]map[string]any, 0, len(os))
	for _, o := range os {
		data = append(data, sim.okxOrderToWireFormat(o))
	}
	return jsonResponse(200, map[string]any{"code": "0", "msg": "", "data": data})
}

// simOKXFetchOrder handles GET /api/v5/trade/order, by ordId or clOrdId.
func (sim *StatefulSimulator) simOKXFetchOrder(state *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}
	id, cid := r.FormValue("ordId"), r.FormValue("clOrdId")
	if id == "" && cid == "" {
		return sim.okxErrorResponse("51000", "Parameter ordId or clOrdId is missing"), true
	}
	o := sim.book("okx").find(id, cid, r.FormValue("instId"))
	if o == nil || !o.visible(sim.clock()) {
		return sim.okxErrorResponse(okxOrderNotFound, "Order does not exist"), true
	}
	return sim.okxOrderList(o), true
}

// simOKXCancelOrder handles POST /api/v5/trade/cancel-order.
func (sim *StatefulSimulator) simOKXCancelOrder(state *VenueState, r *http.Request) (*Response, bool) {
	req, ok := readOKXBody(r)
	if !ok {
		return sim.okxErrorResponse("51000", "invalid JSON"), true
	}
	b := sim.book("okx")
	o := b.find(req.OrdID, req.ClOrdID, req.InstID)
	if req.OrdID == "" && req.ClOrdID == "" {
		return sim.okxErrorResponse("51000", "Parameter ordId or clOrdId is missing"), true
	}
	if o == nil || !stateOf("okx", o.Status).open() {
		return sim.okxErrorResponse(okxOrderNotFound, "Order does not exist"), true
	}
	b.close(o, stCanceled, sim.clock())
	sim.reschedule(sim.clock())
	return jsonResponse(200, map[string]any{"code": "0", "msg": "", "data": []map[string]any{
		{"ordId": o.ID, "clOrdId": o.ClientID, "sCode": "0", "sMsg": ""}}}), true
}

// simOKXListOrders handles GET orders-pending and orders-history, for one instType
// (SWAP, SPOT) and optionally one instrument.
func (sim *StatefulSimulator) simOKXListOrders(state *VenueState, r *http.Request, history bool) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}
	typ := r.FormValue("instType")
	var out []*Order
	for _, o := range sim.book("okx").list(history, r.FormValue("instId"), sim.clock()) {
		if typ == "" || typ == okxInstType(o) {
			out = append(out, o)
		}
	}
	return sim.okxOrderList(out...), true
}

func okxInstType(o *Order) string {
	if o.Market == "spot" {
		return "SPOT"
	}
	return "SWAP"
}

// okxOrderToWireFormat converts an Order to OKX wire format: the fields ccxt's parseOrder reads.
// CRITICAL: uses "state" (not "status") - CCXT expects OKX values like "filled", "live", "canceled".
func (sim *StatefulSimulator) okxOrderToWireFormat(order *Order) map[string]interface{} {
	posSide, _ := order.Params["posSide"].(string)
	if posSide == "" {
		posSide = "net"
	}
	px, avg := "", ""
	if order.Price > 0 {
		px = formatNumber(order.Price)
	}
	if order.Filled > 0 {
		avg = formatNumber(order.Average)
	}
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	return map[string]interface{}{
		"ordId":      order.ID,
		"clOrdId":    order.ClientID,
		"instId":     order.Symbol,
		"instType":   okxInstType(order),
		"side":       order.Side,
		"posSide":    posSide,
		"ordType":    order.OrderType,
		"sz":         formatNumber(order.Amount),
		"px":         px,
		"state":      order.Status, // OKX uses "state" not "status"; value like "filled", "live", "canceled"
		"avgPx":      avg,
		"accFillSz":  formatNumber(order.Filled),
		"reduceOnly": strconv.FormatBool(order.ReduceOnly),
		"cTime":      ms(order.CreatedAt),
		"uTime":      ms(order.UpdatedAt),
		"sCode":      "0", // Per-order success code (0 = success)
		"sMsg":       "",  // Per-order success message
	}
}
