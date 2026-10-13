package attention

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// TestInhibit_RowsAreNamed pins the whole inhibition table. Each prefix is a trigger
// kind (pkg/triggers pins the kind names in TestPortfolioKinds_NamesMatchInhibitRows),
// because the registry requires a key to start with its kind: a row naming a prefix no
// kind can carry never matches and silently loses its inhibition. Renaming a kind, or
// editing the table, must change this list too.
func TestInhibit_RowsAreNamed(t *testing.T) {
	type row struct {
		when, suppress string
		sameBase       bool
		forMin         int
	}
	want := []row{
		{"dn:", "price_cross:", true, 60},
		{"dn_health:", "price_cross:", true, 60},
		{"liquidation_distance:", "move_vs_vol:", true, 60},
		{"drawdown:portfolio", "move_vs_vol:", false, 30},
	}
	var got []row
	for _, r := range defaultInhibits {
		got = append(got, row{r.WhenPrefix, r.SuppressPrefix, r.SameBaseAsset, int(r.For.Minutes())})
	}
	if len(got) != len(want) {
		t.Fatalf("inhibit table:\n got %+v\nwant %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The portfolio triggers' dedup keys start with their kind, as the trigger registry
// requires; the rules must name those prefixes, not the ones the spec's table shows.
func TestInhibit_PortfolioTriggerKeys(t *testing.T) {
	asset := func(a string) func(*proactive.Candidate) {
		return func(c *proactive.Candidate) { c.Evidence = map[string]any{"base_asset": a} }
	}
	e := newEnv(t)
	liq := "liquidation_distance:okx/main:BTC/USDT:USDT:long:warning"
	e.send(cand(liq, warning, onSubject("BTC/USDT:USDT"), asset("BTC")))
	expect(t, e.decide(cand("move_vs_vol:BTC:1h:tier1", notice, onSubject("BTC"), asset("BTC"))),
		proactive.DispDrop, inhibitedBy(liq))
	expect(t, e.decide(cand("move_vs_vol:ETH:1h:tier1", notice, onSubject("ETH"), asset("ETH"))), proactive.DispNotify, ReasonTemplate)
	// A protective move is never inhibited.
	expect(t, e.decide(cand("move_vs_vol:BTC:1h:tier3", critical, protective, onSubject("BTC"), asset("BTC"))),
		proactive.DispNotify, ReasonProtective)

	e = newEnv(t)
	e.send(cand("drawdown:portfolio:day:tier2", warning, onSubject("portfolio")))
	expect(t, e.decide(cand("move_vs_vol:SOL:24h:tier1", notice, onSubject("SOL"), asset("SOL"))),
		proactive.DispDrop, inhibitedBy("drawdown:portfolio:day:tier2"))

	// The re-base notice shares the kind but is not the drawdown inhibitor's prefix.
	e = newEnv(t)
	e.send(cand("drawdown:rebase:2026-03-10", notice, onSubject("portfolio")))
	expect(t, e.decide(cand("move_vs_vol:SOL:24h:tier1", notice, onSubject("SOL"), asset("SOL"))), proactive.DispNotify, ReasonTemplate)
}
