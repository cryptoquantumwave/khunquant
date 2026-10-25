package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func TestHandOver_ToolAlertGetsAStandInAndComesBack(t *testing.T) {
	w := newWorld(t)
	rec := w.addTool(priceTool(60000, true))

	rep := w.sync(Shadow)
	if rep.HandedOver != 1 || rep.Imported != 0 {
		t.Fatalf("report = %+v", rep)
	}
	j, ok := w.standIn(rec.ID)
	if !ok || !j.Enabled || j.Payload.Channel != "telegram" || j.Payload.To != "1001" || !j.Payload.NoHistory {
		t.Fatalf("stand-in = %+v, %v (must be enabled and routed to the configured owner)", j, ok)
	}
	if !strings.HasPrefix(j.Name, "price_alert:okx:BTC/USDT:below:") {
		t.Fatalf("name = %q", j.Name)
	}
	var lp legacyPrice
	if err := json.Unmarshal([]byte(j.Payload.Message), &lp); err != nil {
		t.Fatal(err)
	}
	if lp.ProviderID != "okx" || lp.Symbol != "BTC/USDT" || lp.Condition != "below" || lp.Threshold != 60000 ||
		!lp.Recurring || lp.CooldownMinutes != 30 || lp.AlertMsg != "dip" {
		t.Fatalf("payload = %+v", lp)
	}
	if s, _ := w.spec(rec.ID); s.Mode != triggers.ModeOff {
		t.Fatalf("spec mode = %q, want off: dormant, with a stand-in", s.Mode)
	}
	// No second spec was made from the stand-in, now or on any later run.
	for i := 0; i < 2; i++ {
		if again := w.sync(Shadow); again != (Report{}) {
			t.Fatalf("run %d did something: %+v", i, again)
		}
	}
	if n := len(w.specs()); n != 1 {
		t.Fatalf("%d specs, want 1: the stand-in must not be imported as a new alert", n)
	}

	// The registry is live again: the SAME spec is taken back, the stand-in removed.
	rep = w.sync(Live)
	if rep.Reactivated != 1 {
		t.Fatalf("report = %+v", rep)
	}
	s, _ := w.spec(rec.ID)
	var p triggers.PriceCrossParams
	_ = json.Unmarshal(s.Params, &p)
	if s.Mode != triggers.ModeLive || p.FireIfAlready == nil || *p.FireIfAlready {
		t.Fatalf("spec = %+v params %+v: a recurring alert must come back live and not announce 'already' again", s, p)
	}
	if _, has := w.standIn(rec.ID); has {
		t.Fatal("the stand-in was not removed")
	}
	if n := len(w.specs()); n != 1 {
		t.Fatalf("%d specs after the take-back", n)
	}
	if again := w.sync(Live); again != (Report{}) {
		t.Fatalf("second live run did something: %+v", again)
	}
	// And away again.
	if rep := w.sync(Shadow); rep.HandedOver != 1 {
		t.Fatalf("second hand-over: %+v", rep)
	}
}

func TestHandOver_OneShotKeepsItsOwnFirstObservationRule(t *testing.T) {
	w := newWorld(t)
	rec := w.addTool(priceTool(60000, false))
	w.sync(Shadow)
	w.sync(Live)
	s, _ := w.spec(rec.ID)
	var p triggers.PriceCrossParams
	_ = json.Unmarshal(s.Params, &p)
	if p.FireIfAlready != nil {
		t.Fatalf("a one-shot's parameters were changed: %+v", p)
	}
}

func TestHandOver_AOneShotThatFiredOnTheStandInIsNotRevived(t *testing.T) {
	w := newWorld(t)
	now := time.Now()
	w.clock = func() time.Time { return now }
	rec := w.addTool(priceTool(60000, false))
	w.sync(Shadow)
	j, _ := w.standIn(rec.ID)
	w.cron.RemoveJob(j.ID) // the legacy handler removes a one-shot after delivering it

	// Going live does not bring it back: the spec is off and has no stand-in.
	w.sync(Live)
	if s, ok := w.spec(rec.ID); !ok || s.Mode != triggers.ModeOff {
		t.Fatalf("spec = %+v, %v", s, ok)
	}
	// It is removed once the grace period has passed.
	now = now.Add(DefaultGrace + time.Minute)
	if rep := w.sync(Live); rep.Swept != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := w.spec(rec.ID); ok {
		t.Fatal("a finished alert is still in the table")
	}
}

