package tools

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// ProposalScope carries, for one candidate-driven proactive turn, the facts that
// decide whether the turn may call propose_order. It is created by code where the
// turn starts (agent.RunProactiveTurn) from the turn's candidates, travels in the
// context, and is changed only by code: the tool loop marks it when a tool that
// reads outside text has been called, and propose_order records what it created.
// Nothing a model passes in an argument reaches it.
type ProposalScope struct {
	deliverNow   bool
	candidateID  string
	intervention string

	// taint is the turn's trust floor: it starts at the candidates' minimum and is
	// lowered by the tool loop. WithProposalScope puts the same holder in the
	// context, so the policy engine and propose_order read one definition.
	taint *proactive.Taint

	mu      sync.Mutex
	created map[string]bool
}

// NewProposalScope builds the scope of a turn.
//
//   - trust is the minimum trust of the turn's candidates (nothing means
//     untrusted);
//   - deliverNow says attention decided to deliver the candidates' message now;
//   - tainted starts the turn with its trust already lowered, for a turn whose
//     prompt carries text from outside (forced skills).
func NewProposalScope(candidates []proactive.Candidate, deliverNow bool, interventionID string, tainted bool) *ProposalScope {
	trusts := make([]proactive.Trust, 0, len(candidates))
	first := ""
	for _, c := range candidates {
		trusts = append(trusts, c.Trust)
		if first == "" {
			first = c.ID
		}
	}
	s := &ProposalScope{
		taint: proactive.NewTaint(proactive.MinTrust(trusts...)), deliverNow: deliverNow, candidateID: first,
		intervention: interventionID, created: map[string]bool{},
	}
	if tainted {
		s.taint.Lower(proactive.TrustUntrusted)
	}
	return s
}

// Trust is the turn's trust floor now: the candidates' minimum, dropped to
// untrusted once the turn has called a tool that reads outside text.
func (s *ProposalScope) Trust() proactive.Trust {
	if s == nil {
		return proactive.TrustUntrusted
	}
	return s.taint.Trust()
}

// NoteCreated records the code of a proposal the turn created.
func (s *ProposalScope) NoteCreated(code string) {
	s.mu.Lock()
	s.created[code] = true
	s.mu.Unlock()
}

// Created reports whether the turn created the proposal with this code.
func (s *ProposalScope) Created(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.created[code]
}

type proposalScopeKey struct{}

// WithProposalScope returns a context carrying s and the turn's taint, so that
// NoteToolCalls (which lowers the taint of the context) acts on the scope's trust.
func WithProposalScope(ctx context.Context, s *ProposalScope) context.Context {
	if s != nil {
		ctx = proactive.WithTaint(ctx, s.taint)
	}
	return context.WithValue(ctx, proposalScopeKey{}, s)
}

// ProposalScopeFrom returns the turn's scope, or nil when it has none.
func ProposalScopeFrom(ctx context.Context) *ProposalScope {
	s, _ := ctx.Value(proposalScopeKey{}).(*ProposalScope)
	return s
}

