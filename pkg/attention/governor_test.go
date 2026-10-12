package attention

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

const (
	notice   = proactive.SeverityNotice
	warning  = proactive.SeverityWarning
	critical = proactive.SeverityCritical
	info     = proactive.SeverityInfo
)

func TestDecide_OffLeavesNoFootprint(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeOff, liveCfg())
	for _, c := range []proactive.Candidate{
		cand("a", notice),
		cand("b", critical, protective),
		cand("c", info, func(c *proactive.Candidate) { c.Shadow = true }),
	} {
		d := e.decide(c)
		expect(t, d, proactive.DispDrop, ReasonOff)
		if d.ID == "" {
			t.Fatal("off decision has no ID")
		}
		if err := e.g.MarkDelivered(context.Background(), d, Delivery{Delivered: true}); err != nil {
			t.Fatalf("MarkDelivered in off mode: %v", err)
		}
	}
	for _, table := range []string{"attention_decisions", "attention_state", "attention_mutes"} {
		if n := e.count("SELECT COUNT(*) FROM " + table); n != 0 {
			t.Errorf("%s has %d rows in off mode, want 0", table, n)
		}
	}
	if n := e.prof.callCount(); n != 0 {
		t.Errorf("profile read %d times in off mode, want 0", n)
	}
}

func TestDecide_ShadowLogsOnly(t *testing.T) {
	t.Run("effective mode shadow", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		d := e.decide(cand("a", critical, protective))
		expect(t, d, proactive.DispLog, ReasonShadow)
		if n := e.count("SELECT COUNT(*) FROM attention_decisions WHERE shadow = 1 AND disposition = 'log'"); n != 1 {
			t.Fatalf("shadow rows = %d, want 1", n)
		}
		// a repeat within the hour is coalesced into the first row.
		again := e.decide(cand("a", critical, protective))
		expect(t, again, proactive.DispLog, ReasonShadow)
		if again.ID != d.ID {
			t.Errorf("coalesced decision ID = %s, want the first row's %s", again.ID, d.ID)
		}
		if n := e.count("SELECT COUNT(*) FROM attention_decisions"); n != 1 {
			t.Fatalf("rows = %d, want 1", n)
		}
		if e.prof.callCount() != 0 {
			t.Error("shadow decision read the profile")
		}
	})
	t.Run("shadow candidate in live mode", func(t *testing.T) {
		e := newEnv(t)
		d := e.decide(cand("a", warning, func(c *proactive.Candidate) { c.Shadow = true }))
		expect(t, d, proactive.DispLog, ReasonShadow)
		if n := e.count("SELECT COUNT(*) FROM attention_state"); n != 0 {
			t.Fatalf("state rows = %d, want 0", n)
		}
		// MarkDelivered refuses a log decision.
		if err := e.g.MarkDelivered(context.Background(), d, Delivery{Delivered: true}); err == nil {
			t.Fatal("MarkDelivered of a log decision succeeded")
		}
	})
}