func TestHandOver_NoOwnerRouteIsNeverGuessed(t *testing.T) {
	w := newWorld(t)
	w.noOwner = true
	rec := w.addTool(priceTool(60000, true))
	rep := w.sync(Shadow)
	if rep.Inactive != 1 || rep.HandedOver != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if _, has := w.standIn(rec.ID); has {
		t.Fatal("a stand-in was made without an owner route")
	}
	if s, _ := w.spec(rec.ID); s.Mode != triggers.ModeLive {
		t.Fatalf("spec mode = %q: it is left as it is, to be shown as inactive", s.Mode)
	}
	if len(w.cron.ListJobs(true)) != 0 {
		t.Fatal("a cron job exists")
	}
}

func TestHandOver_WhatCronCannotExpressStaysInactive(t *testing.T) {
	w := newWorld(t)
	p := triggers.IndicatorCrossParams{Provider: "okx", Symbol: "BTC/USDT", Timeframe: "1h", Indicator: triggers.IndMACDHist,
		Direction: "above", Threshold: 0}
	if _, err := w.reg.Add(context.Background(), triggers.Spec{Kind: triggers.KindIndicatorCross, Params: p.JSON(),
		Mode: triggers.ModeLive, Origin: "tool:set_indicator_alert"}); err != nil {
		t.Fatal(err)
	}
	ok := triggers.IndicatorCrossParams{Provider: "okx", Symbol: "BTC/USDT", Timeframe: "4h", Indicator: triggers.IndEMA, Period: 50,
		Direction: "below", Threshold: 1, Recurring: true}
	spec, err := w.reg.Add(context.Background(), triggers.Spec{Kind: triggers.KindIndicatorCross, Params: ok.JSON(),
		Mode: triggers.ModeLive, Origin: "tool:set_indicator_alert"})
	if err != nil {
		t.Fatal(err)
	}
	rep := w.sync(Shadow)
	if rep.Inactive != 1 || rep.HandedOver != 1 {
		t.Fatalf("report = %+v", rep)
	}
	j, _ := w.standIn(spec.ID)
	var li legacyIndicator
	_ = json.Unmarshal([]byte(j.Payload.Message), &li)
	if li.Indicator != "EMA" || li.Period != 50 || li.Timeframe != "4h" || !strings.HasPrefix(j.Name, "indicator_alert:okx:BTC/USDT:EMA50:below:") {
		t.Fatalf("stand-in = %s %+v", j.Name, li)
	}
}

