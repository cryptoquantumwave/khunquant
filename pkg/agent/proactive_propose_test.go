package agent

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

type recordingProposer struct {
	mu   sync.Mutex
	reqs []approval.ProposeRequest
}

func (r *recordingProposer) Propose(_ context.Context, req approval.ProposeRequest) (approval.Proposal, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return approval.Proposal{Code: "7F3K", ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
}

func (r *recordingProposer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func withApprovals(c *config.Config) {
	c.Proactive.Approvals = config.ApprovalsConfig{Enabled: true, ExpiryMinutes: 10}
}

func proposeArgs() map[string]any {
	return map[string]any{"provider": "okx", "symbol": "BTC/USDT", "side": "sell", "amount": 0.01}
}

func trustedCand(id string) proactive.Candidate {
	c := cand(id, proactive.SeverityWarning, false)
	c.Trust = proactive.TrustExchange
	return c
}

func offers(defs []string) bool { return slices.Contains(defs, tools.NameProposeOrder) }

// propose_order is offered to a candidate turn only when approvals are on, the
// candidates are all of exchange trust or better, the message is being delivered
// now and no forced skill is in the prompt; never to a heartbeat turn.
func TestProactiveTurn_ProposeOrderIsOfferedOnlyWhenTheGrantHolds(t *testing.T) {
	untrusted := trustedCand("c2")
	untrusted.Trust = proactive.TrustUntrusted
	for _, tc := range []struct {
		name   string
		cfg    []func(*config.Config)
		req    func() ProactiveTurnRequest
		offers bool
	}{
		{"trusted, delivered now, approvals on", []func(*config.Config){withApprovals},
			func() ProactiveTurnRequest { r := proactiveReq(trustedCand("c1")); r.DeliverNow = true; return r }, true},
		{"not delivered now", []func(*config.Config){withApprovals},
			func() ProactiveTurnRequest { return proactiveReq(trustedCand("c1")) }, false},
		{"one untrusted candidate", []func(*config.Config){withApprovals},
			func() ProactiveTurnRequest {
				r := proactiveReq(trustedCand("c1"), untrusted)
				r.DeliverNow = true
				return r
			}, false},
		{"forced skills in the prompt", []func(*config.Config){withApprovals},
			func() ProactiveTurnRequest {
				r := proactiveReq(trustedCand("c1"))
				r.DeliverNow, r.ForcedSkills = true, []string{"some-skill"}
				return r
			}, false},
		{"approvals off", nil,
			func() ProactiveTurnRequest { r := proactiveReq(trustedCand("c1")); r.DeliverNow = true; return r }, false},
		{"a heartbeat turn, every tool asked for", []func(*config.Config){withApprovals},
			func() ProactiveTurnRequest {
				return ProactiveTurnRequest{Kind: proactive.TurnHeartbeatLegacy, Prompt: "p", Deadline: 20 * time.Second,
					Tools: []string{AllProactiveTools}, DeliverNow: true, Candidates: []proactive.Candidate{trustedCand("c1")}}
			}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := script(respondResponse(map[string]any{"decision": "silent"}))
			al := proactiveLoop(t, prov, tc.cfg...)
			mustRun(t, al, tc.req())
			if got := offers(defNames(prov.defs[0])); got != tc.offers {
				t.Errorf("offered = %v, want %v (defs %v)", got, tc.offers, sortedDefNames(prov.defs[0]))
			}
		})
	}
}

// A proposal, a respond_proactive naming it, and the decision stays a propose. A
// search first - alone or in the same response - takes the grant away.
func TestProactiveTurn_ProposeThenRespondAndOutsideTextOrdering(t *testing.T) {
	proposeCall := func(id string) providers.ToolCall {
		return providers.ToolCall{ID: id, Type: "function", Name: tools.NameProposeOrder, Arguments: proposeArgs()}
	}
	searchCall := providers.ToolCall{ID: "s1", Type: "function", Name: tools.NameWebSearch, Arguments: map[string]any{"query": "btc"}}
	respond := func(ref string) providers.LLMResponse {
		return respondResponse(map[string]any{"decision": "propose", "text": "trim it", "proposal_ref": ref, "candidate_ids": []any{"c1"}})
	}
	run := func(t *testing.T, responses ...providers.LLMResponse) (*scriptProvider, *recordingProposer, ProactiveTurnResult) {
		prov := script(responses...)
		al := proactiveLoop(t, prov, withApprovals)
		readTool(al, tools.NameGetTicker, func(context.Context) *tools.ToolResult { return tools.SilentResult("tick") })
		al.RegisterTool(&funcTool{name: tools.NameWebSearch, fn: func(context.Context, map[string]any) *tools.ToolResult {
			return tools.SilentResult("results")
		}})
		rp := &recordingProposer{}
		tools.SetOrderProposer(rp)
		t.Cleanup(func() { tools.SetOrderProposer(nil) })
		req := proactiveReq(trustedCand("c1"))
		req.DeliverNow = true
		req.Tools = []string{tools.NameGetTicker, tools.NameWebSearch, tools.NameProposeOrder}
		return prov, rp, mustRun(t, al, req)
	}

	t.Run("propose, then respond naming it", func(t *testing.T) {
		_, rp, res := run(t, callsResponse(proposeCall("p1")), respond("7f3k"))
		if rp.count() != 1 || rp.reqs[0].Origin != approval.OriginProactive || rp.reqs[0].Trust != proactive.TrustExchange ||
			!rp.reqs[0].DeliverNow || rp.reqs[0].CandidateID != "c1" {
			t.Fatalf("proposer saw %+v", rp.reqs)
		}
		if d := res.Decisions[0]; d.Kind != proactive.DecidePropose || d.ProposalRef != "7F3K" {
			t.Errorf("decision = %+v", d)
		}
	})

	t.Run("a propose naming nothing this turn created is an ask", func(t *testing.T) {
		_, rp, res := run(t, respond("7F3K"))
		if rp.count() != 0 || res.Decisions[0].Kind != proactive.DecideAsk {
			t.Errorf("proposals %d, decision %+v", rp.count(), res.Decisions[0])
		}
	})

	t.Run("search, then propose: not offered, refused", func(t *testing.T) {
		prov, rp, _ := run(t, callsResponse(searchCall), callsResponse(proposeCall("p1")), respondResponse(map[string]any{"decision": "silent"}))
		if !offers(defNames(prov.defs[0])) || offers(defNames(prov.defs[1])) {
			t.Errorf("offered before the search %v, after %v", offers(defNames(prov.defs[0])), offers(defNames(prov.defs[1])))
		}
		if rp.count() != 0 {
			t.Fatal("proposed after reading the web")
		}
		if got := toolResultFor(t, prov, 2, "p1"); !strings.Contains(got, "not permitted") && !strings.Contains(got, "not available") {
			t.Errorf("the refusal = %q", got)
		}
	})

	t.Run("search and propose in one response: refused whatever the scheduling", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			_, rp, _ := run(t, callsResponse(proposeCall("p1"), searchCall), respondResponse(map[string]any{"decision": "silent"}))
			if rp.count() != 0 {
				t.Fatalf("iteration %d: a proposal was made in a batch that also searched the web", i)
			}
		}
	})
}

// The owner's own conversation is offered the tool (approvals on); another chat is not.
func TestUserTurn_ProposeOrderIsOfferedToTheOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    []func(*config.Config)
		user   string
		offers bool
	}{
		{"the owner, approvals on", []func(*config.Config){withApprovals}, "424242", true},
		{"another user", []func(*config.Config){withApprovals}, "999", false},
		{"the owner, approvals off", nil, "424242", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := script(providers.LLMResponse{Content: "ok"})
			al := proactiveLoop(t, prov, tc.cfg...)
			if _, err := al.processMessage(context.Background(), dmFrom(tc.user, "hello")); err != nil {
				t.Fatal(err)
			}
			if got := offers(defNames(prov.defs[0])); got != tc.offers {
				t.Errorf("offered = %v, want %v", got, tc.offers)
			}
		})
	}
}