func TestDecide_Muted(t *testing.T) {
	ctx := context.Background()
	until := func(e *env) time.Time { return e.clock.Now().Add(time.Hour) }

	t.Run("prefix of dedup key and of trigger id", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "dn:", until(e), "snooze"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("dn:12:margin", warning)), proactive.DispDrop, ReasonMuted)
		expect(t, e.decide(cand("other", warning, withTrigger("dn:7"))), proactive.DispDrop, ReasonMuted)
		expect(t, e.decide(cand("price:btc", warning)), proactive.DispNotify, ReasonTemplate)
	})

	t.Run("expired mute is ignored and removed", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "dn:", until(e), "snooze"); err != nil {
			t.Fatal(err)
		}
		e.clock.Add(2 * time.Hour)
		expect(t, e.decide(cand("dn:12", notice)), proactive.DispNotify, ReasonTemplate)
		if n := e.count("SELECT COUNT(*) FROM attention_mutes"); n != 0 {
			t.Fatalf("expired mute still stored (%d rows)", n)
		}
	})

	t.Run("mute is literal, not a LIKE pattern", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "a%", until(e), ""); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("abc", notice)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand("a%x", notice)), proactive.DispDrop, ReasonMuted)
	})

	t.Run("broad mute does not hide a protective critical", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "dn:", until(e), "snooze"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("dn:12:liq", critical, protective)), proactive.DispNotify, ReasonProtective)
		// a protective candidate below Critical is an ordinary candidate to mutes.
		expect(t, e.decide(cand("dn:13:margin", warning, protective)), proactive.DispDrop, ReasonMuted)
		// and a Critical that is not protective is muted by the broad prefix.
		expect(t, e.decide(cand("dn:14:x", critical)), proactive.DispDrop, ReasonMuted)
	})

	t.Run("exact key or trigger mute does hide it", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(ctx, "dn:12:liq", until(e), "owner said so"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("dn:12:liq", critical, protective)), proactive.DispDrop, ReasonMuted)

		if err := e.g.Mute(ctx, "dn:77", until(e), ""); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("k", critical, protective, withTrigger("dn:77"))), proactive.DispDrop, ReasonMuted)
		// Unmute lifts it again.
		if err := e.g.Unmute(ctx, "dn:77"); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("k2", critical, protective, withTrigger("dn:77"))), proactive.DispNotify, ReasonProtective)
	})
}

func TestDecide_DedupSameTierDropped(t *testing.T) {
	e := newEnv(t)
	d := e.send(cand("drawdown", warning))
	expect(t, d, proactive.DispNotify, ReasonTemplate)

	e.clock.Add(time.Hour)
	expect(t, e.decide(cand("drawdown", warning)), proactive.DispDrop, ReasonDedupSame)
	expect(t, e.decide(cand("drawdown", notice)), proactive.DispDrop, ReasonDedupSame)

	e.clock.Add(22*time.Hour + 59*time.Minute) // 23h59m after the send
	expect(t, e.decide(cand("drawdown", warning)), proactive.DispDrop, ReasonDedupSame)

	e.clock.Add(time.Minute) // exactly 24h: the window is over
	expect(t, e.decide(cand("drawdown", warning)), proactive.DispNotify, ReasonTemplate)
}

func TestDecide_HigherTierPasses(t *testing.T) {
	e := newEnv(t)
	first := cand("drawdown", notice, func(c *proactive.Candidate) {
		c.Title = "BTC -8%"
		c.Evidence = map[string]any{"pct": -8.0}
	})
	d1 := e.send(first)
	if d1.Previous != nil {
		t.Fatalf("first decision has Previous %+v", d1.Previous)
	}

	e.clock.Add(time.Hour)
	d2 := e.decide(cand("drawdown", warning, func(c *proactive.Candidate) { c.Title = "BTC -15%" }))
	expect(t, d2, proactive.DispNotify, ReasonTemplate)
	if d2.Previous == nil {
		t.Fatal("Previous is nil on a re-alert")
	}
	if d2.Previous.Title != "BTC -8%" || d2.Previous.Severity != notice {
		t.Fatalf("Previous = %q/%s, want the delivered notice", d2.Previous.Title, d2.Previous.Severity)
	}
	if got, _ := d2.Previous.Evidence["pct"].(float64); got != -8 {
		t.Fatalf("Previous.Evidence[pct] = %v, want -8", d2.Previous.Evidence["pct"])
	}

	// delivering the update moves the baseline: the same tier is now deduped
	// and the next update would cite the -15% message.
	e.mark(d2, true)
	e.clock.Add(time.Hour)
	d3 := e.decide(cand("drawdown", warning))
	expect(t, d3, proactive.DispDrop, ReasonDedupSame)
	if d3.Previous == nil || d3.Previous.Title != "BTC -15%" {
		t.Fatalf("Previous = %+v, want the -15%% message", d3.Previous)
	}
	d4 := e.decide(cand("drawdown", critical))
	expect(t, d4, proactive.DispNotify, ReasonTemplate)
}