// The bound: finished alerts do not accumulate. Those inside the grace period stay
// (listed as done), the rest go at the next sync, however many there are.
func TestSweep_FinishedAlertsAreBounded(t *testing.T) {
	w := newWorld(t)
	base := time.Now()
	w.clock = func() time.Time { return base }
	var ids []string
	for i := 0; i < 40; i++ {
		s := w.addTool(priceTool(float64(1000+i), false))
		s.Mode = triggers.ModeOff // a one-shot that fired
		if err := w.reg.Update(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
	}
	if rep := w.sync(Shadow); rep.Swept != 0 {
		t.Fatalf("finished alerts inside the grace period were removed: %+v", rep)
	}
	w.clock = func() time.Time { return base.Add(DefaultGrace - time.Minute) }
	if rep := w.sync(Shadow); rep.Swept != 0 {
		t.Fatalf("removed before the grace period: %+v", rep)
	}
	w.clock = func() time.Time { return base.Add(DefaultGrace + time.Minute) }
	if rep := w.sync(Shadow); rep.Swept != 40 {
		t.Fatalf("report = %+v, want all 40 swept", rep)
	}
	for _, id := range ids {
		if _, ok := w.spec(id); ok {
			t.Fatalf("spec %s survived", id)
		}
	}
	// A live one-shot that has not fired is never touched by the sweep.
	live := w.addTool(priceTool(5, false))
	w.clock = func() time.Time { return base.Add(100 * DefaultGrace) }
	w.sync(Live)
	if s, ok := w.spec(live.ID); !ok || s.Mode != triggers.ModeLive {
		t.Fatalf("an unfinished alert was swept: %+v %v", s, ok)
	}
}

func TestSweep_ZeroGraceRemovesAtOnce(t *testing.T) {
	w := newWorld(t)
	s := w.addTool(priceTool(1000, false))
	s.Mode = triggers.ModeOff
	_ = w.reg.Update(context.Background(), s)
	rep, err := MigrateAlertJobs(context.Background(), w.cron, w.reg, Shadow, WithGrace(0), WithClock(func() time.Time { return time.Now().Add(time.Second) }))
	if err != nil || rep.Swept != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestHandOver_InterruptedAtEveryStep(t *testing.T) {
	setup := func() (*world, []string) {
		w := newWorld(t)
		a := w.addTool(priceTool(60000, true))
		b := w.addTool(priceTool(70000, false))
		return w, []string{a.ID, b.ID}
	}
	probe, _ := setup()
	total, err := probe.syncWithCrash(Shadow, 0)
	if err != nil || total == 0 {
		t.Fatalf("clean run: %d calls, %v", total, err)
	}
	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprintf("crash at write %d of %d", k, total), func(t *testing.T) {
			w, ids := setup()
			if _, err := w.syncWithCrash(Shadow, k); err == nil {
				t.Fatal("the crash was not reported")
			}
			// Whatever was touched is delivered by a stand-in; what was not is as it
			// was (a live spec the global mode keeps quiet, found again by the next run).
			for _, id := range ids {
				s, _ := w.spec(id)
				_, has := w.standIn(id)
				if s.Mode == triggers.ModeOff && !has {
					t.Fatalf("after a crash at write %d, %s is off with no stand-in", k, id)
				}
			}
			// Restart: converges, with exactly one stand-in per spec.
			w.reg = w.newRegistry()
			w.sync(Shadow)
			for _, id := range ids {
				s, _ := w.spec(id)
				j, has := w.standIn(id)
				if s.Mode != triggers.ModeOff || !has || !j.Enabled {
					t.Fatalf("recovered: spec %q, stand-in %v", s.Mode, has)
				}
			}
			if n := countStandIns(w); n != len(ids) {
				t.Fatalf("%d stand-ins for %d alerts", n, len(ids))
			}
			if len(w.specs()) != len(ids) {
				t.Fatalf("%d specs: the stand-ins were imported as new alerts", len(w.specs()))
			}
		})
	}
}

func TestTakeBack_InterruptedAtEveryStep(t *testing.T) {
	setup := func() (*world, []string) {
		w := newWorld(t)
		a := w.addTool(priceTool(60000, true))
		b := w.addTool(priceTool(70000, false))
		w.sync(Shadow)
		return w, []string{a.ID, b.ID}
	}
	probe, _ := setup()
	total, err := probe.syncWithCrash(Live, 0)
	if err != nil || total == 0 {
		t.Fatalf("clean run: %d calls, %v", total, err)
	}
	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprintf("crash at write %d of %d", k, total), func(t *testing.T) {
			w, ids := setup()
			if _, err := w.syncWithCrash(Live, k); err == nil {
				t.Fatal("the crash was not reported")
			}
			// Between any two writes the alert is alive: the stand-in delivers, or the
			// live spec does; at worst both.
			for _, id := range ids {
				s, _ := w.spec(id)
				_, has := w.standIn(id)
				if !has && s.Mode != triggers.ModeLive {
					t.Fatalf("after a crash at write %d, %s is silent: spec %q, no stand-in", k, id, s.Mode)
				}
			}
			// Restarted as live: one spec, live, no stand-in.
			w.reg = w.newRegistry()
			w.sync(Live)
			for _, id := range ids {
				s, _ := w.spec(id)
				if _, has := w.standIn(id); has || s.Mode != triggers.ModeLive {
					t.Fatalf("recovered as live: spec %q stand-in %v", s.Mode, has)
				}
			}
			// Or restarted as shadow after the crash: handed over again, exactly once.
			w2, ids2 := setup()
			_, _ = w2.syncWithCrash(Live, k)
			w2.reg = w2.newRegistry()
			w2.sync(Shadow)
			for _, id := range ids2 {
				s, _ := w2.spec(id)
				if _, has := w2.standIn(id); !has || s.Mode != triggers.ModeOff {
					t.Fatalf("recovered as shadow: spec %q stand-in %v", s.Mode, has)
				}
			}
			if n := countStandIns(w2); n != len(ids2) {
				t.Fatalf("%d stand-ins for %d alerts", n, len(ids2))
			}
		})
	}
}

func countStandIns(w *world) int {
	n := 0
	for _, j := range w.cron.ListJobs(true) {
		if j.State.StandInFor != "" {
			n++
		}
	}
	return n
}
