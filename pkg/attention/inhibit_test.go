package attention

import (
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func onSubject(s string) func(*proactive.Candidate) {
	return func(c *proactive.Candidate) { c.Subject = s }
}

func withEvidence(k string, v any) func(*proactive.Candidate) {
	return func(c *proactive.Candidate) {
		c.Evidence = map[string]any{k: v}
	}
}

func inhibitedBy(key string) string { return ReasonInhibitedPrefix + key }

func TestInhibit_DNSuppressesSameAssetPriceAlert(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("BTC/USDT:USDT")))

	d := e.decide(cand("price_cross:btc:70k", notice, onSubject("BTC/USDT")))
	expect(t, d, proactive.DispDrop, inhibitedBy("dn:7:margin"))

	// the drop is recorded like any other decision, and is not queued for the digest.
	if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'drop' AND reason = ?`,
		d.ID, inhibitedBy("dn:7:margin")); n != 1 {
		t.Fatalf("no inhibited row for %s", d.ID)
	}
	if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
		t.Fatalf("an inhibited candidate was queued for the digest (%d rows)", n)
	}

	// a different asset is a different story.
	expect(t, e.decide(cand("price_cross:eth:4k", notice, onSubject("ETH/USDT"))), proactive.DispNotify, ReasonTemplate)
	// a key that does not match the suppressed prefix is untouched.
	expect(t, e.decide(cand("price:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_InFlightInhibitorCounts(t *testing.T) {
	e := newEnv(t)
	e.decide(cand("dn:7:margin", warning, onSubject("BTC/USDT"))) // not reported yet
	expect(t, e.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("dn:7:margin"))

	// the reservation lapses after the lease.
	e.clock.Add(pendingLease + time.Minute)
	expect(t, e.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_FailedSendDoesNotInhibit(t *testing.T) {
	e := newEnv(t)
	d := e.decide(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	e.mark(d, false) // delivered = 0, delivered_at_ms set

	expect(t, e.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)

	// a failed attempt retried successfully does inhibit.
	e.mark(d, true)
	expect(t, e.decide(cand("price_cross:btc:2", notice, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("dn:7:margin"))
}

func TestInhibit_OnlySentDecisionsInhibit(t *testing.T) {
	e := newEnv(t)
	// a digest item, a drop and a shadow row about dn: are not messages the owner got.
	e.clock.Set(bkkAt(0, 23, 0))
	expect(t, e.decide(cand("dn:1:margin", warning, onSubject("BTC/USDT"))), proactive.DispDigest, ReasonQuietHours)
	e.clock.Set(bkkAt(0, 12, 0))
	expect(t, e.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_ProtectiveCriticalIsNeverInhibited(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	e.send(cand("liquidation:okx:btc", warning, onSubject("BTC/USDT:USDT")))

	// a protective Critical under a suppressed prefix gets through, in active
	// hours and in quiet hours.
	expect(t, e.decide(cand("price_cross:btc", critical, protective, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonProtective)
	e.clock.Set(bkkAt(0, 3, 0))
	expect(t, e.decide(cand("move:btc", critical, protective, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonProtective)
	// a Critical that is not protective is inhibited like any other.
	e.clock.Set(bkkAt(0, 12, 5))
	expect(t, e.decide(cand("move:btc:2", critical, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("liquidation:okx:btc"))
}

func TestInhibit_ExpiresAfterFor(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	e.clock.Add(59 * time.Minute)
	expect(t, e.decide(cand("price_cross:a", notice, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("dn:7:margin"))
	e.clock.Add(2 * time.Minute) // 61 minutes after the inhibitor
	expect(t, e.decide(cand("price_cross:b", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_DrawdownSuppressesAnyAssetMoveFor30Minutes(t *testing.T) {
	e := newEnv(t)
	e.send(cand("drawdown:portfolio:tier2", warning, onSubject("portfolio")))
	d := e.decide(cand("move:sol", notice, onSubject("SOL/USDT")))
	expect(t, d, proactive.DispDrop, inhibitedBy("drawdown:portfolio:tier2"))
	// a subject with no asset at all is covered too, because the rule does not
	// need a base asset.
	expect(t, e.decide(cand("move:market", notice, onSubject("portfolio"))), proactive.DispDrop, inhibitedBy("drawdown:portfolio:tier2"))
	// price_cross is not in this rule.
	expect(t, e.decide(cand("price_cross:sol", notice, onSubject("SOL/USDT"))), proactive.DispNotify, ReasonTemplate)

	e.clock.Add(31 * time.Minute)
	expect(t, e.decide(cand("move:sol:2", notice, onSubject("SOL/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_LiquidationSuppressesSameAssetMove(t *testing.T) {
	e := newEnv(t)
	e.send(cand("liquidation:okx:btc", critical, protective, onSubject("BTC/USDT:USDT")))
	expect(t, e.decide(cand("move:btc", notice, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("liquidation:okx:btc"))
	expect(t, e.decide(cand("move:eth", notice, onSubject("ETH/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_NoBaseAssetNoMatch(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	// the candidate names no asset: a same-asset rule cannot match.
	expect(t, e.decide(cand("price_cross:x", notice, onSubject("portfolio"))), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("price_cross:y", notice, onSubject("plan:12"))), proactive.DispNotify, ReasonTemplate)

	// the inhibitor names no asset: nothing to match against.
	e2 := newEnv(t)
	e2.send(cand("dn:8:margin", warning, onSubject("plan:8")))
	expect(t, e2.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_EvidenceBaseAssetWins(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("plan:7"), withEvidence("base_asset", " btc ")))
	expect(t, e.decide(cand("price_cross:btc", notice, onSubject("portfolio"), withEvidence("base_asset", "BTC"))),
		proactive.DispDrop, inhibitedBy("dn:7:margin"))
	// evidence says ETH although the subject says BTC: evidence wins.
	expect(t, e.decide(cand("price_cross:eth", notice, onSubject("BTC/USDT"), withEvidence("base_asset", "ETH"))),
		proactive.DispNotify, ReasonTemplate)
	// stored upper-case.
	var base string
	if err := e.db.QueryRow(`SELECT base_asset FROM attention_decisions WHERE dedup_key = 'dn:7:margin'`).Scan(&base); err != nil || base != "BTC" {
		t.Fatalf("stored base_asset = %q, %v", base, err)
	}
}

func TestInhibit_ShadowModeIsUnaffected(t *testing.T) {
	cfg := liveCfg()
	e := newEnvMode(t, config.ProactiveModeShadow, cfg)
	d1 := e.decide(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	d2 := e.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT")))
	expect(t, d1, proactive.DispLog, ReasonShadow)
	expect(t, d2, proactive.DispLog, ReasonShadow) // shadow, not inhibited_by

	// a shadow candidate in live mode is not inhibited either, and does not inhibit.
	live := newEnv(t)
	live.send(cand("dn:7:margin", warning, onSubject("BTC/USDT")))
	s := cand("price_cross:btc", notice, onSubject("BTC/USDT"))
	s.Shadow = true
	expect(t, live.decide(s), proactive.DispLog, ReasonShadow)
	live2 := newEnv(t)
	sh := cand("dn:7:margin", warning, onSubject("BTC/USDT"))
	sh.Shadow = true
	live2.decide(sh)
	expect(t, live2.decide(cand("price_cross:btc", notice, onSubject("BTC/USDT"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_BeatsQuietHoursAndBudgetButNotDedupOrMute(t *testing.T) {
	e := newEnv(t)
	e.send(cand("dn:7:margin", warning, onSubject("BTC/USDT")))

	// quiet hours: dropped, not digested.
	e.clock.Set(bkkAt(0, 12, 30))
	e.prof.set(func(p *intent.Profile) { p.ActiveHours = intent.HoursRange{Start: "13:00", End: "14:00"} })
	expect(t, e.decide(cand("price_cross:q", notice, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("dn:7:margin"))

	// an Info candidate would be a digest item; the inhibitor drops it too.
	expect(t, e.decide(cand("price_cross:i", info, onSubject("BTC/USDT"))), proactive.DispDrop, inhibitedBy("dn:7:margin"))

	// a mute and dedup come first in the table.
	if err := e.g.Mute(t.Context(), "price_cross:m", e.clock.Now().Add(time.Hour), "test"); err != nil {
		t.Fatal(err)
	}
	expect(t, e.decide(cand("price_cross:m", notice, onSubject("BTC/USDT"))), proactive.DispDrop, ReasonMuted)
}

func TestInhibit_CustomRuleTable(t *testing.T) {
	e := newEnv(t)
	e.g.inhibits = []InhibitRule{{WhenPrefix: "a:", SuppressPrefix: "b:", SameBaseAsset: false, For: time.Minute}}
	e.send(cand("a:1", notice, onSubject("portfolio")))
	expect(t, e.decide(cand("b:1", notice, onSubject("portfolio"))), proactive.DispDrop, inhibitedBy("a:1"))
	e.clock.Add(2 * time.Minute)
	expect(t, e.decide(cand("b:2", notice, onSubject("portfolio"))), proactive.DispNotify, ReasonTemplate)

	// a rule with an empty prefix never matches everything.
	e.g.inhibits = []InhibitRule{{WhenPrefix: "", SuppressPrefix: "b:", For: time.Hour}, {WhenPrefix: "a:", SuppressPrefix: "", For: time.Hour}}
	expect(t, e.decide(cand("b:3", notice, onSubject("portfolio"))), proactive.DispNotify, ReasonTemplate)
}

func TestInhibit_PrefixMatchIsLiteral(t *testing.T) {
	e := newEnv(t)
	e.g.inhibits = []InhibitRule{{WhenPrefix: "x_%", SuppressPrefix: "y:", For: time.Hour}}
	e.send(cand("xa", notice, onSubject("portfolio"))) // LIKE 'x_%' would match this
	expect(t, e.decide(cand("y:1", notice, onSubject("portfolio"))), proactive.DispNotify, ReasonTemplate)
	e.send(cand("x_%real", notice, onSubject("portfolio")))
	expect(t, e.decide(cand("y:2", notice, onSubject("portfolio"))), proactive.DispDrop, inhibitedBy("x_%real"))
}

func TestBaseAsset(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		evidence any
		want     string
	}{
		{"spot", "BTC/USDT", nil, "BTC"},
		{"swap", "BTC/USDT:USDT", nil, "BTC"},
		{"dash", "BTC-USDT", nil, "BTC"},
		{"okx swap id", "btc-usdt-swap", nil, "BTC"},
		{"thb", "eth/thb", nil, "ETH"},
		{"numeric base", "1000PEPE/USDT", nil, "1000PEPE"},
		{"whitespace", "  SOL/USDC  ", nil, "SOL"},
		{"evidence", "plan:12", "doge", "DOGE"},
		{"evidence beats subject", "BTC/USDT", "ETH", "ETH"},
		{"bad evidence falls back", "BTC/USDT", "BT C!", "BTC"},
		{"non-string evidence falls back", "BTC/USDT", 42, "BTC"},
		{"empty evidence falls back", "BTC/USDT", "  ", "BTC"},
		{"portfolio", "portfolio", nil, ""},
		{"plan", "plan:12", nil, ""},
		{"empty", "", nil, ""},
		{"hyphenated word", "stop-loss", nil, ""},
		{"unknown quote", "BTC/XYZ", nil, ""},
		{"empty base", "/USDT", nil, ""},
		{"three dashes", "a-b-c-d", nil, ""},
		{"bad suffix", "BTC-USDT-FOO", nil, ""},
		{"bad settle", "BTC/USDT:US DT", nil, ""},
		{"base too long", strings.Repeat("A", 21) + "/USDT", nil, ""},
		{"symbol chars", "BT$C/USDT", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cd := proactive.Candidate{Subject: c.subject}
			if c.evidence != nil {
				cd.Evidence = map[string]any{"base_asset": c.evidence}
			}
			if got := baseAsset(cd); got != c.want {
				t.Fatalf("baseAsset(%q, %v) = %q, want %q", c.subject, c.evidence, got, c.want)
			}
		})
	}
}
