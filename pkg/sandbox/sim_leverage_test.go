package sandbox_test

import (
	"strings"
	"testing"
)

// The two futures reads and writes a delta-neutral open makes before it places a leg are
// answered once the simulator has markets, and left to the fixtures before.
func TestBinanceFutures_LeverageBracketAndMarginType(t *testing.T) {
	r := newSimRig(t, false)
	const base = "/__sbx__/binance/fapi.binance.com/fapi/v1/"
	got := r.do(t, "GET", base+"leverageBracket?symbol=BTCUSDT", "")
	if !strings.Contains(got, `"symbol":"BTCUSDT"`) || !strings.Contains(got, `"initialLeverage":100`) || strings.Contains(got, "ETHUSDT") {
		t.Fatalf("leverageBracket = %s, want the one tier of BTCUSDT", got)
	}
	if got := r.do(t, "POST", base+"marginType?symbol=BTCUSDT&marginType=ISOLATED", ""); !strings.Contains(got, `"msg":"success"`) {
		t.Fatalf("marginType = %s", got)
	}
}