// trustedDataTools are the tools whose results are exchange data, data computed
// by this program from it, or text the owner authored. Reading them does not lower
// a turn's trust. Every other tool does, by default: a tool added later is outside
// until someone decides otherwise here, and TestEveryReadToolIsPlacedForTrust makes
// that decision explicit for the read class.
//
// Left out on purpose, because their results can carry text somebody else wrote:
// web_search and web_fetch (the web), any news tool, read_file and list_dir (files
// an earlier turn may have saved from the web), find_skills and load_skill (skill
// text from a registry), short_grep and short_expand (earlier conversation, which
// may hold fetched pages), list_intents (inferred intents, written by a model that
// may have read a page), the tool-search tools, and every MCP tool.
var trustedDataTools = map[string]bool{
	NameRespondProactive: true, NameProposeOrder: true,

	NameGetAssetsList: true, NameGetTotalValue: true, NameListPortfolios: true, NameQuerySnapshots: true,
	NameSnapshotSummary: true, NameGetPnLSummary: true, NameGetPnLDetail: true, NamePortfolioAllocation: true,
	NameGetTicker: true, NameGetTickers: true, NameGetOHLCV: true, NameGetOrderBook: true, NameGetMarkets: true,
	NameCalculateIndicators: true, NameMarketAnalysis: true, NameFundingRateHistory: true, NameOptionQuote: true,

	NameGetOrder: true, NameGetOpenOrders: true, NameGetOrderHistory: true, NameGetTradeHistory: true,
	NameGetOrderRateStatus: true, NameOptionGetOrder: true, NameOptionOpenOrders: true,
	NameFuturesGetOrder: true, NameFuturesGetPositions: true, NameFuturesGetFunding: true,
	NameFuturesValidateMarket: true, NameFuturesRiskSummary: true, NameFuturesEstimateFundingFee: true,

	NameListDCAPlans: true, NameGetDCAHistory: true, NameGetDCASummary: true, NameListDeltaNeutralPlans: true,
	NameGetDeltaNeutralPlan: true, NameGetDeltaNeutralSummary: true, NameGetDeltaNeutralHistory: true,
	NameGetDeltaNeutralSpread: true, NameGetDeltaNeutralEarn: true, NameScanDeltaNeutralOpportunities: true,
	NameEarnOverview: true, NameGetProfile: true,
}

// readsTrustedData reports whether calling the named tool leaves a turn's trust where it was.
func readsTrustedData(name string) bool { return trustedDataTools[name] }

// NoteToolCalls tells the turn's scope which tools are about to run. It is called
// by the tool loop for a whole batch BEFORE any call of the batch executes (the
// calls of one model response run concurrently, so marking per call would leave the
// outcome to goroutine scheduling), and again by ToolRegistry.ExecuteWithContext
// for the call itself. A tool that is not known to return only trusted data lowers
// the turn's trust for the rest of the turn.
func NoteToolCalls(ctx context.Context, names ...string) {
	t := proactive.TaintFrom(ctx)
	if t == nil {
		return
	}
	for _, n := range names {
		if !readsTrustedData(n) {
			t.Lower(proactive.TrustUntrusted)
			return
		}
	}
}

// armedConfig is the running configuration as far as propose_order's visibility
// goes, set at start and on every reload by SetProposalConfig.
var armedConfig atomic.Pointer[config.Config]

// SetProposalConfig hands the running configuration to propose_order's turn
// check (who the owner is, whether approvals are on). Like SetPolicyConfig it is
// re-armed on every config reload; until it is called the tool is open to no turn.
func SetProposalConfig(cfg *config.Config) { armedConfig.Store(cfg) }

// proposeOrderOpen is the one predicate for "may the turn carried by ctx call
// propose_order". The class ceiling refuses it to every autonomous turn; this
// grants it, by name, to exactly two kinds of turn:
//
//   - the owner's own root conversation turn (checkOwnerTurn, root only);
//   - a candidate-driven proactive turn (TurnProactive, not a heartbeat or a sweep,
//     which keep web_fetch and read outside text) whose candidates were all of at
//     least exchange trust, that attention decided to deliver now, and that has not
//     called a tool reading outside text.
//
// Everything else - cron, heartbeat, sweep, a sub-turn, a system turn, a non-owner,
// an untagged context - gets false, and the same answer hides the tool's definition
// and refuses its call. Approvals off means false for all.
func proposeOrderOpen(ctx context.Context) bool {
	cfg := armedConfig.Load()
	if cfg == nil || !cfg.Proactive.Effective().Approvals {
		return false
	}
	switch origin := ToolOrigin(ctx); origin {
	case OriginProactive:
		if ProactiveTurnKind(ctx) != proactive.TurnProactive || IsSubTurn(ctx) {
			return false
		}
		s := ProposalScopeFrom(ctx)
		return s != nil && s.deliverNow && s.candidateID != "" && s.Trust() >= proactive.TrustExchange
	case OriginCron, OriginHeartbeat:
		return false
	}
	_, v := checkOwnerTurn(ctx, cfg, true)
	return v == ownerYes
}
