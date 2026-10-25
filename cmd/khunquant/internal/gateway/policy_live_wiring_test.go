package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// moneyProbe stands in for create_order: it counts its runs and places nothing.
type moneyProbe struct{ runs atomic.Int32 }

func (*moneyProbe) Name() string               { return tools.NameCreateOrder }
func (*moneyProbe) Description() string        { return "probe" }
func (*moneyProbe) Parameters() map[string]any { return map[string]any{} }
func (m *moneyProbe) Execute(context.Context, map[string]any) *tools.ToolResult {
	m.runs.Add(1)
	return tools.NewToolResult("placed")
}

// tradeAs runs the probe as the given sender in a chat turn and reports whether the
// call ran and the result text.
func tradeAs(f *assemblyFixture, sender string) (ran bool, text string) {
	probe := &moneyProbe{}
	reg := f.al.GetRegistry().GetDefaultAgent().Tools
	reg.Register(probe)
	ctx := tools.WithToolSender(tools.WithToolOrigin(context.Background(), tools.OriginRemote), sender)
	res := reg.ExecuteWithContext(ctx, tools.NameCreateOrder,
		map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "buy", "amount": 1.0, "price": 10.0, "confirm": true},
		"telegram", "c", nil)
	return probe.runs.Load() == 1, res.ForLLM
}

func withShareCap(cfg *config.Config) { cfg.TradingRisk.MaxSymbolSharePct = 20 }

// Live is armed only with proactive on, a valid owner and brakes that validate, and
// follows the config across reloads; everywhere else it records and refuses nothing.
func TestPolicyLive_ArmedOnlyWhenEverythingItNeedsIsThere(t *testing.T) {
	t.Cleanup(func() { tools.SetPolicyShadow(nil); tools.SetPolicyLive(nil) })
	f := newAssemblyFixture(t, withPolicy(config.ProactiveModeLive, true))
	if got := f.services.Policy.mode; got != config.ProactiveModeLive {
		t.Fatalf("armed mode = %q, want live", got)
	}
	if ran, text := tradeAs(f, "telegram:999"); ran || !strings.Contains(text, "may only inform the owner") {
		t.Fatalf("live: another person's order ran=%v: %q", ran, text)
	}
	if ran, text := tradeAs(f, "telegram:1001"); !ran {
		t.Fatalf("live: the owner's own order was refused: %q", text)
	}

	// Back to shadow: the engine records and nothing is enforced.
	if err := f.reload(withPolicy(config.ProactiveModeShadow, true)); err != nil {
		t.Fatal(err)
	}
	if got := f.services.Policy.mode; got != config.ProactiveModeShadow {
		t.Fatalf("armed mode = %q, want shadow", got)
	}
	if ran, text := tradeAs(f, "telegram:999"); !ran {
		t.Fatalf("shadow enforced: %q", text)
	}

	// Live again, but a share cap is set and no portfolio source exists: the cap
	// would refuse every buy, so live stays shadow (validation says why).
	if err := f.reload(func(cfg *config.Config) { withPolicy(config.ProactiveModeLive, true)(cfg); withShareCap(cfg) }); err != nil {
		t.Fatal(err)
	}
	if got := f.services.Policy.mode; got != config.ProactiveModeShadow {
		t.Fatalf("with a share cap the armed mode = %q, want shadow", got)
	}
	if ran, text := tradeAs(f, "telegram:1001"); !ran {
		t.Fatalf("a share cap blocked the owner's buy: %q", text)
	}

	// Live without an owner is shadow (Effective lowers it).
	if err := f.reload(func(cfg *config.Config) {
		cfg.Proactive.Enabled = true
		cfg.Proactive.Policy.Mode = config.ProactiveModeLive
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.services.Policy.mode; got != config.ProactiveModeShadow {
		t.Fatalf("live without an owner armed %q, want shadow", got)
	}
	if ran, text := tradeAs(f, "telegram:999"); !ran {
		t.Fatalf("live without an owner enforced: %q", text)
	}

	// Live, then off: nothing is armed any more.
	if err := f.reload(withPolicy(config.ProactiveModeLive, true)); err != nil {
		t.Fatal(err)
	}
	if err := f.reload(withPolicy(config.ProactiveModeOff, true)); err != nil {
		t.Fatal(err)
	}
	if f.services.Policy != nil {
		t.Fatal("a policy runtime survived mode off")
	}
	if ran, text := tradeAs(f, "telegram:999"); !ran {
		t.Fatalf("mode off enforced: %q", text)
	}
}

// The stats route shows the armed mode, today's decisions by mode and the recorder's
// dropped count, as counts only.
func TestPolicyLive_StatsShowModeDecisionsAndDropped(t *testing.T) {
	t.Cleanup(func() { tools.SetPolicyShadow(nil); tools.SetPolicyLive(nil) })
	f := newAssemblyFixture(t, withPolicy(config.ProactiveModeLive, true))
	tradeAs(f, "telegram:999")  // notify_only
	tradeAs(f, "telegram:1001") // act
	if !f.services.Policy.rec.Flush(5 * time.Second) {
		t.Fatal("the recorder did not drain")
	}
	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != 200 {
		t.Fatalf("stats: %d %v", code, err)
	}
	var got struct {
		Policy *policyStatsResponse `json:"policy"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Policy == nil {
		t.Fatalf("no policy part in %s (%v)", body, err)
	}
	if got.Policy.Mode != "live" || got.Policy.DecisionsToday["notify_only"] < 1 || got.Policy.DecisionsToday["act"] < 1 || got.Policy.Dropped != 0 {
		t.Fatalf("policy stats = %+v", got.Policy)
	}

	if err := f.reload(withPolicy(config.ProactiveModeOff, true)); err != nil {
		t.Fatal(err)
	}
	_, body, _ = getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if strings.Contains(string(body), `"policy"`) {
		t.Fatalf("a policy part with the engine off: %s", body)
	}
}