func TestDecide_DedupWindowPerCandidate(t *testing.T) {
	e := newEnv(t)
	short := func(c *proactive.Candidate) { c.DedupWindow = time.Hour }
	e.send(cand("fast", warning, short))
	e.send(cand("slow", warning))

	e.clock.Add(30 * time.Minute)
	expect(t, e.decide(cand("fast", warning, short)), proactive.DispDrop, ReasonDedupSame)

	e.clock.Add(31 * time.Minute) // 61 minutes after the sends
	expect(t, e.decide(cand("fast", warning, short)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("slow", warning)), proactive.DispDrop, ReasonDedupSame)

	t.Run("config zero falls back to 24h", func(t *testing.T) {
		cfg := liveCfg()
		cfg.DedupWindowHours = 0
		e := newEnvMode(t, config.ProactiveModeLive, cfg)
		e.send(cand("k", warning))
		e.clock.Add(23 * time.Hour)
		expect(t, e.decide(cand("k", warning)), proactive.DispDrop, ReasonDedupSame)
		e.clock.Add(time.Hour)
		expect(t, e.decide(cand("k", warning)), proactive.DispNotify, ReasonTemplate)
	})

	t.Run("config window is honoured", func(t *testing.T) {
		cfg := liveCfg()
		cfg.DedupWindowHours = 2
		e := newEnvMode(t, config.ProactiveModeLive, cfg)
		e.send(cand("k", warning))
		e.clock.Add(2 * time.Hour)
		expect(t, e.decide(cand("k", warning)), proactive.DispNotify, ReasonTemplate)
	})
}

func TestDecide_DedupQueuedDigest(t *testing.T) {
	e := newEnv(t)
	e.clock.Set(bkkAt(0, 23, 0)) // quiet hours

	d1 := e.decide(cand("drawdown", notice))
	expect(t, d1, proactive.DispDigest, ReasonQuietHours)

	// a candidate that fires every minute overnight queues once, not 60 times.
	for i := 0; i < 5; i++ {
		e.clock.Add(time.Minute)
		expect(t, e.decide(cand("drawdown", notice)), proactive.DispDrop, ReasonDedupQueued)
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions WHERE disposition = 'digest'"); n != 1 {
		t.Fatalf("queued digest rows = %d, want 1", n)
	}

	// a higher tier is news and queues again.
	expect(t, e.decide(cand("drawdown", warning)), proactive.DispDigest, ReasonQuietHours)
	// a different situation is independent.
	expect(t, e.decide(cand("other", notice)), proactive.DispDigest, ReasonQuietHours)
	// a protective critical is not swallowed by a queued item.
	expect(t, e.decide(cand("drawdown", critical, protective)), proactive.DispNotify, ReasonProtective)

	// once the digest item is delivered it is no longer "queued", and the state
	// it left behind dedups the same tier instead.
	e.mark(d1, true)
	e.clock.Add(time.Minute)
	expect(t, e.decide(cand("drawdown", notice)), proactive.DispDrop, ReasonDedupSame)
}

func TestDecide_ProtectiveBypassesQuietHoursAndBudget(t *testing.T) {
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 0 })
	e.clock.Set(bkkAt(0, 3, 0))

	expect(t, e.send(cand("liq:1", critical, protective)), proactive.DispNotify, ReasonProtective)
	expect(t, e.send(cand("liq:2", critical, protective, composeLLM)), proactive.DispEscalate, ReasonProtective)
	// the ordinary candidate at the same time waits.
	expect(t, e.decide(cand("n", notice)), proactive.DispDigest, ReasonQuietHours)

	// protective sends consume no budget: with a budget of one, two protective
	// alerts leave the Notice its slot.
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
	e.clock.Set(bkkAt(0, 12, 0))
	e.send(cand("liq:3", critical, protective))
	e.send(cand("liq:4", critical, protective))
	expect(t, e.send(cand("n2", notice)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("n3", notice)), proactive.DispDigest, ReasonBudget)

	// a protective alert is still deduplicated once delivered...
	expect(t, e.decide(cand("liq:3", critical, protective)), proactive.DispDrop, ReasonDedupSame)
}

