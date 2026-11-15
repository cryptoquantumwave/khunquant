package agent

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// B9-7: after a config reload through the real ReloadProviderAndConfig (not a direct call to
// armToolPolicy), the autonomous ceiling and the trust floor still hold, and the parts of the
// policy that come from the configuration (here: whether approvals are on, which gates
// propose_order) follow the NEW configuration in both directions.
func TestB97_ReloadRearmsTheCeilingAndTheTrustFloor(t *testing.T) {
	t.Cleanup(func() { tools.SetOrderProposer(nil); tools.SetProposalConfig(nil); tools.SetPolicyConfig(false) })
	rp := &recordingProposer{}
	tools.SetOrderProposer(rp)

	// Generation one: proactive on, approvals OFF.
	al := proactiveLoop(t, script(providers.LLMResponse{Content: "ok"}))

	reload := func(mutate func(*config.Config)) {
		t.Helper()
		next := *al.GetConfig()
		mutate(&next)
		if err := al.ReloadProviderAndConfig(context.Background(), script(providers.LLMResponse{Content: "ok"}), &next); err != nil {
			t.Fatalf("reload: %v", err)
		}
	}

	searchCall := providers.ToolCall{ID: "s1", Type: "function", Name: tools.NameWebSearch, Arguments: map[string]any{"query": "btc"}}
	proposeCall := providers.ToolCall{ID: "p1", Type: "function", Name: tools.NameProposeOrder, Arguments: proposeArgs()}
	silent := respondResponse(map[string]any{"decision": "silent"})

	// A candidate-driven proactive turn that delivers now, with the script given. The tools it
	// registers are re-registered on every call: a reload replaces the registry.
	proactiveTurn := func(t *testing.T, responses ...providers.LLMResponse) *scriptProvider {
		t.Helper()
		prov := script(responses...)
		if err := al.ReloadProviderAndConfig(context.Background(), prov, al.GetConfig()); err != nil {
			t.Fatalf("swap provider: %v", err)
		}
		readTool(al, tools.NameGetTicker, func(context.Context) *tools.ToolResult { return tools.SilentResult("tick") })
		al.RegisterTool(&funcTool{name: tools.NameWebSearch, fn: func(context.Context, map[string]any) *tools.ToolResult {
			return tools.SilentResult("results")
		}})
		req := proactiveReq(trustedCand("c1"))
		req.DeliverNow = true
		req.Tools = []string{tools.NameGetTicker, tools.NameWebSearch, tools.NameProposeOrder}
		mustRun(t, al, req)
		return prov
	}
	heartbeatCreateOrder := func(t *testing.T) int {
		t.Helper()
		prov := script(callsResponse(providers.ToolCall{ID: "c1", Type: "function", Name: tools.NameCreateOrder, Arguments: map[string]any{"value": "1"}}),
			providers.LLMResponse{Content: "HEARTBEAT_OK"})
		if err := al.ReloadProviderAndConfig(context.Background(), prov, al.GetConfig()); err != nil {
			t.Fatalf("swap provider: %v", err)
		}
		rec := registerRecording(al, tools.NameCreateOrder)[tools.NameCreateOrder]
		if _, err := al.ProcessHeartbeat(context.Background(), "check the watchlist", "telegram", "424242"); err != nil {
			t.Fatal(err)
		}
		if got := defNames(prov.defs[0]); hasName(got, tools.NameCreateOrder) {
			t.Errorf("a heartbeat turn was shown create_order: %v", got)
		}
		return len(rec.calls())
	}

	// Before the reload: approvals off, so propose_order is neither offered nor callable.
	rp.reqs = nil
	prov := proactiveTurn(t, callsResponse(proposeCall), silent)
	if offers(defNames(prov.defs[0])) || rp.count() != 0 {
		t.Fatalf("approvals off: offered=%v, %d proposals", offers(defNames(prov.defs[0])), rp.count())
	}

	// The reload that turns approvals on.
	reload(withApprovals)

	// The ceiling: a heartbeat-origin turn cannot call a trade tool.
	if n := heartbeatCreateOrder(t); n != 0 {
		t.Errorf("a heartbeat turn executed create_order %d time(s) after a reload", n)
	}

	// Control: with the trust floor untouched the proposal goes through under the new configuration.
	prov = proactiveTurn(t, callsResponse(proposeCall), silent)
	if !offers(defNames(prov.defs[0])) || rp.count() != 1 {
		t.Fatalf("after the reload turned approvals on: offered=%v, %d proposals (the new configuration was not armed)",
			offers(defNames(prov.defs[0])), rp.count())
	}

	// The trust floor: a turn that called web_search cannot propose afterwards, alone or in one batch.
	prov = proactiveTurn(t, callsResponse(searchCall), callsResponse(proposeCall), silent)
	if offers(defNames(prov.defs[1])) || rp.count() != 1 {
		t.Errorf("after a search: still offered=%v, %d proposals total (want the one from the control run)",
			offers(defNames(prov.defs[1])), rp.count())
	}
	proactiveTurn(t, callsResponse(proposeCall, searchCall), silent)
	if rp.count() != 1 {
		t.Errorf("a batch holding a search and a proposal proposed after a reload: %d proposals", rp.count())
	}

	// And the other direction: a reload that switches approvals off closes the tool again.
	reload(func(c *config.Config) { c.Proactive.Approvals = config.ApprovalsConfig{} })
	prov = proactiveTurn(t, callsResponse(proposeCall), silent)
	if offers(defNames(prov.defs[0])) || rp.count() != 1 {
		t.Errorf("after the reload turned approvals off: offered=%v, %d proposals", offers(defNames(prov.defs[0])), rp.count())
	}
	if n := heartbeatCreateOrder(t); n != 0 {
		t.Errorf("a heartbeat turn executed create_order %d time(s) after the second reload", n)
	}
}
