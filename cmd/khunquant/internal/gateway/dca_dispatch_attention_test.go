package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

type staticProfile struct{ p intent.Profile }

func (s staticProfile) Profile(context.Context) (intent.Profile, error) { return s.p, nil }

// TestDispatcher_AttentionsDecisionOnAMandatedRun feeds a live execute_dca candidate to
// the REAL attention governor and the result to the dispatcher. Attention governs
// messages, a mandate governs an order: quiet hours, the digest, the daily budget and
// dedup do not stop a mandated purchase, while an explicit owner mute of the trigger
// does (and a snooze, which is one exact dedup key, stops only that slot).
func TestDispatcher_AttentionsDecisionOnAMandatedRun(t *testing.T) {
	bkk, _ := time.LoadLocation("Asia/Bangkok")
	const planID = 5
	dueMS := int64(1791770400000)
	mk := func(slotMS int64) proactive.Candidate {
		return proactive.Candidate{
			TriggerID: "dca_due:plan-5", DedupKey: fmt.Sprintf("dca_due:5:%d", slotMS), Severity: proactive.SeverityInfo,
			Subject: "BTC/THB", Title: "DCA plan 5 is due", Compose: proactive.ComposeTemplate,
			Evidence: map[string]any{"action": "execute_dca", "plan_id": int64(planID), "due_at_ms": slotMS, "mandate_id": "mnd_1"},
		}
	}
	type tc struct {
		name    string
		at      time.Time // the governor's clock
		prep    func(g *attention.Governor)
		budget  int
		wantRun bool
		reason  string // the disposition reason the test expects to have seen
	}
	noon := time.Date(2026, 10, 12, 12, 0, 0, 0, bkk)
	cases := []tc{
		{name: "in active hours (an Info message is digested)", at: noon, budget: 3, wantRun: true, reason: attention.ReasonInfoToDigest},
		{name: "held by quiet hours", at: time.Date(2026, 10, 12, 3, 0, 0, 0, bkk), budget: 3, wantRun: true, reason: attention.ReasonQuietHours},
		{name: "daily budget spent", at: noon, budget: 1, wantRun: true, reason: attention.ReasonInfoToDigest},
		{name: "muted by /mute dca_due", at: noon, budget: 3, wantRun: false, reason: attention.ReasonMuted,
			prep: func(g *attention.Governor) {
				if err := g.Mute(context.Background(), "dca_due", noon.Add(24*time.Hour), "owner"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "muted by /mute of this plan's trigger", at: noon, budget: 3, wantRun: false, reason: attention.ReasonMuted,
			prep: func(g *attention.Governor) {
				if err := g.Mute(context.Background(), "dca_due:plan-5", noon.Add(24*time.Hour), "owner"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "snoozed (this slot's exact key)", at: noon, budget: 3, wantRun: false, reason: attention.ReasonMuted,
			prep: func(g *attention.Governor) {
				if err := g.MuteExact(context.Background(), fmt.Sprintf("dca_due:5:%d", dueMS), noon.Add(24*time.Hour), "snooze"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "snoozed for another slot", at: noon, budget: 3, wantRun: true, reason: attention.ReasonInfoToDigest,
			prep: func(g *attention.Governor) {
				if err := g.MuteExact(context.Background(), "dca_due:5:1", noon.Add(24*time.Hour), "snooze"); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.AttentionConfig{Mode: config.ProactiveModeLive, DailyBudget: c.budget, DedupWindowHours: 24}
			g, err := attention.New(sqlitextest.Open(t), staticProfile{intent.DefaultProfile()}, cfg, config.ProactiveModeLive, func() time.Time { return c.at })
			if err != nil {
				t.Fatal(err)
			}
			if c.prep != nil {
				c.prep(g)
			}
			dec, err := g.Decide(context.Background(), mk(dueMS))
			if err != nil {
				t.Fatal(err)
			}
			if dec.Reason != c.reason {
				t.Fatalf("attention decided %s/%s, the case expects reason %s", dec.Disposition, dec.Reason, c.reason)
			}
			shared := &sharedResources{}
			st, next := &recStarter{}, &recNext{}
			shared.setDCAStarter(st)
			shared.dcaPromoted.Store(int64(planID), "mnd_1")
			if err := (dcaDispatcher{next: next, shared: shared}).Dispatch(context.Background(), []attention.Decision{dec}); err != nil {
				t.Fatal(err)
			}
			started := len(st.ids) == 1 && st.ids[0] == fmt.Sprintf("%s/%s", dca.KindExecute, dca.WorkflowID(planID, dueMS))
			if started != c.wantRun {
				t.Errorf("started = %v (%v), want %v", started, st.ids, c.wantRun)
			}
			if len(next.got) != 0 {
				t.Error("a mandated run's decision also went to the outbox as a message")
			}
		})
	}

	// Dispositions attention can produce for a message that has nothing to do with the
	// order: none of them stops it, only a mute, or attention being off or shadow.
	for _, d := range []struct {
		disp   proactive.Disposition
		reason string
		run    bool
	}{
		{proactive.DispDigest, attention.ReasonBudget, true},
		{proactive.DispDigest, attention.ReasonDeferred, true},
		{proactive.DispDrop, attention.ReasonDedupSame, true},
		{proactive.DispDrop, attention.ReasonDedupQueued, true},
		{proactive.DispDrop, attention.ReasonInhibitedPrefix + "x", true},
		{proactive.DispDrop, attention.ReasonMuted, false},
		{proactive.DispDrop, attention.ReasonOff, false},
		{proactive.DispLog, attention.ReasonShadow, false},
	} {
		shared := &sharedResources{}
		st := &recStarter{}
		shared.setDCAStarter(st)
		shared.dcaPromoted.Store(int64(planID), "mnd_1")
		dec := attention.Decision{Disposition: d.disp, Reason: d.reason, Candidate: mk(dueMS)}
		_ = (dcaDispatcher{next: &recNext{}, shared: shared}).Dispatch(context.Background(), []attention.Decision{dec})
		if (len(st.ids) == 1) != d.run {
			t.Errorf("%s/%s: started %v, want %v", d.disp, d.reason, st.ids, d.run)
		}
	}
}