func TestDecide_ProtectiveIgnoresInFlightDecision(t *testing.T) {
	e := newEnv(t)
	d1 := e.decide(cand("liq:1", critical, protective)) // caller crashes: never marked
	expect(t, d1, proactive.DispNotify, ReasonProtective)
	expect(t, e.decide(cand("liq:1", critical, protective)), proactive.DispNotify, ReasonProtective)
}

func TestDecide_CriticalNotProtectiveFollowsRules(t *testing.T) {
	t.Run("quiet hours", func(t *testing.T) {
		e := newEnv(t)
		e.clock.Set(bkkAt(0, 3, 0))
		expect(t, e.decide(cand("c", critical)), proactive.DispDigest, ReasonQuietHours)
	})
	t.Run("budget does not apply", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 0 })
		expect(t, e.decide(cand("c", critical)), proactive.DispNotify, ReasonTemplate)
		// even with the budget spent by lower tiers.
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
		e.send(cand("n", notice))
		expect(t, e.decide(cand("c2", critical)), proactive.DispNotify, ReasonTemplate)
	})
	t.Run("mute and dedup still apply", func(t *testing.T) {
		e := newEnv(t)
		if err := e.g.Mute(context.Background(), "dn:", e.clock.Now().Add(time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		expect(t, e.decide(cand("dn:1", critical)), proactive.DispDrop, ReasonMuted)
		e.send(cand("c", critical))
		expect(t, e.decide(cand("c", critical)), proactive.DispDrop, ReasonDedupSame)
	})
	t.Run("compose llm", func(t *testing.T) {
		e := newEnv(t)
		expect(t, e.decide(cand("c", critical, composeLLM)), proactive.DispEscalate, ReasonComposeLLM)
	})
	t.Run("a delivered critical consumes no budget", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
		expect(t, e.send(cand("c", critical)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.send(cand("n", notice)), proactive.DispNotify, ReasonTemplate)
	})
	t.Run("protective but not critical is ordinary", func(t *testing.T) {
		e := newEnv(t)
		e.clock.Set(bkkAt(0, 3, 0))
		expect(t, e.decide(cand("c", warning, protective)), proactive.DispDigest, ReasonQuietHours)
	})
}

func TestDecide_QuietHoursToDigest_Bangkok(t *testing.T) {
	type tc struct {
		name string
		at   time.Time
		want proactive.Disposition
	}
	hm := func(h, m int) time.Time { return bkkAt(0, h, m) }
	cases := []tc{
		{"midnight", hm(0, 0), proactive.DispDigest},
		{"06:59 is still quiet", hm(6, 59), proactive.DispDigest},
		{"07:00 opens the window", hm(7, 0), proactive.DispNotify},
		{"noon", hm(12, 0), proactive.DispNotify},
		{"21:59 is still active", hm(21, 59), proactive.DispNotify},
		{"22:00 closes the window", hm(22, 0), proactive.DispDigest},
		{"23:59", hm(23, 59), proactive.DispDigest},
		{"same instant given in UTC (23:59 UTC = 06:59 Bangkok)", time.Date(2026, 3, 10, 23, 59, 0, 0, time.UTC), proactive.DispDigest},
		{"00:00 UTC = 07:00 Bangkok", time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC), proactive.DispNotify},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.clock.Set(c.at)
			d := e.decide(cand(fmt.Sprintf("k%d", i), notice))
			if d.Disposition != c.want {
				t.Fatalf("at %s: %s/%s, want %s", c.at.In(bkk).Format("15:04"), d.Disposition, d.Reason, c.want)
			}
			if c.want == proactive.DispDigest && d.Reason != ReasonQuietHours {
				t.Fatalf("reason = %s, want quiet_hours", d.Reason)
			}
		})
	}

	t.Run("overnight active hours", func(t *testing.T) {
		night := []tc{
			{"21:59 before the window", hm(21, 59), proactive.DispDigest},
			{"22:00 opens", hm(22, 0), proactive.DispNotify},
			{"23:30", hm(23, 30), proactive.DispNotify},
			{"00:00", hm(0, 0), proactive.DispNotify},
			{"05:59", hm(5, 59), proactive.DispNotify},
			{"06:00 closes", hm(6, 0), proactive.DispDigest},
			{"noon", hm(12, 0), proactive.DispDigest},
		}
		for i, c := range night {
			t.Run(c.name, func(t *testing.T) {
				e := newEnv(t)
				e.prof.set(func(p *intent.Profile) { p.ActiveHours = intent.HoursRange{Start: "22:00", End: "06:00"} })
				e.clock.Set(c.at)
				d := e.decide(cand(fmt.Sprintf("n%d", i), notice))
				if d.Disposition != c.want {
					t.Fatalf("at %s: %s/%s, want %s", c.at.Format("15:04"), d.Disposition, d.Reason, c.want)
				}
			})
		}
	})

	t.Run("other time zone", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.Timezone = "America/New_York" })
		e.clock.Set(time.Date(2026, 3, 10, 14, 0, 0, 0, time.UTC)) // 10:00 EDT
		expect(t, e.decide(cand("ny1", notice)), proactive.DispNotify, ReasonTemplate)
		e.clock.Set(time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)) // 01:00 EDT
		expect(t, e.decide(cand("ny2", notice)), proactive.DispDigest, ReasonQuietHours)
	})
}

