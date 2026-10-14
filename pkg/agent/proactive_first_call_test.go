package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// The size of a proactive turn's first provider call is the input every wake-up
// pays before the model has done anything. The plan's target is a median of
// 10K input tokens; at four bytes a token that is 40,000 bytes for a governed
// heartbeat (which carries the owner's HEARTBEAT.md) and a tighter bound for a
// turn about candidates, which carries almost no text of its own.
//
// Both are measured the way TestProactiveTurn_InputWithinQuarterOfHeartbeat
// measures: the JSON of the first call's messages plus the JSON of its tool
// definitions, on the repo's default workspace and the default config. The
// system prompt is counted as the request carries it, as both Content and
// SystemParts, so a provider that sends it once sees about this much less.
const (
	governedHeartbeatFirstCallBudget = 40_000
	candidateTurnFirstCallBudget     = 32_000
)

// registerGatewayReadTools registers the read tools the gateway adds to the
// loop after NewAgentLoop (the DCA and delta-neutral stores exist only there)
// and the futures one the default config leaves off, so that the default tool
// subsets are measured with every tool they name present. Definitions do not
// read the stores, so none is given.
func registerGatewayReadTools(al *AgentLoop) {
	cfg := al.GetConfig()
	al.RegisterTool(tools.NewGetDCASummaryTool(cfg, nil))
	al.RegisterTool(tools.NewGetDeltaNeutralSummaryTool(cfg, nil))
	al.RegisterTool(tools.NewFuturesRiskSummaryTool(cfg))
}

type sizedPart struct {
	name  string
	bytes int
}

func sortedBySize(parts []sizedPart) []sizedPart {
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].bytes > parts[j].bytes })
	return parts
}

// firstCall is the measured first provider call of a turn.
type firstCall struct {
	total, messages, defs int
	system                []sizedPart // system prompt sections, content bytes
	user                  []sizedPart // the user message by part, content bytes
	tools                 []sizedPart // tool definitions, JSON bytes
}

func measureFirstCall(t *testing.T, p *scriptProvider) firstCall {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.msgs) == 0 {
		t.Fatal("the provider was never called")
	}
	msgs, err := json.Marshal(p.msgs[0])
	if err != nil {
		t.Fatal(err)
	}
	defs, err := json.Marshal(p.defs[0])
	if err != nil {
		t.Fatal(err)
	}
	fc := firstCall{total: len(msgs) + len(defs), messages: len(msgs), defs: len(defs)}

	for _, m := range p.msgs[0] {
		switch m.Role {
		case "system":
			for _, sec := range strings.Split(m.Content, "\n\n---\n\n") {
				head, _, _ := strings.Cut(sec, "\n")
				if len(head) > 36 {
					head = head[:36]
				}
				fc.system = append(fc.system, sizedPart{head, len(sec)})
			}
		case "user":
			fc.user = splitUserMessage(m.Content)
		}
	}
	for _, d := range p.defs[0] {
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		fc.tools = append(fc.tools, sizedPart{d.Function.Name, len(b)})
	}
	sortedBySize(fc.system)
	sortedBySize(fc.tools)
	return fc
}

// splitUserMessage splits a proactive user message into the contract, the
// caller's prompt and the candidate block.
func splitUserMessage(content string) []sizedPart {
	contract := strings.TrimSpace(proactiveContract)
	var parts []sizedPart
	rest := strings.TrimPrefix(content, contract)
	if len(rest) != len(content) {
		parts = append(parts, sizedPart{"contract", len(contract)})
	}
	if i := strings.Index(rest, "Candidates for this wake-up"); i >= 0 {
		parts = append(parts, sizedPart{"caller prompt", i}, sizedPart{"candidate block", len(rest) - i})
	} else {
		parts = append(parts, sizedPart{"caller prompt", len(rest)})
	}
	return parts
}

// String is the breakdown a budget failure prints, so the part to cut is named.
func (fc firstCall) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "first call %d bytes = messages %d + tool definitions %d (%d tools)\n",
		fc.total, fc.messages, fc.defs, len(fc.tools))
	section := func(title string, parts []sizedPart) {
		fmt.Fprintf(&b, "  %s:\n", title)
		for _, p := range parts {
			fmt.Fprintf(&b, "    %6d  %s\n", p.bytes, p.name)
		}
	}
	section("system prompt sections (content bytes; carried twice in the request)", fc.system)
	section("user message parts (content bytes)", fc.user)
	section("tool definitions (JSON bytes)", fc.tools)
	return b.String()
}

