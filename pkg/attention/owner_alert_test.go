package attention

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func ownerRequested(c *proactive.Candidate) { c.OwnerRequested = true }

// atNight puts the fake clock in the owner's quiet hours (03:00 Bangkok).
func (e *env) atNight() { e.clock.Set(bkkAt(1, 3, 0)) }

func TestOwnerAlert_DeliveredInQuietHours(t *testing.T) {
	t.Run("marked notice goes out, with the reason on the row", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		d := e.decide(cand("price_cross:b:btc:above:70k", notice, ownerRequested))
		expect(t, d, proactive.DispNotify, ReasonOwnerAlertQuiet)
		if n := e.count(`SELECT COUNT(*) FROM attention_decisions WHERE id = ? AND disposition = 'notify' AND reason = ?`,
			d.ID, ReasonOwnerAlertQuiet); n != 1 {
			t.Fatalf("the decision row does not carry %q", ReasonOwnerAlertQuiet)
		}
		if n := e.count(`SELECT COUNT(*) FROM attention_digest_queue`); n != 0 {
			t.Fatalf("a delivered owner alert was queued for the digest (%d rows)", n)
		}
	})
	t.Run("an unmarked notice is still held for the digest", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.decide(cand("price_cross:b:btc:above:70k", notice)), proactive.DispDigest, ReasonQuietHours)
	})
	t.Run("in active hours the reason is the ordinary one", func(t *testing.T) {
		e := newEnv(t)
		expect(t, e.decide(cand("a", notice, ownerRequested)), proactive.DispNotify, ReasonTemplate)
	})
	t.Run("a composed one escalates and says why it came at night", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.decide(cand("a", notice, ownerRequested, composeLLM)), proactive.DispEscalate, ReasonOwnerAlertQuiet)
	})
	t.Run("the mark survives the stored candidate", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		d := e.send(cand("a", notice, ownerRequested))
		var js string
		if err := e.db.QueryRow(`SELECT candidate_json FROM attention_decisions WHERE id = ?`, d.ID).Scan(&js); err != nil {
			t.Fatal(err)
		}
		got, ok := decodeCandidate(js)
		if !ok || !got.OwnerRequested {
			t.Fatalf("stored candidate lost the mark: ok=%v %s", ok, js)
		}
		// and the state row a later "previous" is read from
		e.clock.Add(time.Minute)
		d2 := e.decide(cand("a", warning, ownerRequested))
		if d2.Previous == nil || !d2.Previous.OwnerRequested {
			t.Fatalf("Previous = %+v, want the marked candidate", d2.Previous)
		}
	})
}

func TestOwnerAlert_EveryOtherRuleStillApplies(t *testing.T) {
	ctx := context.Background()

	t.Run("muted", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		if err := e.g.Mute(ctx, "price_cross:", e.clock.Now().Add(time.Hour), "snooze"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("price_cross:b:btc:above:70k", notice, ownerRequested)), proactive.DispDrop, ReasonMuted)
	})
	t.Run("repeat inside the dedup window", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.send(cand("price_cross:b:btc:above:70k", notice, ownerRequested)), proactive.DispNotify, ReasonOwnerAlertQuiet)
		e.clock.Add(10 * time.Minute)
		expect(t, e.decide(cand("price_cross:b:btc:above:70k", notice, ownerRequested)), proactive.DispDrop, ReasonDedupSame)
	})
	t.Run("inhibited by a delta-neutral row", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		e.send(cand("dn:7:margin", critical, protective, onSubject("BTC/USDT:USDT")))
		d := e.decide(cand("price_cross:b:btc:above:70k", notice, ownerRequested, onSubject("BTC/USDT")))
		expect(t, d, proactive.DispDrop, inhibitedBy("dn:7:margin"))
	})
	t.Run("inhibited by a liquidation row", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		e.send(cand("liquidation_distance:b:btc:tier1", critical, protective, onSubject("BTC/USDT:USDT")))
		d := e.decide(cand("move_vs_vol:btc:up", notice, ownerRequested, onSubject("BTC/USDT")))
		expect(t, d, proactive.DispDrop, inhibitedBy("liquidation_distance:b:btc:tier1"))
	})
	t.Run("shadow only logs", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.decide(cand("a", notice, ownerRequested, func(c *proactive.Candidate) { c.Shadow = true })), proactive.DispLog, ReasonShadow)
		em := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		em.atNight()
		expect(t, em.decide(cand("a", notice, ownerRequested)), proactive.DispLog, ReasonShadow)
	})
	t.Run("over the daily budget it is held for the digest", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
		e.atNight()
		expect(t, e.send(cand("a", notice, ownerRequested)), proactive.DispNotify, ReasonOwnerAlertQuiet)
		expect(t, e.decide(cand("b", notice, ownerRequested)), proactive.DispDigest, ReasonBudget)
	})
	t.Run("info is still digest material", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.decide(cand("a", info, ownerRequested)), proactive.DispDigest, ReasonInfoToDigest)
	})
	t.Run("a queued digest item of the same situation still dedups it", func(t *testing.T) {
		e := newEnv(t)
		e.atNight()
		expect(t, e.decide(cand("a", notice)), proactive.DispDigest, ReasonQuietHours)
		expect(t, e.decide(cand("a", notice, ownerRequested)), proactive.DispDrop, ReasonDedupQueued)
	})
}

// Only rule 6 (quiet hours) looks at the mark: for every other set of facts a marked
// candidate gets the same disposition as an unmarked one. The one reason that differs
// is the ordinary delivery reason, which says it came at night.
func TestOwnerAlert_OnlyQuietHoursLooksAtTheMark(t *testing.T) {
	for _, f := range []facts{
		{Mode: modeLive, InActiveHours: false, Budget: 3},
		{Mode: modeLive, InActiveHours: true, Budget: 3},
		{Mode: modeLive, InActiveHours: true, Muted: true},
		{Mode: modeLive, DedupHit: true},
		{Mode: modeLive, QueuedHit: true},
		{Mode: modeLive, InhibitedBy: "dn:1:x"},
		{Mode: modeLive, InActiveHours: true, SendsToday: 3, Budget: 3},
		{Mode: modeLive, InActiveHours: false, SendsToday: 3, Budget: 3},
		{Mode: modeShadow},
		{Mode: modeOff},
	} {
		for _, sev := range []proactive.Severity{info, notice, warning, critical} {
			plain := proactive.Candidate{Severity: sev, DedupKey: "k"}
			marked := plain
			marked.OwnerRequested = true
			pd, pr := evaluate(plain, f)
			md, mr := evaluate(marked, f)
			switch {
			case pr == ReasonQuietHours:
				// the one rule the mark skips: what follows it is judged as in active hours.
				af := f
				af.InActiveHours = true
				wd, wr := evaluate(plain, af)
				if wr == ReasonTemplate || wr == ReasonComposeLLM {
					wr = ReasonOwnerAlertQuiet
				}
				if md != wd || mr != wr {
					t.Errorf("%+v sev %s: marked = %s/%s, want %s/%s", f, sev, md, mr, wd, wr)
				}
			case pd != md || pr != mr:
				t.Errorf("%+v sev %s: unmarked %s/%s, marked %s/%s", f, sev, pd, pr, md, mr)
			}
		}
	}
}

func TestOwnerAlert_MarkIsPartOfTheStoredCandidate(t *testing.T) {
	b, err := json.Marshal(proactive.Candidate{OwnerRequested: true})
	if err != nil {
		t.Fatal(err)
	}
	var back proactive.Candidate
	if err := json.Unmarshal(b, &back); err != nil || !back.OwnerRequested {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}