func TestDecide_BudgetExhausted_WarningGetsOneExtra(t *testing.T) {
	t.Run("budget 3", func(t *testing.T) {
		e := newEnv(t)
		for i := 0; i < 3; i++ {
			expect(t, e.send(cand(fmt.Sprintf("n%d", i), notice)), proactive.DispNotify, ReasonTemplate)
		}
		expect(t, e.decide(cand("n3", notice)), proactive.DispDigest, ReasonBudget)
		expect(t, e.send(cand("w1", warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand("w2", warning)), proactive.DispDigest, ReasonBudget)
		expect(t, e.decide(cand("n4", notice)), proactive.DispDigest, ReasonBudget)
		// Info is digest material regardless, and is not counted.
		expect(t, e.decide(cand("i1", info)), proactive.DispDigest, ReasonInfoToDigest)
	})
	t.Run("budget 0", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 0 })
		expect(t, e.decide(cand("n0", notice)), proactive.DispDigest, ReasonBudget)
		expect(t, e.send(cand("w1", warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand("w2", warning)), proactive.DispDigest, ReasonBudget)
	})
	t.Run("a warning first still leaves a total of budget+1", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })
		expect(t, e.send(cand("w1", warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.send(cand("w2", warning)), proactive.DispNotify, ReasonTemplate)
		expect(t, e.decide(cand("w3", warning)), proactive.DispDigest, ReasonBudget)
		expect(t, e.decide(cand("n1", notice)), proactive.DispDigest, ReasonBudget)
	})
	t.Run("negative budget behaves as zero", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = -5 })
		expect(t, e.decide(cand("n", notice)), proactive.DispDigest, ReasonBudget)
	})
	t.Run("budget is checked before compose", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 0 })
		expect(t, e.decide(cand("n", notice, composeLLM)), proactive.DispDigest, ReasonBudget)
	})
}

func TestDecide_InFlightReservation(t *testing.T) {
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) { p.NotificationBudgetPerDay = 1 })

	d1 := e.decide(cand("a", notice)) // not yet reported
	expect(t, d1, proactive.DispNotify, ReasonTemplate)
	// the same situation is in flight, and the budget slot is reserved.
	expect(t, e.decide(cand("a", notice)), proactive.DispDrop, ReasonDedupSame)
	expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonBudget)

	// a failed send gives both back.
	e.mark(d1, false)
	expect(t, e.decide(cand("a", notice)), proactive.DispNotify, ReasonTemplate)

	// a caller that never reports loses its reservation after the lease.
	e.clock.Add(pendingLease + time.Second)
	expect(t, e.decide(cand("c", notice)), proactive.DispNotify, ReasonTemplate)
}