func governedHeartbeatRequest() ProactiveTurnRequest {
	return ProactiveTurnRequest{
		Kind: proactive.TurnHeartbeatLegacy,
		// The v2 template, wrapped the way the governed service wraps it.
		Prompt:   heartbeat.GovernedPrompt(time.Now().Format("2006-01-02 15:04:05"), heartbeat.TemplateV2()),
		Deadline: 20 * time.Second,
	}
}

func candidateTurnRequest() ProactiveTurnRequest {
	req := proactiveReq(
		cand("01JABCDEFGHJKMNPQRSTVWXYZ0", proactive.SeverityWarning, true),
		cand("01JABCDEFGHJKMNPQRSTVWXYZ1", proactive.SeverityNotice, false),
		cand("01JABCDEFGHJKMNPQRSTVWXYZ2", proactive.SeverityNotice, false),
	)
	req.Prompt = "These candidates fired since the last wake-up. The owner already sees the evidence block " +
		"for each; add only why it matters and what to do next."
	return req
}

func runAndMeasure(t *testing.T, req ProactiveTurnRequest, fixtures ...budgetFixture) firstCall {
	t.Helper()
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al, _ := defaultWorkspaceLoopWith(t, prov, fixtures...)
	registerGatewayReadTools(al)
	mustRun(t, al, req)
	return measureFirstCall(t, prov)
}

func TestProactiveTurn_GovernedHeartbeatFirstCallWithinBudget(t *testing.T) {
	fc := runAndMeasure(t, governedHeartbeatRequest())
	t.Log("\n" + fc.String())
	if fc.total > governedHeartbeatFirstCallBudget {
		t.Errorf("a governed heartbeat's first call is %d bytes, budget %d (about 10K tokens)\n%s",
			fc.total, governedHeartbeatFirstCallBudget, fc)
	}
}

func TestProactiveTurn_CandidateTurnFirstCallWithinBudget(t *testing.T) {
	fc := runAndMeasure(t, candidateTurnRequest())
	t.Log("\n" + fc.String())
	if fc.total > candidateTurnFirstCallBudget {
		t.Errorf("a candidate-driven turn's first call is %d bytes, budget %d\n%s",
			fc.total, candidateTurnFirstCallBudget, fc)
	}
}

// The budget is only honest if the default subsets are really what the turns
// above were offered: every tool the lists name, once registered, is defined.
func TestProactiveTurn_DefaultSubsetsAreOfferedInFull(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  ProactiveTurnRequest
		want []string
	}{
		{"governed heartbeat", governedHeartbeatRequest(), heartbeatTools},
		{"candidate turn", candidateTurnRequest(), candidateTools},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := runAndMeasure(t, tc.req)
			offered := map[string]bool{}
			for _, p := range fc.tools {
				offered[p.name] = true
			}
			// propose_order is in the candidate list but exists only while approvals
			// are on (off here): the list is the same, the registry decides.
			want := slices.DeleteFunc(slices.Clone(tc.want), func(n string) bool { return n == tools.NameProposeOrder })
			for _, name := range want {
				if !offered[name] {
					t.Errorf("default subset names %q but the turn was not offered it (offered: %v)", name, offered)
				}
			}
			if !offered[tools.NameRespondProactive] {
				t.Error("respond_proactive is not offered")
			}
			if got, wantN := len(fc.tools), len(want)+1; got != wantN {
				t.Errorf("turn was offered %d tools, want exactly the %d of its default subset plus respond_proactive",
					got, wantN-1)
			}
		})
	}
}

// A proactive turn writes suggestions and does not read AGENTS.md, so the advice
// rules that file carries (risk tolerance, no leverage for a conservative owner,
// no acting on one unverified source) must be in the contract itself.
func TestProactiveContract_CarriesTheAdviceRules(t *testing.T) {
	for _, want := range []string{
		"an option for the owner to decide",
		"never an instruction",
		"was or will be done",
		"risk tolerance",
		"no leverage, margin or derivatives for a conservative profile",
		"allocation target",
		"one unverified source",
		"say what to check, not what to do",
		"alert only",
	} {
		if !strings.Contains(proactiveContract, want) {
			t.Errorf("the proactive contract lacks %q", want)
		}
	}
	if n := len(proactiveContract); n > 1600 {
		t.Errorf("the contract is %d bytes; it is sent on every wake-up", n)
	}
}
