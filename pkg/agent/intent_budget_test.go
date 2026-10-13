package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// The intents digest is part of every prompt's financial context while the ledger
// is on, so it is part of the first-call budgets of proactive turns. These tests
// measure those turns with the section at its cap, on the same default workspace
// and the same budgets as proactive_first_call_test.go, without raising any of
// them.

const (
	digestStart = "Intents (active, set by the owner):"
	digestEnd   = "not active: 20 (list_intents shows them)"
)

func intentsOnConfig(cfg *config.Config) {
	cfg.Proactive.Enabled = true
	cfg.Proactive.Intents.Enabled = true
}

// intentsEmptyFixture turns the ledger on and leaves it empty.
func intentsEmptyFixture() budgetFixture {
	return budgetFixture{
		config: intentsOnConfig,
		seed:   func(t *testing.T, _ string) { t.Cleanup(func() { _ = sqlitex.CloseShared() }) },
	}
}

// intentsFullFixture turns the ledger on and fills it: far more active intents
// than the digest can show, each with a title at the limit, and the proposal cap
// of suggestions. The digest is then at its cap in every prompt mode.
func intentsFullFixture() budgetFixture {
	fx := intentsEmptyFixture()
	fx.after = func(t *testing.T, ws string) {
		store := profileStoreFor(t, ws)
		ctx := context.Background()
		owner := intent.Provenance{SenderID: profileOwner, Trust: proactive.TrustUserAuthored, TurnKind: proactive.TurnUser}
		model := intent.Provenance{Trust: proactive.TrustUntrusted, TurnKind: proactive.TurnProactive}
		types := []intent.IntentType{intent.TypeGoal, intent.TypeStandingOrder, intent.TypeWatch}
		for i := 0; i < 16; i++ {
			title := fmt.Sprintf("%02d keep the long term plan steady and review it every month with care", i)
			title += strings.Repeat(" more", (intent.MaxTitleRunes-len(title))/5)
			if _, err := store.CreateIntent(ctx, intent.Intent{Type: types[i%3], Title: title,
				Status: intent.StatusActive, Source: intent.SourceExplicit, Provenance: owner}); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < intent.MaxProposedIntents; i++ {
			if _, err := store.CreateIntent(ctx, intent.Intent{Type: intent.TypeGoal, Title: fmt.Sprintf("suggestion %d", i), Provenance: model}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fx
}

// portfolioFixture saves an automatic snapshot of five ordinary holdings, so the
// portfolio section of the financial context is populated.
func portfolioFixture() budgetFixture {
	return budgetFixture{seed: func(t *testing.T, ws string) {
		store, err := snapshot.NewStore(ws)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		pos := []snapshot.Position{
			{Source: "binance", Asset: "BTC", Quantity: 0.1523, Quote: "USDT", Value: 9120.5},
			{Source: "binance", Asset: "ETH", Quantity: 2.41, Quote: "USDT", Value: 7210.2},
			{Source: "okx", Asset: "SOL", Quantity: 31.2, Quote: "USDT", Value: 4310.9},
			{Source: "bitkub", Asset: "BNB", Quantity: 5.5, Quote: "USDT", Value: 3300.1},
			{Source: "bitkub", Asset: "USDT", Quantity: 2500, Quote: "USDT", Value: 2500},
		}
		if _, err := store.SaveAutoSnapshot(context.Background(), &snapshot.Snapshot{
			TakenAt: time.Now(), Quote: "USDT", TotalValue: 26441.7, Note: snapshot.AutoNote, Positions: pos,
		}); err != nil {
			t.Fatal(err)
		}
	}}
}

// merge combines fixtures into one (config edits and seeds run in order).
func merge(fxs ...budgetFixture) budgetFixture {
	return budgetFixture{
		config: func(c *config.Config) {
			for _, f := range fxs {
				if f.config != nil {
					f.config(c)
				}
			}
		},
		seed: func(t *testing.T, ws string) {
			for _, f := range fxs {
				if f.seed != nil {
					f.seed(t, ws)
				}
			}
		},
		after: func(t *testing.T, ws string) {
			for _, f := range fxs {
				if f.after != nil {
					f.after(t, ws)
				}
			}
		},
	}
}

// measureWith runs a proactive turn on the default workspace under fixtures and
// returns its first call and the intents section it carried ("" if none).
func measureWith(t *testing.T, req ProactiveTurnRequest, fixtures ...budgetFixture) (firstCall, string) {
	t.Helper()
	prov := script(respondResponse(map[string]any{"decision": "silent"}))
	al, _ := defaultWorkspaceLoopWith(t, prov, fixtures...)
	registerGatewayReadTools(al)
	mustRun(t, al, req)
	fc := measureFirstCall(t, prov)
	prov.mu.Lock()
	defer prov.mu.Unlock()
	return fc, digestIn(prov.msgs[0])
}

func digestIn(msgs []providers.Message) string {
	for _, m := range msgs {
		if m.Role != "system" {
			continue
		}
		if i := strings.Index(m.Content, digestStart); i >= 0 {
			rest := m.Content[i:]
			if j := strings.Index(rest, digestEnd); j >= 0 {
				return rest[:j+len(digestEnd)]
			}
			return rest
		}
	}
	return ""
}

// atCap asserts the section really is as full as it can be: within one line of the
// cap (so one more title would not have fitted), and not over it.
func atCap(t *testing.T, section string, limit int) {
	t.Helper()
	if len(section) > limit || len(section) < limit-170 {
		t.Fatalf("the digest is %d bytes; the test must be at the cap (%d, at most one line below)", len(section), limit)
	}
	if !strings.Contains(section, "... and ") {
		t.Fatalf("the digest does not say it left titles out; the ledger is not fuller than the cap:\n%s", section)
	}
}

func TestIntentsBudget_GovernedHeartbeatWithFullDigest(t *testing.T) {
	base, none := measureWith(t, governedHeartbeatRequest())
	empty, _ := measureWith(t, governedHeartbeatRequest(), intentsEmptyFixture())
	fc, section := measureWith(t, governedHeartbeatRequest(), intentsFullFixture())
	atCap(t, section, intentDigestCompactCap)
	t.Logf("governed heartbeat first call: default config %d bytes; ledger on, empty %d; ledger on, digest full (%d bytes) %d (budget %d)",
		base.total, empty.total, len(section), fc.total, governedHeartbeatFirstCallBudget)
	if none != "" {
		t.Fatal("the default config carries a digest")
	}
	if empty.total != base.total {
		t.Errorf("an empty ledger changed the first call: %d -> %d bytes", base.total, empty.total)
	}
	if fc.total > governedHeartbeatFirstCallBudget {
		t.Errorf("a governed heartbeat with a full digest is %d bytes, budget %d\n%s", fc.total, governedHeartbeatFirstCallBudget, fc)
	}
}

func TestIntentsBudget_CandidateTurnWithFullDigest(t *testing.T) {
	base, _ := measureWith(t, candidateTurnRequest())
	fc, section := measureWith(t, candidateTurnRequest(), intentsFullFixture())
	atCap(t, section, intentDigestCompactCap)
	t.Logf("candidate-driven first call: default config %d bytes; ledger on, digest full (%d bytes) %d (budget %d)",
		base.total, len(section), fc.total, candidateTurnFirstCallBudget)
	if fc.total > candidateTurnFirstCallBudget {
		t.Errorf("a candidate-driven turn with a full digest is %d bytes, budget %d\n%s", fc.total, candidateTurnFirstCallBudget, fc)
	}
}

func TestIntentsBudget_SweepWithFullDigest(t *testing.T) {
	base, _, _ := firstCallOfSweep(t, heartbeat.TemplateV2(), true)
	fc, _, al := firstCallOfSweep(t, heartbeat.TemplateV2(), true, intentsFullFixture())
	c := &intentsContributor{store: profileStoreFor(t, DefaultAgentWorkspace(al.GetConfig()))}
	section, err := c.LiveSection(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	atCap(t, section, intentDigestCompactCap)
	t.Logf("sweep first call: default config %d bytes; ledger on, digest full (%d bytes) %d (budget %d, %d spare)",
		base.total, len(section), fc.total, governedHeartbeatFirstCallBudget, governedHeartbeatFirstCallBudget-fc.total)
	if fc.total > governedHeartbeatFirstCallBudget {
		t.Errorf("a sweep with a full digest is %d bytes, budget %d\n%s", fc.total, governedHeartbeatFirstCallBudget, fc)
	}
}

// None of the budget tests above has a populated portfolio section; this one has
// the realistic one (a header and five ordinary rows) AND the full digest.
func TestIntentsBudget_SweepWithPortfolioAndFullDigest(t *testing.T) {
	withPortfolio, _, _ := firstCallOfSweep(t, heartbeat.TemplateV2(), true, portfolioFixture())
	fc, _, _ := firstCallOfSweep(t, heartbeat.TemplateV2(), true, merge(portfolioFixture(), intentsFullFixture()))
	t.Logf("sweep first call with a five-row portfolio section: %d bytes; with the full digest as well: %d bytes (budget %d, %d spare)",
		withPortfolio.total, fc.total, governedHeartbeatFirstCallBudget, governedHeartbeatFirstCallBudget-fc.total)
	if fc.total > governedHeartbeatFirstCallBudget {
		t.Errorf("a sweep with a portfolio section and a full digest is %d bytes, budget %d\n%s",
			fc.total, governedHeartbeatFirstCallBudget, fc)
	}
}

// A chat turn's cap is the larger one, and holds as well.
func TestIntentsDigest_ChatTurnCap(t *testing.T) {
	provider := &charScriptedProvider{}
	al, workspace := newIntentLoop(t, provider, true)
	_ = workspace
	fx := intentsFullFixture()
	fx.after(t, workspace)
	cb := al.GetRegistry().GetDefaultAgent().ContextBuilder

	for _, tc := range []struct {
		mode  PromptMode
		limit int
	}{{PromptFull, intentDigestCap}, {PromptLight, intentDigestCompactCap}, {PromptProactive, intentDigestCompactCap}} {
		msgs := cb.BuildMessagesWithOptions(nil, "", "hi", nil, "telegram", "c1", BuildOptions{Mode: tc.mode})
		section := digestIn(msgs)
		if section == "" || len(section) > tc.limit || len(section) < tc.limit-170 {
			t.Errorf("mode %d: digest is %d bytes, want just under %d:\n%s", tc.mode, len(section), tc.limit, section)
		}
	}
}