func TestDecide_InfoToDigest(t *testing.T) {
	e := newEnv(t)
	d := e.decide(cand("i", info))
	expect(t, d, proactive.DispDigest, ReasonInfoToDigest)
	// Info is digest material even for a protective trigger below Critical.
	expect(t, e.decide(cand("i2", info, protective)), proactive.DispDigest, ReasonInfoToDigest)
}

func TestDecide_ComposeLLMEscalates(t *testing.T) {
	e := newEnv(t)
	expect(t, e.decide(cand("a", notice, composeLLM)), proactive.DispEscalate, ReasonComposeLLM)
	expect(t, e.decide(cand("b", warning, composeLLM)), proactive.DispEscalate, ReasonComposeLLM)
	expect(t, e.decide(cand("c", notice)), proactive.DispNotify, ReasonTemplate)
}

func TestDecide_RecordsEveryDecision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.g.Mute(ctx, "muted:", e.clock.Now().Add(time.Hour), "x"); err != nil {
		t.Fatal(err)
	}
	sent := e.send(cand("sent", warning))
	e.clock.Add(time.Minute)

	cands := []struct {
		c    proactive.Candidate
		disp proactive.Disposition
		why  string
	}{
		{cand("sent", warning), proactive.DispDrop, ReasonDedupSame},
		{cand("muted:1", notice), proactive.DispDrop, ReasonMuted},
		{cand("sh", notice, func(c *proactive.Candidate) { c.Shadow = true }), proactive.DispLog, ReasonShadow},
		{cand("inf", info), proactive.DispDigest, ReasonInfoToDigest},
		{cand("llm", notice, composeLLM), proactive.DispEscalate, ReasonComposeLLM},
		{cand("liq", critical, protective), proactive.DispNotify, ReasonProtective},
	}
	ids := map[string]bool{sent.ID: true}
	for _, x := range cands {
		d := e.decide(x.c)
		expect(t, d, x.disp, x.why)
		if ids[d.ID] {
			t.Fatalf("duplicate decision ID %s", d.ID)
		}
		ids[d.ID] = true
		if d.At.IsZero() {
			t.Error("Decision.At is zero")
		}
		var disp, reason, key, trig string
		var sev, shadow int
		var atMs int64
		err := e.db.QueryRow(`SELECT disposition, reason, dedup_key, trigger_id, severity, shadow, at_ms
			FROM attention_decisions WHERE id = ?`, d.ID).Scan(&disp, &reason, &key, &trig, &sev, &shadow, &atMs)
		if err != nil {
			t.Fatalf("no row for %s decision: %v", x.why, err)
		}
		wantShadow := 0
		if x.why == ReasonShadow {
			wantShadow = 1
		}
		if disp != string(x.disp) || reason != x.why || key != x.c.DedupKey || trig != x.c.TriggerID ||
			sev != int(x.c.Severity) || shadow != wantShadow || atMs != e.clock.Now().UnixMilli() {
			t.Errorf("row for %s = %s/%s %s %s sev=%d shadow=%d at=%d", x.why, disp, reason, key, trig, sev, shadow, atMs)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions"); n != len(cands)+1 {
		t.Fatalf("rows = %d, want %d", n, len(cands)+1)
	}
}

