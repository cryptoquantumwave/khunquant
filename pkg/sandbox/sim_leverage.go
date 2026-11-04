package sandbox

import (
	"maps"
	"net/http"
	"slices"
)

// simBinanceAccountSetting handles POST /fapi/v1/marginType: the simulator keeps no margin
// mode (every position is margined as cross), so a change is accepted. Like exchangeInfo it
// falls through while there are no markets.
func (sim *StatefulSimulator) simBinanceAccountSetting(state *VenueState, _ *http.Request) (*Response, bool) {
	if len(state.Markets) == 0 {
		return nil, false
	}
	return jsonResponse(200, map[string]any{"code": 200, "msg": "success"}), true
}

// simBinanceLeverageBracket handles GET /fapi/v1/leverageBracket: the one leverage tier ccxt
// reads to value a position's margin (fetchPositions asks for it). Like exchangeInfo it falls
// through to the fixtures while the simulator has no markets. The tier is as wide as the
// market's MaxLeverage and has no cap, so it never limits an order the ledger accepts.
func (sim *StatefulSimulator) simBinanceLeverageBracket(state *VenueState, r *http.Request) (*Response, bool) {
	if len(state.Markets) == 0 {
		return nil, false
	}
	if err := r.ParseForm(); err != nil {
		return sim.binanceErrorResponse(400, -1000, "invalid query string"), true
	}
	want := r.FormValue("symbol")
	out := []map[string]any{}
	for _, symbol := range slices.Sorted(maps.Keys(state.Markets)) {
		if want != "" && symbol != want {
			continue
		}
		lev := state.Markets[symbol].MaxLeverage
		if lev <= 0 {
			lev = 20
		}
		out = append(out, map[string]any{"symbol": symbol, "notionalCoef": 1.0, "brackets": []map[string]any{
			{"bracket": 1, "initialLeverage": lev, "notionalCap": 1e12, "notionalFloor": 0, "maintMarginRatio": 0.005, "cum": 0.0}}})
	}
	return jsonResponse(200, out), true
}
