package fault

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// maxClassifyBody bounds how much of a request body ClassifyOrder reads; the rest is left
// for the simulator, which reads the whole body afterwards.
const maxClassifyBody = 1 << 20

// binanceOrderPath matches the order endpoints exactly, so /api/v3/order/oco and
// /api/v3/orderList are not mistaken for them.
var (
	binanceOrderPath = regexp.MustCompile(`^/(fapi|dapi|api)/v[0-9]+/(order|openOrders|allOrders)$`)
	okxDatedFutures  = regexp.MustCompile(`-[0-9]{6}$`)
)

// okxOrderPaths maps an OKX order endpoint and method to its operation.
var okxOrderPaths = map[string]sandbox.OrderOp{
	"POST /api/v5/trade/order": sandbox.OrderCreate, "POST /api/v5/trade/batch-orders": sandbox.OrderCreate,
	"POST /api/v5/trade/cancel-order": sandbox.OrderCancel, "POST /api/v5/trade/cancel-batch-orders": sandbox.OrderCancel,
	"GET /api/v5/trade/order": sandbox.OrderFetch, "GET /api/v5/trade/orders-pending": sandbox.OrderList,
	"GET /api/v5/trade/orders-history": sandbox.OrderList, "GET /api/v5/trade/orders-history-archive": sandbox.OrderList,
}

// ClassifyOrder says whether a request to venue is an order operation and, if so, which
// order it is about. ok is false for every other request, and for a venue it has no rules
// for (Settrade and Webull rows arrive with SBX-14c). It reads the request body up to a
// cap and puts it back, so the simulator reads it whole afterwards.
func ClassifyOrder(venue, method, path string, r *http.Request) (sandbox.OrderOp, sandbox.OrderInfo, bool) {
	switch venue {
	case "binance", "binanceth": // Binance TH speaks Binance's API
		return classifyBinance(venue, method, path, r)
	case "okx":
		return classifyOKX(method, path, r)
	}
	return "", sandbox.OrderInfo{}, false
}

// peekBody returns up to maxClassifyBody bytes of the request body and restores the body
// so the next reader sees all of it.
func peekBody(r *http.Request) []byte {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	head, _ := io.ReadAll(io.LimitReader(r.Body, maxClassifyBody))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
	return head
}

func classifyBinance(venue, method, path string, r *http.Request) (sandbox.OrderOp, sandbox.OrderInfo, bool) {
	m := binanceOrderPath.FindStringSubmatch(path)
	if m == nil || (m[1] == "api" && !strings.HasPrefix(path, "/api/v3/")) {
		return "", sandbox.OrderInfo{}, false
	}
	var op sandbox.OrderOp
	switch {
	case m[2] == "order" && method == http.MethodPost:
		op = sandbox.OrderCreate
	case m[2] == "order" && method == http.MethodGet:
		op = sandbox.OrderFetch
	case method == http.MethodDelete: // one order, or every open order
		op = sandbox.OrderCancel
	case m[2] != "order" && method == http.MethodGet:
		op = sandbox.OrderList
	default:
		return "", sandbox.OrderInfo{}, false
	}
	// Signed parameters travel in the query string, the form body, or both.
	vals := url.Values{}
	if r != nil && r.URL != nil {
		vals = r.URL.Query()
	}
	if body := peekBody(r); len(body) > 0 && !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		if form, err := url.ParseQuery(string(body)); err == nil {
			for k, v := range form {
				if _, set := vals[k]; !set {
					vals[k] = v
				}
			}
		}
	}
	info := sandbox.OrderInfo{
		Venue: venue, Market: "spot", Symbol: vals.Get("symbol"), Side: strings.ToLower(vals.Get("side")),
		Type: strings.ToLower(vals.Get("type")), ClientID: firstSet(vals.Get("newClientOrderId"), vals.Get("origClientOrderId")),
		ReduceOnly: strings.EqualFold(vals.Get("reduceOnly"), "true"),
	}
	if m[1] != "api" {
		info.Market = "futures"
	}
	info.Amount, _ = strconv.ParseFloat(vals.Get("quantity"), 64)
	return op, info, true
}

func classifyOKX(method, path string, r *http.Request) (sandbox.OrderOp, sandbox.OrderInfo, bool) {
	op, ok := okxOrderPaths[method+" "+path]
	if !ok {
		return "", sandbox.OrderInfo{}, false
	}
	f := map[string]any{}
	if r != nil && r.URL != nil {
		for k, v := range r.URL.Query() {
			f[k] = v[0]
		}
	}
	if body := bytes.TrimSpace(peekBody(r)); len(body) > 0 {
		var one map[string]any
		var many []map[string]any
		switch {
		case json.Unmarshal(body, &one) == nil:
			for k, v := range one {
				f[k] = v
			}
		case json.Unmarshal(body, &many) == nil && len(many) > 0: // a batch: the first order, as the simulator does
			for k, v := range many[0] {
				f[k] = v
			}
		}
	}
	info := sandbox.OrderInfo{
		Venue: "okx", Symbol: str(f["instId"]), Side: strings.ToLower(str(f["side"])), Type: strings.ToLower(str(f["ordType"])),
		ClientID: str(f["clOrdId"]), ReduceOnly: str(f["reduceOnly"]) == "true", Market: "spot",
	}
	if inst, typ := strings.ToUpper(info.Symbol), strings.ToUpper(str(f["instType"])); strings.HasSuffix(inst, "-SWAP") ||
		strings.HasSuffix(inst, "-FUTURES") || okxDatedFutures.MatchString(inst) || typ == "SWAP" || typ == "FUTURES" {
		info.Market = "futures"
	}
	info.Amount, _ = strconv.ParseFloat(str(f["sz"]), 64)
	return op, info, true
}

// str reads a JSON scalar as text (OKX sends numbers and booleans as either).
func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

func firstSet(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// newRequest is the request a rule is matched against, built from an inbound sandbox
// request. The order fields are read only when some rule looks at them.
func (i *Injector) newRequest(venue, method, path string, r *http.Request) *request {
	q := &request{venue: venue, method: method, path: path}
	i.mu.Lock()
	classify := i.classify
	i.mu.Unlock()
	if classify {
		if op, info, ok := ClassifyOrder(venue, method, path, r); ok {
			q.op, q.info, q.classified = op, info, true
		}
	}
	return q
}
