package sandbox

import (
	"net/http"
	"strconv"
	"time"
)

// The routes that read what funding and liquidation left: Binance income and forceOrders,
// and OKX bills. The funding-rate histories are the book's (market/render_funding.go): they
// are rates, not a wallet's ledger.

// msParam reads a millisecond timestamp parameter (zero time: not given).
func msParam(r *http.Request, key string) time.Time {
	n, err := strconv.ParseInt(r.FormValue(key), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}

// limitParam reads limit, def when absent or not a number, at most max.
func limitParam(r *http.Request, def, max int) int {
	if n, err := strconv.Atoi(r.FormValue("limit")); err == nil && n > 0 {
		return min(n, max)
	}
	return def
}

// simBinanceIncome handles GET /fapi/v1/income from the funding ledger. While the ledger is
// empty it falls through to the recorded fixture, as exchangeInfo does: nothing changes for
// an instance that has paid no funding.
func (sim *StatefulSimulator) simBinanceIncome(st *VenueState, r *http.Request) (*Response, bool) {
	if len(st.Income) == 0 {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	typ, symbol, from, to := r.FormValue("incomeType"), r.FormValue("symbol"), msParam(r, "startTime"), msParam(r, "endTime")
	rows := []map[string]any{}
	for i, e := range st.Income {
		if (typ != "" && typ != e.Type) || (symbol != "" && symbol != e.Symbol) || e.Time.Before(from) || (!to.IsZero() && e.Time.After(to)) {
			continue
		}
		rows = append(rows, map[string]any{"symbol": e.Symbol, "incomeType": e.Type, "income": formatNumber(e.Income), "asset": "USDT",
			"info": e.Type, "time": e.Time.UnixMilli(), "tranId": 1000 + i, "tradeId": ""})
	}
	// Oldest first; with no start the newest limit of them, with one the first limit after it.
	if n := limitParam(r, 100, 1000); len(rows) > n {
		if from.IsZero() {
			rows = rows[len(rows)-n:]
		} else {
			rows = rows[:n]
		}
	}
	return jsonResponse(200, rows), true
}

// simBinanceForceOrders handles GET /fapi/v1/forceOrders: the liquidation orders, oldest
// first. The answer is the simulator's whether or not there are any.
func (sim *StatefulSimulator) simBinanceForceOrders(st *VenueState, r *http.Request) (*Response, bool) {
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	symbol, from, to := r.FormValue("symbol"), msParam(r, "startTime"), msParam(r, "endTime")
	rows := []map[string]any{}
	if t := r.FormValue("autoCloseType"); t == "" || t == "LIQUIDATION" {
		for _, o := range liquidationsOf(st) {
			if (symbol == "" || symbol == o.Symbol) && !o.CreatedAt.Before(from) && (to.IsZero() || !o.CreatedAt.After(to)) {
				w := sim.binanceOrderToWireFormat(o)
				w["autoCloseType"] = "LIQUIDATION"
				rows = append(rows, w)
			}
		}
	}
	if n := limitParam(r, 50, 100); len(rows) > n {
		rows = rows[:n]
	}
	return jsonResponse(200, rows), true
}

// simOKXBills handles GET /api/v5/account/bills and bills-archive (what ccxt's
// fetchFundingHistory reads) from the funding ledger, newest first, as type 8 (funding
// fee). While the ledger is empty it falls through to the fixture.
func (sim *StatefulSimulator) simOKXBills(st *VenueState, r *http.Request) (*Response, bool) {
	if len(st.Income) == 0 {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		return sim.okxErrorResponse("51000", "invalid query string"), true
	}
	typ, instID, newer, older := r.FormValue("type"), r.FormValue("instId"), msParam(r, "before"), msParam(r, "after")
	rows := []map[string]any{}
	for i := len(st.Income) - 1; i >= 0; i-- {
		e := st.Income[i]
		if (typ != "" && typ != "8") || (instID != "" && instID != e.Symbol) || (!newer.IsZero() && !e.Time.After(newer)) || (!older.IsZero() && !e.Time.Before(older)) {
			continue
		}
		sub := "174" // funding fee income
		if e.Income < 0 {
			sub = "173" // funding fee expense
		}
		rows = append(rows, map[string]any{"billId": strconv.Itoa(1000 + i), "ccy": "USDT", "instId": e.Symbol, "instType": "SWAP", "mgnMode": "cross",
			"type": "8", "subType": sub, "balChg": formatNumber(e.Income), "pnl": formatNumber(e.Income), "posBalChg": "0", "fee": "0",
			"bal": "", "posBal": "", "sz": "", "ordId": "", "execType": "", "from": "", "to": "", "notes": "", "ts": strconv.FormatInt(e.Time.UnixMilli(), 10)})
	}
	if n := limitParam(r, 100, 100); len(rows) > n {
		rows = rows[:n]
	}
	return jsonResponse(200, map[string]any{"code": "0", "msg": "", "data": rows}), true
}
