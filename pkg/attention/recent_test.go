package attention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func decide(t *testing.T, e *env, c proactive.Candidate) Decision {
	t.Helper()
	d, err := e.g.Decide(context.Background(), c)
	if err != nil {
		t.Fatalf("Decide(%s): %v", c.DedupKey, err)
	}
	return d
}

// DecisionsSince lists what the governor decided in the window, newest first, with
// the base asset it indexed, and leaves out the heartbeat's own rows and anything
// older.
func TestDecisionsSince_ListsTheWindowsDecisions(t *testing.T) {
	e := newEnv(t)
	old := e.clock.Now()
	decide(t, e, cand("move_vs_vol:ETH/USDT", proactive.SeverityNotice, func(c *proactive.Candidate) { c.Subject = "ETH/USDT" }))
	e.clock.Add(2 * time.Hour)
	since := e.clock.Now().Add(-time.Minute)

	decide(t, e, cand("move_vs_vol:BTC/USDT", proactive.SeverityWarning))
	e.clock.Add(time.Minute)
	decide(t, e, cand("drawdown:portfolio:day:5", proactive.SeverityWarning, func(c *proactive.Candidate) { c.Subject = "portfolio" }))
	e.clock.Add(time.Minute)
	decide(t, e, cand("heartbeat:sol:drop", proactive.SeverityNotice, withTrigger("heartbeat")))
	e.clock.Add(time.Minute)
	// A repeat of the first is a decision too (a drop): the topic is live.
	rep := decide(t, e, cand("move_vs_vol:BTC/USDT", proactive.SeverityWarning))
	if rep.Disposition != proactive.DispDrop {
		t.Fatalf("setup: the repeat was %s, want a drop", rep.Disposition)
	}

	got, err := e.g.DecisionsSince(context.Background(), since, "heartbeat", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d decisions, want 3: %+v", len(got), got)
	}
	if got[0].DedupKey != "move_vs_vol:BTC/USDT" || got[0].Disposition != proactive.DispDrop {
		t.Errorf("newest first: %+v", got[0])
	}
	for _, r := range got {
		if r.TriggerID == "heartbeat" || r.At.Before(since) || r.At.Before(old) {
			t.Errorf("row outside the window or the heartbeat's own: %+v", r)
		}
	}
	byKey := map[string]RecordedDecision{}
	for _, r := range got {
		byKey[r.DedupKey] = r
	}
	if r := byKey["move_vs_vol:BTC/USDT"]; r.BaseAsset != "BTC" || r.Severity != proactive.SeverityWarning {
		t.Errorf("btc row: %+v", r)
	}
	if r := byKey["drawdown:portfolio:day:5"]; r.BaseAsset != "" {
		t.Errorf("a portfolio-wide topic has no base asset: %+v", r)
	}

	// Without the exclusion the heartbeat's row is there too; the limit applies.
	all, _ := e.g.DecisionsSince(context.Background(), since, "", 0)
	if len(all) != 4 {
		t.Errorf("%d rows without the exclusion, want 4", len(all))
	}
	if one, _ := e.g.DecisionsSince(context.Background(), since, "", 1); len(one) != 1 {
		t.Errorf("limit 1 gave %d rows", len(one))
	}
}

// Shadow rows told the owner nothing, so they are not "decided topics".
func TestDecisionsSince_LeavesOutShadowRows(t *testing.T) {
	e := newEnvMode(t, config.ProactiveModeShadow, liveCfg())
	decide(t, e, cand("move_vs_vol:BTC/USDT", proactive.SeverityWarning))
	got, err := e.g.DecisionsSince(context.Background(), e.clock.Now().Add(-time.Hour), "", 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("shadow rows listed: %+v (%v)", got, err)
	}
}

// The outbox reads through its governor, and says so when its gate cannot.
func TestOutbox_DecisionsSince(t *testing.T) {
	e := newEnv(t)
	ob := NewOutbox(e.g, nil, 0, e.clock.Now)
	decide(t, e, cand("move_vs_vol:BTC/USDT", proactive.SeverityNotice))
	got, err := ob.DecisionsSince(context.Background(), e.clock.Now().Add(-time.Hour), "", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}

	bare := &Outbox{g: noLogGate{}}
	if _, err := bare.DecisionsSince(context.Background(), time.Time{}, "", 0); !errors.Is(err, ErrNoDecisionLog) {
		t.Fatalf("err = %v, want ErrNoDecisionLog", err)
	}
}

// noLogGate is a Gate that keeps no decision log.
type noLogGate struct{ Gate }
