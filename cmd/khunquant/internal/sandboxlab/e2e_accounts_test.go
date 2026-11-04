//go:build !windows

package sandboxlab

import (
	"strings"
	"testing"
)

// A locked child started with accounts: [okx] has a placeholder account the lock put in:
// a tool places an order on it, and the simulator's ledger journals it. (The credentials
// are in memory only; the unit tests in the gateway and supervisor packages check the files.)
// KQ_SANDBOX_E2E_BIN names a prebuilt khunquant.
func TestE2E_PlaceholderAccountPlacesAnOrder(t *testing.T) {
	bin := e2eBinary(t)
	src := strings.Replace(e2eHead, "%s", "placeholder-account-order", 1) + `
llm: scripted
instance:
  accounts: [okx]
  config:
    proactive:
      enabled: true
      owner: { channel: simchat, chat_id: "dm:owner", sender_id: "simchat:owner" }
market:
  BTC/USDT: { venue: okx, model: set, price: 98000 }
account:
  okx: { balances: { USDT: 10000 } }
setup:
  - call_tool:
      as: owner
      name: create_dca_plan
      args: { plan_name: okx-daily, provider: okx, symbol: BTC/USDT, amount_per_order: 100, schedule: { cron: "0 10 * * *", timezone: Asia/Bangkok } }
  - call_tool:
      as: owner
      name: execute_dca_order
      args: { plan_id: 1 }
timeline:
  - advance: 1m
expect:
  - id: bought
    order: { venue: okx, symbol: BTC/USDT, side: buy, count: 1 }
  - id: filled
    order: { venue: okx, symbol: BTC/USDT, status: filled, count: 1 }
`
	file := writeFile(t, t.TempDir(), "placeholder-account-order.yaml", src)
	out, code, _ := runBinary(t, bin, file)
	if code != 0 || !strings.Contains(out, "pass     placeholder-account-order") {
		t.Fatalf("exit %d: a locked child with accounts: [okx] must place an order through the tool\n%s", code, out)
	}
}