func TestDayBoundaryInProfileTimezone(t *testing.T) {
	allDay := func(p *intent.Profile) {
		p.NotificationBudgetPerDay = 1
		p.ActiveHours = intent.HoursRange{Start: "00:00", End: "23:59"}
	}
	t.Run("resets at local midnight, though UTC has not rolled over", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(allDay)
		e.clock.Set(bkkAt(0, 23, 30)) // 16:30 UTC
		expect(t, e.send(cand("a", notice)), proactive.DispNotify, ReasonTemplate)
		e.clock.Set(bkkAt(0, 23, 50))
		expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonBudget)
		e.clock.Set(bkkAt(1, 0, 10)) // 17:10 UTC, same UTC day
		expect(t, e.decide(cand("c", notice)), proactive.DispNotify, ReasonTemplate)
	})
	t.Run("does not reset at UTC midnight", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(allDay)
		e.clock.Set(bkkAt(1, 6, 30)) // 23:30 UTC the day before
		expect(t, e.send(cand("a", notice)), proactive.DispNotify, ReasonTemplate)
		e.clock.Set(bkkAt(1, 7, 10)) // 00:10 UTC: a new UTC day, the same Bangkok day
		expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonBudget)
	})
	t.Run("the day is the delivery day", func(t *testing.T) {
		e := newEnv(t)
		e.prof.set(allDay)
		e.clock.Set(bkkAt(0, 23, 58))
		d := e.decide(cand("a", notice))
		expect(t, d, proactive.DispNotify, ReasonTemplate)
		e.clock.Set(bkkAt(1, 0, 1)) // the send completes after midnight
		e.mark(d, true)
		expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonBudget)
	})
}

func TestDecide_ProfileSourceErrorFallsBack(t *testing.T) {
	cfg := liveCfg()
	cfg.DailyBudget = 2
	e := newEnvMode(t, config.ProactiveModeLive, cfg)
	e.prof.err = fmt.Errorf("profile store is down")

	// the default window and cfg.DailyBudget apply.
	expect(t, e.send(cand("a", notice)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.send(cand("b", notice)), proactive.DispNotify, ReasonTemplate)
	expect(t, e.decide(cand("c", notice)), proactive.DispDigest, ReasonBudget)

	// still quiet overnight (Bangkok default), still protective.
	e.clock.Set(bkkAt(1, 3, 0))
	expect(t, e.decide(cand("d", notice)), proactive.DispDigest, ReasonQuietHours)
	expect(t, e.decide(cand("liq", critical, protective)), proactive.DispNotify, ReasonProtective)
	if e.prof.callCount() == 0 {
		t.Fatal("profile source was never consulted")
	}
}

func TestDecide_UnknownTimezoneFallsBackToUTC(t *testing.T) {
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) { p.Timezone = "Mars/Olympus_Mons" })

	// 06:30 UTC is 13:30 in Bangkok (active) but before 07:00 in UTC.
	e.clock.Set(time.Date(2026, 3, 10, 6, 30, 0, 0, time.UTC))
	expect(t, e.decide(cand("a", notice)), proactive.DispDigest, ReasonQuietHours)
	// 21:30 UTC is 04:30 in Bangkok (quiet) but active in UTC.
	e.clock.Set(time.Date(2026, 3, 10, 21, 30, 0, 0, time.UTC))
	expect(t, e.decide(cand("b", notice)), proactive.DispNotify, ReasonTemplate)
}

func TestDecide_UnknownTimezoneDayRollsAtUTCMidnight(t *testing.T) {
	e := newEnv(t)
	e.prof.set(func(p *intent.Profile) {
		p.Timezone = "Mars/Olympus_Mons"
		p.NotificationBudgetPerDay = 1
		p.ActiveHours = intent.HoursRange{Start: "00:00", End: "23:59"}
	})
	e.clock.Set(time.Date(2026, 3, 10, 23, 30, 0, 0, time.UTC))
	expect(t, e.send(cand("a", notice)), proactive.DispNotify, ReasonTemplate)
	e.clock.Set(time.Date(2026, 3, 10, 23, 50, 0, 0, time.UTC))
	expect(t, e.decide(cand("b", notice)), proactive.DispDigest, ReasonBudget)
	e.clock.Set(time.Date(2026, 3, 11, 0, 10, 0, 0, time.UTC)) // 07:10 in Bangkok: same day there
	expect(t, e.decide(cand("c", notice)), proactive.DispNotify, ReasonTemplate)
}

