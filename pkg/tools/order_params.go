package tools

// Provider-specific order parameters supplied by the model.
//
// H-5: create_order and futures_open_position both advertise a free-form
// "params" object ("Extra CCXT/exchange params") and forwarded it verbatim to
// the exchange. Those parameters are not inert - they select the trading mode.
// `{"tdMode": "cross"}` turns a spot order into a cross-margin one on OKX, and
// `{"marginMode": "isolated"}` or a borrow flag does the equivalent elsewhere,
// so a model could obtain leveraged, borrowed exposure while
// trading_risk.allow_margin was false. The risk flags describe what the agent
// may do; a passthrough dictionary that can re-enable the forbidden mode makes
// them advisory.
//
// Parameters are therefore filtered: ordinary execution options pass, the ones
// that change trading mode pass only when the matching risk flag permits it,
// and anything unrecognised is refused rather than forwarded blind.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// orderParamAllowlist holds parameters that affect how an order executes but
// not what kind of exposure it creates. Keys are compared lowercased, because
// exchanges spell the same parameter in several cases (clOrdId, clientOrderId).
var orderParamAllowlist = map[string]bool{
	"clientorderid":    true,
	"clordid":          true,
	"neworderresptype": true,
	"newclientorderid": true,
	"timeinforce":      true,
	"postonly":         true,
	"reduceonly":       true,
	"stopprice":        true,
	"triggerprice":     true,
	"stoplossprice":    true,
	"takeprofitprice":  true,
	"tpslmode":         true,
	"tag":              true,
}

// marginParamKeys select margin / borrowed exposure. Permitted only when
// trading_risk.allow_margin is set.
var marginParamKeys = map[string]bool{
	"tdmode":         true,
	"marginmode":     true,
	"defaulttype":    true,
	"type":           true,
	"margin":         true,
	"isisolated":     true,
	"sideeffecttype": true,
	"autoborrow":     true,
	"autorepay":      true,
}

// leverageParamKeys select leveraged / position-mode behaviour. Permitted only
// when trading_risk.allow_leverage is set.
var leverageParamKeys = map[string]bool{
	"leverage":     true,
	"lever":        true,
	"posside":      true,
	"positionside": true,
}

// sanitizeOrderParams filters model-supplied exchange parameters against the
// configured risk flags.
//
// Returns the parameters to forward, or an error naming every rejected key so
// the model is told what was refused rather than silently having it dropped -
// a dropped parameter would otherwise look like a placed order that quietly
// behaved differently from the one requested.
func sanitizeOrderParams(raw any, risk config.TradingRiskConfig) (map[string]interface{}, error) {
	in := copyToolParams(raw)
	if len(in) == 0 {
		return nil, nil
	}

	out := make(map[string]interface{}, len(in))
	var refused []string

	for key, value := range in {
		lower := strings.ToLower(strings.TrimSpace(key))

		switch {
		case orderParamAllowlist[lower]:
			out[key] = value

		case marginParamKeys[lower] || strings.Contains(lower, "borrow"):
			if !risk.AllowMargin {
				refused = append(refused, fmt.Sprintf("%s (margin: set trading_risk.allow_margin=true to permit)", key))
				continue
			}
			out[key] = value

		case leverageParamKeys[lower]:
			if !risk.AllowLeverage {
				refused = append(refused, fmt.Sprintf("%s (leverage: set trading_risk.allow_leverage=true to permit)", key))
				continue
			}
			out[key] = value

		default:
			refused = append(refused, fmt.Sprintf("%s (not a recognised order parameter)", key))
		}
	}

	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, fmt.Errorf("refused exchange parameter(s): %s", strings.Join(refused, "; "))
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
