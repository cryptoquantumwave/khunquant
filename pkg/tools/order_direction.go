package tools

import (
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// orderAddsRisk is the ONE classifier of whether an order can add to the
// account's exposure, shared by create_order (its daily-loss gate) and by the
// approval path (propose_order and the order the owner approved), so the two
// cannot disagree about what "only reduces risk" means.
//
// An order adds risk unless it is a sell that can only shrink a holding:
//
//   - every buy adds risk (it opens or enlarges a position);
//   - a sell of a contract symbol (a ":" in the unified symbol, e.g.
//     BTC/USDT:USDT) can open or enlarge a short, unless it is reduce-only, which
//     the exchange refuses to let grow a position;
//   - a sell carrying margin, borrow or leverage params can open or enlarge a
//     short on a spot symbol;
//   - a plain spot sell reduces exposure.
//
// reduceOnly only exempts an order on a contract symbol: it is meaningless on
// spot. Whether a spot sell stays within what the account holds is not decided
// here (it needs a balance); the approval path checks it on top.
func orderAddsRisk(symbol, side string, params map[string]interface{}, reduceOnly bool) bool {
	isContract := strings.Contains(symbol, ":")
	if reduceOnly && isContract {
		return false
	}
	return !strings.EqualFold(side, "sell") || isContract || orderParamsAddRisk(params)
}

// orderRiskDirection is orderAddsRisk as the direction the gates take.
func orderRiskDirection(symbol, side string, params map[string]interface{}, reduceOnly bool) broker.RiskDirection {
	if orderAddsRisk(symbol, side, params, reduceOnly) {
		return broker.RiskIncrease
	}
	return broker.RiskReduce
}