func TestDecide_Validation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.g.Decide(ctx, proactive.Candidate{Severity: notice}); err == nil {
		t.Error("empty dedup key accepted")
	}
	for _, s := range []proactive.Severity{-1, 4, 99} {
		if _, err := e.g.Decide(ctx, cand("k", s)); err == nil {
			t.Errorf("severity %d accepted", s)
		}
	}
	if n := e.count("SELECT COUNT(*) FROM attention_decisions"); n != 0 {
		t.Fatalf("rejected candidates left %d rows", n)
	}

	c := cand("k", notice)
	c.ID = ""
	d := e.decide(c)
	if d.Candidate.ID == "" || !d.Candidate.At.Equal(e.clock.Now()) {
		t.Fatalf("candidate not normalised: ID=%q At=%v", d.Candidate.ID, d.Candidate.At)
	}
	c2 := cand("k2", notice)
	c2.ID, c2.At = "given", bkkAt(0, 1, 2)
	d2 := e.decide(c2)
	if d2.Candidate.ID != "given" || !d2.Candidate.At.Equal(bkkAt(0, 1, 2)) {
		t.Fatalf("caller's ID/At overwritten: %q %v", d2.Candidate.ID, d2.Candidate.At)
	}
}

func TestDecide_ShadowCoalescing(t *testing.T) {
	shadowCount := func(e *env) int {
		return e.count("SELECT COUNT(*) FROM attention_decisions WHERE reason = 'shadow'")
	}
	t.Run("120 calls a minute apart for one key", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		for i := 0; i < 120; i++ {
			expect(t, e.decide(cand("k", notice)), proactive.DispLog, ReasonShadow)
			e.clock.Add(time.Minute)
		}
		if n := shadowCount(e); n != 2 {
			t.Fatalf("shadow rows = %d, want 2 (one per hour)", n)
		}
	})
	t.Run("a severity increase inserts at once", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		e.decide(cand("k", notice))
		e.clock.Add(time.Minute)
		e.decide(cand("k", notice))
		e.decide(cand("k", info)) // lower: still coalesced
		if n := shadowCount(e); n != 1 {
			t.Fatalf("rows = %d, want 1", n)
		}
		e.decide(cand("k", warning))
		e.decide(cand("k", warning))
		if n := shadowCount(e); n != 2 {
			t.Fatalf("rows after increase = %d, want 2", n)
		}
	})
	t.Run("different keys are independent", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		for _, k := range []string{"a", "b", "c", "a", "b", "c"} {
			e.decide(cand(k, notice))
		}
		if n := shadowCount(e); n != 3 {
			t.Fatalf("rows = %d, want 3", n)
		}
	})
	t.Run("c.Shadow in live mode coalesces too, other decisions do not", func(t *testing.T) {
		e := newEnv(t)
		sh := func(c *proactive.Candidate) { c.Shadow = true }
		e.decide(cand("k", notice, sh))
		e.decide(cand("k", notice, sh))
		if n := shadowCount(e); n != 1 {
			t.Fatalf("shadow rows = %d, want 1", n)
		}
		// a live candidate with the same key is unaffected by the shadow row.
		expect(t, e.decide(cand("k", notice)), proactive.DispNotify, ReasonTemplate)
		// non-shadow decisions are all recorded: two drops make two rows.
		expect(t, e.decide(cand("k", notice)), proactive.DispDrop, ReasonDedupSame)
		expect(t, e.decide(cand("k", notice)), proactive.DispDrop, ReasonDedupSame)
		if n := e.count("SELECT COUNT(*) FROM attention_decisions WHERE reason = ?", ReasonDedupSame); n != 2 {
			t.Fatalf("dedup rows = %d, want 2", n)
		}
	})
	t.Run("the hour passes", func(t *testing.T) {
		e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
		e.decide(cand("k", notice))
		e.clock.Add(shadowCoalesce)
		e.decide(cand("k", notice))
		if n := shadowCount(e); n != 2 {
			t.Fatalf("rows = %d, want 2", n)
		}
	})
}
