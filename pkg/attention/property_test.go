package attention

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// TestDecide_NoNotifyInQuietHoursUnlessProtectiveCritical is the acceptance
// property: over a simulated week of pseudo-random candidates (fixed seed), no
// decision reaches the owner (notify or escalate) while the owner's clock is
// outside 07:00-22:00 Bangkok, unless the candidate is protective and Critical.
// It also checks the daily cap, that protective Criticals are never silenced by
// anything but dedup, that every inhibited decision names a delivered inhibitor
// of the right kind, asset and age, and that every decision left a row.
func TestDecide_NoNotifyInQuietHoursUnlessProtectiveCritical(t *testing.T) {
	const (
		n      = 1000
		budget = 3
	)
	e := newEnv(t)
	rng := rand.New(rand.NewSource(20260310))
	ctx := context.Background()

	e.clock.Set(bkkAt(-1, 0, 0)) // Monday 00:00 Bangkok
	keys := []string{"drawdown:portfolio", "dn:1:funding", "dn:2:margin", "price:btc", "price:eth",
		"liq:okx:btc", "concentration:eth", "news:fed", "stale:okx", "pnl:daily", "basis:sol", "rebalance",
		"price_cross:btc", "price_cross:eth", "liquidation:okx:btc", "move:btc", "move:eth", "move:sol"}
	subjects := []string{"BTC/USDT", "ETH/USDT", "BTC/USDT:USDT", "SOL/USDT", "portfolio"}

	// independent re-statement of the rule under test: the Bangkok wall clock.
	quiet := func(ts time.Time) bool {
		m := ts.In(bkk).Hour()*60 + ts.In(bkk).Minute()
		return m < 7*60 || m >= 22*60
	}

	type outcome struct {
		d         Decision
		delivered bool
		at        time.Time
	}
	var all []outcome
	inhibited := 0
	ids := map[string]bool{}
	counts := map[proactive.Disposition]int{}
	quietSeen, protectiveQuiet := 0, 0

	for i := 0; i < n; i++ {
		e.clock.Add(time.Duration(1+rng.Intn(20)) * time.Minute)
		now := e.clock.Now()

		c := cand(keys[rng.Intn(len(keys))], proactive.Severity(rng.Intn(4)))
		c.Subject = subjects[rng.Intn(len(subjects))]
		if strings.HasPrefix(c.DedupKey, "price_cross:") || strings.HasPrefix(c.DedupKey, "move:") ||
			strings.HasPrefix(c.DedupKey, "liquidation:") || strings.HasPrefix(c.DedupKey, "dn:") {
			c.DedupKey += ":" + strconv.Itoa(rng.Intn(8)) // many situations, so dedup does not hide the rule
		}
		c.TriggerID = "trig:" + c.DedupKey
		c.Protective = rng.Intn(4) == 0
		c.Shadow = rng.Intn(30) == 0
		if rng.Intn(3) == 0 {
			c.Compose = proactive.ComposeLLM
		}
		if rng.Intn(5) == 0 {
			c.DedupWindow = time.Duration(30+rng.Intn(90)) * time.Minute
		}

		d, err := e.g.Decide(ctx, c)
		if err != nil {
			t.Fatalf("decision %d: %v", i, err)
		}
		counts[d.Disposition]++
		o := outcome{d: d, at: now}
		sends := d.Disposition == proactive.DispNotify || d.Disposition == proactive.DispEscalate
		if sends && rng.Intn(5) != 0 { // one send in five fails
			e.mark(d, true)
			o.delivered = true
		} else if sends {
			e.mark(d, false)
		}
		all = append(all, o)
		ids[d.ID] = true

		if by, ok := strings.CutPrefix(d.Reason, ReasonInhibitedPrefix); ok {
			inhibited++
			if d.Disposition != proactive.DispDrop {
				t.Fatalf("decision %d: inhibited but %s", i, d.Disposition)
			}
			if c.Protective && c.Severity == critical {
				t.Fatalf("decision %d: a protective critical was inhibited by %s", i, by)
			}
			// independent restatement: some earlier DELIVERED message with that key
			// and an inhibiting prefix, of the right asset and within the rule's age.
			found := false
			for _, prev := range all[:len(all)-1] {
				pc := prev.d.Candidate
				if !prev.delivered || pc.DedupKey != by {
					continue
				}
				for _, r := range defaultInhibits {
					if strings.HasPrefix(by, r.WhenPrefix) && strings.HasPrefix(c.DedupKey, r.SuppressPrefix) &&
						now.Sub(prev.at) < r.For && (!r.SameBaseAsset || (baseAsset(pc) != "" && baseAsset(pc) == baseAsset(c))) {
						found = true
					}
				}
			}
			if !found {
				t.Fatalf("decision %d (%s %s) inhibited by %q, which no delivered message explains", i, c.DedupKey, c.Subject, by)
			}
		}

		protectiveCritical := c.Protective && c.Severity == critical
		if quiet(now) {
			quietSeen++
			if protectiveCritical && !c.Shadow {
				protectiveQuiet++
			}
			if sends && !protectiveCritical {
				t.Fatalf("decision %d (%s sev=%s protective=%v) reached the owner at %s Bangkok: %s/%s",
					i, c.DedupKey, c.Severity, c.Protective, now.In(bkk).Format("Mon 15:04"), d.Disposition, d.Reason)
			}
		}
		// a protective Critical is only ever held back by dedup or shadow mode.
		if protectiveCritical && !c.Shadow && !sends && d.Reason != ReasonDedupSame {
			t.Fatalf("decision %d: protective critical got %s/%s", i, d.Disposition, d.Reason)
		}
		if c.Shadow && d.Disposition != proactive.DispLog {
			t.Fatalf("decision %d: shadow candidate got %s", i, d.Disposition)
		}
	}

	// the cap: per local day, delivered non-critical, non-protective sends never
	// exceed budget + 1.
	perDay := map[string]int{}
	for _, o := range all {
		if o.delivered && o.d.Reason != ReasonProtective && o.d.Candidate.Severity < critical {
			day := o.at.In(bkk).Format("2006-01-02")
			perDay[day]++
			if perDay[day] > budget+1 {
				t.Fatalf("%d non-critical sends on %s, cap is %d", perDay[day], day, budget+1)
			}
		}
	}

	// every decision has a row, and the row says what the decision said. The one
	// exception is shadow coalescing, where repeats share the first row's ID.
	if got := e.count("SELECT COUNT(*) FROM attention_decisions"); got != len(ids) {
		t.Fatalf("rows = %d, want %d (one per distinct decision)", got, len(ids))
	}
	if coalesced := n - len(ids); coalesced < 0 || coalesced > counts[proactive.DispLog] {
		t.Fatalf("%d decisions share an ID but only %d were shadow", coalesced, counts[proactive.DispLog])
	}
	for _, o := range all {
		var disp, reason string
		if err := e.db.QueryRow(`SELECT disposition, reason FROM attention_decisions WHERE id = ?`, o.d.ID).Scan(&disp, &reason); err != nil {
			t.Fatalf("decision %s has no row: %v", o.d.ID, err)
		}
		if disp != string(o.d.Disposition) || reason != o.d.Reason {
			t.Fatalf("row %s = %s/%s, decision was %s/%s", o.d.ID, disp, reason, o.d.Disposition, o.d.Reason)
		}
	}

	// the run must exercise what it claims to: every disposition occurs, the
	// week spans quiet hours, and protective alerts fired inside them.
	for _, d := range []proactive.Disposition{proactive.DispDrop, proactive.DispLog, proactive.DispDigest, proactive.DispNotify, proactive.DispEscalate} {
		if counts[d] == 0 {
			t.Errorf("seed never produced a %s decision (counts %v)", d, counts)
		}
	}
	if inhibited == 0 {
		t.Errorf("seed never produced an inhibited decision (counts %v)", counts)
	}
	if quietSeen < n/4 || protectiveQuiet == 0 {
		t.Errorf("weak run: %d decisions in quiet hours, %d protective criticals among them", quietSeen, protectiveQuiet)
	}
	if span := e.clock.Now().Sub(bkkAt(-1, 0, 0)); span < 6*24*time.Hour {
		t.Errorf("simulated span %v is less than a week", span)
	}
}
