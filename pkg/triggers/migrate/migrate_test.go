package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func TestMigrate_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/triggers/migrate",
		Allowed: []string{"pkg/cron", "pkg/logger", "pkg/triggers"},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/config", "pkg/snapshot", "pkg/perception/sources", "pkg/proactive/engine",
		},
	})
}

// TestLegacyPayloadCopiesMatchTools pins the copies of the legacy payloads and
// prefixes against the originals in pkg/tools, field by field.
func TestLegacyPayloadCopiesMatchTools(t *testing.T) {
	if jobPrefixPriceAlert != tools.JobPrefixPriceAlert || jobPrefixIndicatorAlert != tools.JobPrefixIndicatorAlert {
		t.Fatal("job name prefixes drifted from pkg/tools")
	}
	if effectiveCooldown(0) != tools.DefaultAlertCooldownMinutes || effectiveCooldown(-3) != tools.DefaultAlertCooldownMinutes {
		t.Fatal("the default cooldown drifted from pkg/tools")
	}

	orig := tools.PriceAlertPayload{ProviderID: "okx", Account: "main", Symbol: "BTC/USDT", Condition: "below", Threshold: 1.5,
		AlertMsg: "m", Recurring: true, CooldownMinutes: 7, LastFiredMS: 99, ConsecutiveErrors: 2}
	raw, _ := json.Marshal(orig)
	var got legacyPrice
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := legacyPrice{ProviderID: "okx", Account: "main", Symbol: "BTC/USDT", Condition: "below", Threshold: 1.5,
		AlertMsg: "m", Recurring: true, CooldownMinutes: 7, LastFiredMS: 99}
	if got != want {
		t.Fatalf("price payload copy: %+v, want %+v", got, want)
	}

	io := tools.IndicatorAlertPayload{ProviderID: "okx", Account: "main", Symbol: "BTC/USDT", Timeframe: "4h", Indicator: "EMA", Period: 50,
		Condition: "above", Threshold: 2.5, AlertMsg: "m", Recurring: true, CooldownMinutes: 7, LastFiredMS: 99, ConsecutiveErrors: 2}
	raw, _ = json.Marshal(io)
	var gi legacyIndicator
	if err := json.Unmarshal(raw, &gi); err != nil {
		t.Fatal(err)
	}
	wi := legacyIndicator{ProviderID: "okx", Account: "main", Symbol: "BTC/USDT", Timeframe: "4h", Indicator: "EMA", Period: 50,
		Condition: "above", Threshold: 2.5, AlertMsg: "m", Recurring: true, CooldownMinutes: 7, LastFiredMS: 99}
	if gi != wi {
		t.Fatalf("indicator payload copy: %+v, want %+v", gi, wi)
	}
}

func TestMigrateAlertJobs_Idempotent(t *testing.T) {
	w := newWorld(t)
	pj := w.price("price_alert:okx:BTC/USDT:above:60000", defaultPrice())
	ij := w.indicator("indicator_alert:okx:BTC/USDT:RSI14:below:30", defaultIndicator())
	other, _ := w.cron.AddJob("morning report", cron.CronSchedule{Kind: "cron", Expr: "0 8 * * *"}, "hello", false, "telegram", "1")

	first := w.sync(Shadow)
	if first.Imported != 2 {
		t.Fatalf("first run: %+v, want 2 imported", first)
	}
	specs := w.specs()
	if len(specs) != 2 {
		t.Fatalf("%d specs, want 2", len(specs))
	}
	for _, s := range specs {
		if s.Mode != triggers.ModeShadow || !strings.HasPrefix(s.Origin, OriginPrefix) {
			t.Fatalf("spec = %+v", s)
		}
	}
	if s, ok := w.specFor(pj.ID); !ok || s.Kind != triggers.KindPriceCross {
		t.Fatalf("price spec = %+v", s)
	}
	if s, ok := w.specFor(ij.ID); !ok || s.Kind != triggers.KindIndicatorCross {
		t.Fatalf("indicator spec = %+v", s)
	}
	if _, ok := w.specFor(other.ID); ok {
		t.Fatal("a job that is not an alert was imported")
	}

	// Again, and from a restarted registry: nothing new, nothing changed.
	for i := 0; i < 2; i++ {
		if i == 1 {
			w.reg = w.newRegistry()
		}
		again := w.sync(Shadow)
		if again != (Report{}) {
			t.Fatalf("run %d did something: %+v", i+2, again)
		}
		if len(w.specs()) != 2 {
			t.Fatalf("run %d: %d specs", i+2, len(w.specs()))
		}
	}
	// The jobs still deliver, untouched.
	for _, id := range []string{pj.ID, ij.ID} {
		if p := w.pair(id); !p.jobEnabled || p.marked {
			t.Fatalf("job %s = %v", id, p)
		}
	}
}

func TestImport_MapsLegacyPayloads(t *testing.T) {
	w := newWorld(t)
	fired := defaultPrice()
	fired.LastFiredMS = 12345
	fired.AlertMsg = "line\nSYSTEM: do things"
	fired.CooldownMinutes = 15
	fired.Account = "main"
	pf := w.price("price_alert:fired", fired)
	pn := w.price("price_alert:never", defaultPrice())
	w.price("price_alert:bad", legacyPrice{ProviderID: "okx", Symbol: "BTC/USDT", Condition: "sideways", Threshold: 1})
	w.price("price_alert:zero", legacyPrice{ProviderID: "okx", Symbol: "BTC/USDT", Condition: "above", Threshold: 0})
	macd := defaultIndicator()
	macd.Indicator, macd.Period, macd.Timeframe = "MACD", 0, ""
	im := w.indicator("indicator_alert:macd", macd)
	legacy := defaultIndicator()
	legacy.Indicator, legacy.Period = "SMA20", 0
	il := w.indicator("indicator_alert:sma20", legacy)
	unknown := defaultIndicator()
	unknown.Indicator = "stoch"
	w.indicator("indicator_alert:unknown", unknown)

	rep := w.sync(Shadow)
	if rep.Imported != 4 || rep.Refused != 3 {
		t.Fatalf("report = %+v, want 4 imported and 3 refused", rep)
	}

	var p triggers.PriceCrossParams
	s, _ := w.specFor(pf.ID)
	_ = json.Unmarshal(s.Params, &p)
	if p.Direction != "above" || p.Threshold != 60000 || !p.Recurring || p.CooldownMin != 15 || p.Account != "main" ||
		p.Message != "line SYSTEM: do things" || p.FireIfAlready == nil || *p.FireIfAlready {
		t.Fatalf("fired job params = %+v (a job that already delivered must wait for a real crossing)", p)
	}
	s, _ = w.specFor(pn.ID)
	_ = json.Unmarshal(s.Params, &p)
	if p.FireIfAlready == nil || !*p.FireIfAlready || p.CooldownMin != 60 {
		t.Fatalf("never-fired job params = %+v", p)
	}

	var ip triggers.IndicatorCrossParams
	s, _ = w.specFor(im.ID)
	_ = json.Unmarshal(s.Params, &ip)
	if ip.Indicator != triggers.IndMACDLine || ip.Period != 0 || ip.Timeframe != "1h" {
		t.Fatalf("MACD job params = %+v (the legacy alert compared the MACD line)", ip)
	}
	s, _ = w.specFor(il.ID)
	_ = json.Unmarshal(s.Params, &ip)
	if ip.Indicator != triggers.IndSMA || ip.Period != 20 {
		t.Fatalf("SMA20 job params = %+v", ip)
	}
	// The refused jobs keep running on cron.
	for _, j := range w.cron.ListJobs(true) {
		if !j.Enabled {
			t.Fatalf("job %s was switched off by an import", j.Name)
		}
	}
}

func TestPromotion_DisablesCron_Rollback_ReEnables(t *testing.T) {
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	b := w.indicator("indicator_alert:b", defaultIndicator())

	w.sync(Shadow)
	for _, id := range []string{a.ID, b.ID} {
		if p := w.pair(id); !p.jobEnabled || p.specMode != triggers.ModeShadow {
			t.Fatalf("shadow: %v", p)
		}
	}

	rep := w.sync(Live)
	if rep.Promoted != 2 {
		t.Fatalf("promotion report = %+v", rep)
	}
	for _, id := range []string{a.ID, b.ID} {
		p := w.pair(id)
		if !p.jobExists || p.jobEnabled || !p.marked || p.specMode != triggers.ModeLive {
			t.Fatalf("promoted: %v (the job must be kept, disabled and marked; the spec live)", p)
		}
	}
	if j := w.cron.GetJob(a.ID); j.State.Migrated.Target == "" || j.State.Migrated.Fingerprint == "" {
		t.Fatalf("marker = %+v", j.State.Migrated)
	}
	// Idempotent in live too.
	if again := w.sync(Live); again != (Report{}) {
		t.Fatalf("second live run did something: %+v", again)
	}

	rep = w.sync(Shadow)
	if rep.RolledBack != 4 { // two jobs switched on, two specs back to shadow
		t.Fatalf("rollback report = %+v", rep)
	}
	for _, id := range []string{a.ID, b.ID} {
		p := w.pair(id)
		if !p.jobEnabled || p.marked || p.specMode != triggers.ModeShadow {
			t.Fatalf("rolled back: %v", p)
		}
	}
	if j := w.cron.GetJob(a.ID); j.State.NextRunAtMS == nil {
		t.Fatal("a re-enabled job has no next run")
	}

	// And forward again.
	w.sync(Live)
	if p := w.pair(a.ID); p.jobEnabled || p.specMode != triggers.ModeLive {
		t.Fatalf("promoted again: %v", p)
	}
}

func TestPromotion_NewJobIsPromotedInTheSameRun(t *testing.T) {
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	rep := w.sync(Live)
	if rep.Imported != 1 || rep.Promoted != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if p := w.pair(a.ID); p.jobEnabled || p.specMode != triggers.ModeLive {
		t.Fatalf("pair = %v", p)
	}
}

func TestPromotion_OnlyOneShotsStartFresh(t *testing.T) {
	w := newWorld(t)
	rec := w.price("price_alert:rec", defaultPrice())
	one := defaultPrice()
	one.Recurring, one.Threshold = false, 70000
	os := w.price("price_alert:one", one)
	w.sync(Shadow)

	r := &recorder{Specs: w.reg}
	if _, err := New(AlertFamily(), w.cron, r).Sync(context.Background(), Live); err != nil {
		t.Fatal(err)
	}
	if len(r.fresh) != 1 || r.fresh[0] != OriginPrefix+os.ID {
		t.Fatalf("fresh updates = %v, want only the one-shot's (its job is still here, so it was never delivered)", r.fresh)
	}
	if len(r.updates) != 1 || r.updates[0] != OriginPrefix+rec.ID {
		t.Fatalf("plain updates = %v, want only the recurring alert's", r.updates)
	}
}

func TestPromotion_InterruptedAtEveryStep(t *testing.T) {
	setup := func() (*world, []string) {
		w := newWorld(t)
		a := w.price("price_alert:a", defaultPrice())
		one := defaultPrice()
		one.Recurring, one.Threshold = false, 70000
		b := w.price("price_alert:b", one)
		c := w.indicator("indicator_alert:c", defaultIndicator())
		w.sync(Shadow)
		return w, []string{a.ID, b.ID, c.ID}
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
			// Between any two writes the alert is alive: the job delivers, or the live
			// spec does. The worst case is both, a duplicate.
			for _, id := range ids {
				if p := w.pair(id); !p.aliveWithLiveRegistry() {
					t.Fatalf("after a crash at write %d, alert %s is silent: %v", k, id, p)
				}
			}
			// Restart as live: finishes the promotion.
			w.reg = w.newRegistry()
			w.sync(Live)
			for _, id := range ids {
				if p := w.pair(id); p.jobEnabled || !p.marked || p.specMode != triggers.ModeLive {
					t.Fatalf("recovered as live: %v", p)
				}
			}
		})
		t.Run(fmt.Sprintf("crash at write %d, then rollback", k), func(t *testing.T) {
			w, ids := setup()
			_, _ = w.syncWithCrash(Live, k)
			w.reg = w.newRegistry()
			w.sync(Shadow)
			for _, id := range ids {
				if p := w.pair(id); !p.jobEnabled || p.marked || p.specMode != triggers.ModeShadow {
					t.Fatalf("rolled back after an interrupted promotion: %v", p)
				}
			}
		})
	}
}

func TestRollback_InterruptedAtEveryStep(t *testing.T) {
	setup := func() (*world, []string) {
		w := newWorld(t)
		a := w.price("price_alert:a", defaultPrice())
		c := w.indicator("indicator_alert:c", defaultIndicator())
		w.sync(Live)
		return w, []string{a.ID, c.ID}
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
			// The job is switched on before its spec leaves live, so wherever the run
			// stopped, no alert that was touched is off in both paths: a job still off
			// has not been touched yet, and the next run (below) restores it.
			for _, id := range ids {
				p := w.pair(id)
				if !p.jobEnabled && p.specMode != triggers.ModeLive {
					t.Fatalf("after a crash at write %d, alert %s is off in both paths: %v", k, id, p)
				}
			}
			w.reg = w.newRegistry()
			w.sync(Shadow)
			for _, id := range ids {
				if p := w.pair(id); !p.jobEnabled || p.marked || p.specMode != triggers.ModeShadow {
					t.Fatalf("recovered as shadow: %v", p)
				}
			}
			// And back to live again from any of those states.
			w.sync(Live)
			for _, id := range ids {
				if p := w.pair(id); p.jobEnabled || !p.marked || p.specMode != triggers.ModeLive {
					t.Fatalf("promoted again: %v", p)
				}
			}
		})
	}
}

func TestRollback_LeavesUserChangedJobsAlone(t *testing.T) {
	w := newWorld(t)
	edited := w.price("price_alert:edited", defaultPrice())
	untouched := w.price("price_alert:untouched", defaultPrice())
	deleted := w.price("price_alert:deleted", defaultPrice())
	w.sync(Live)

	// While live the user edits one hidden job, and deletes another.
	j := w.cron.GetJob(edited.ID)
	var p legacyPrice
	_ = json.Unmarshal([]byte(j.Payload.Message), &p)
	p.Threshold = 99999
	b, _ := json.Marshal(p)
	j.Payload.Message = string(b)
	if err := w.cron.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	w.cron.RemoveJob(deleted.ID)

	rep := w.sync(Shadow)
	if rep.Left != 1 {
		t.Fatalf("report = %+v, want 1 job left", rep)
	}
	if got := w.pair(edited.ID); got.jobEnabled || got.marked {
		t.Fatalf("the edited job was touched: %v (it must stay as the user left it: off, marker cleared)", got)
	}
	if got := w.cron.GetJob(edited.ID); got.Payload.Message != string(b) {
		t.Fatal("the edited job's payload was changed")
	}
	if got := w.pair(deleted.ID); got.jobExists || got.specExists {
		t.Fatalf("the deleted job came back: %v", got)
	}
	if got := w.pair(untouched.ID); !got.jobEnabled || got.marked || got.specMode != triggers.ModeShadow {
		t.Fatalf("the untouched job was not rolled back: %v", got)
	}
}

func TestRollback_AJobThatRanInBetweenIsNotAnEdit(t *testing.T) {
	// The legacy handler rewrites last_fired_ms and consecutive_errors in the payload
	// as it runs; a run in the window must not count as the user's edit.
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	w.sync(Live)
	j := w.cron.GetJob(a.ID)
	var p legacyPrice
	_ = json.Unmarshal([]byte(j.Payload.Message), &p)
	p.LastFiredMS = 777
	b, _ := json.Marshal(map[string]any{"provider": p.ProviderID, "symbol": p.Symbol, "condition": p.Condition,
		"threshold": p.Threshold, "recurring": p.Recurring, "alert_msg": p.AlertMsg, "account": p.Account,
		"last_fired_ms": 777, "consecutive_errors": 4})
	j.Payload.Message = string(b)
	if err := w.cron.UpdateJob(j); err != nil {
		t.Fatal(err)
	}
	if rep := w.sync(Shadow); rep.Left != 0 || rep.RolledBack != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if p := w.pair(a.ID); !p.jobEnabled {
		t.Fatalf("pair = %v", p)
	}
}

func TestFinishedOneShot_IsRemovedNotRestored(t *testing.T) {
	w := newWorld(t)
	one := defaultPrice()
	one.Recurring = false
	a := w.price("price_alert:one", one)
	w.sync(Live)
	s, _ := w.specFor(a.ID)
	s.Mode = triggers.ModeOff // what a live firing's TurnOff does
	if err := w.reg.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	// With a registry (either mode): spec and job go, nothing is re-enabled.
	for _, mode := range []Mode{Live, Shadow} {
		w2 := newWorld(t)
		a2 := w2.price("price_alert:one", one)
		w2.sync(Live)
		s2, _ := w2.specFor(a2.ID)
		s2.Mode = triggers.ModeOff
		_ = w2.reg.Update(context.Background(), s2)
		rep := w2.sync(mode)
		if rep.Finished != 1 {
			t.Fatalf("mode %d: report = %+v", mode, rep)
		}
		if p := w2.pair(a2.ID); p.jobExists || p.specExists {
			t.Fatalf("mode %d: %v (a one-shot that fired must not come back)", mode, p)
		}
	}

	// Without a registry: RestoreAll, told which specs are done.
	rep, err := RestoreAll(w.cron, AlertFamily(), func(specID string) bool { return specID == s.ID })
	if err != nil || rep.Finished != 1 || rep.RolledBack != 0 {
		t.Fatalf("RestoreAll = %+v, %v", rep, err)
	}
	if p := w.pair(a.ID); p.jobExists {
		t.Fatalf("pair = %v", p)
	}
}

func TestRestoreAll_WithoutARegistry(t *testing.T) {
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	b := w.indicator("indicator_alert:b", defaultIndicator())
	keep := w.price("price_alert:keep", defaultPrice())
	w.sync(Live)
	// A job the migration did not switch off stays as it is.
	user := w.price("price_alert:user-off", defaultPrice())
	uj := w.cron.GetJob(user.ID)
	uj.Enabled = false
	_ = w.cron.UpdateJob(uj)

	rep, err := RestoreAll(w.cron, AlertFamily(), nil)
	if err != nil || rep.RolledBack != 3 {
		t.Fatalf("RestoreAll = %+v, %v", rep, err)
	}
	for _, id := range []string{a.ID, b.ID, keep.ID} {
		if j := w.cron.GetJob(id); !j.Enabled || j.State.Migrated != nil {
			t.Fatalf("job %s = %+v", id, j)
		}
	}
	if j := w.cron.GetJob(user.ID); j.Enabled {
		t.Fatal("a job the user switched off was switched on")
	}
	// The specs are untouched by it (no registry to ask); the next registry Sync
	// brings them back to shadow.
	if p := w.pair(a.ID); p.specMode != triggers.ModeLive {
		t.Fatalf("pair = %v", p)
	}
	w.sync(Shadow)
	if p := w.pair(a.ID); !p.jobEnabled || p.specMode != triggers.ModeShadow {
		t.Fatalf("pair = %v", p)
	}
}

func TestMarkerWithoutSpec_IsHandedBack(t *testing.T) {
	remove := func(w *world, id string) {
		s, _ := w.specFor(id)
		if err := w.reg.Remove(context.Background(), s.ID); err != nil { // the table was lost or cleaned
			t.Fatal(err)
		}
	}
	t.Run("shadow: the job delivers again", func(t *testing.T) {
		w := newWorld(t)
		a := w.price("price_alert:a", defaultPrice())
		w.sync(Live)
		remove(w, a.ID)
		w.sync(Shadow)
		// Handed back, then imported as new, in shadow.
		if p := w.pair(a.ID); !p.jobEnabled || p.marked || p.specMode != triggers.ModeShadow {
			t.Fatalf("pair = %v", p)
		}
	})
	t.Run("live: adopted again, never lost in between", func(t *testing.T) {
		w := newWorld(t)
		a := w.price("price_alert:a", defaultPrice())
		w.sync(Live)
		remove(w, a.ID)
		w.sync(Live)
		if p := w.pair(a.ID); p.jobEnabled || !p.marked || p.specMode != triggers.ModeLive {
			t.Fatalf("pair = %v", p)
		}
	})
}

func TestShadow_FollowsTheJob(t *testing.T) {
	w := newWorld(t)
	gone := w.price("price_alert:gone", defaultPrice())
	off := w.price("price_alert:off", defaultPrice())
	edited := w.price("price_alert:edited", defaultPrice())
	w.sync(Shadow)

	w.cron.RemoveJob(gone.ID) // a legacy one-shot that fired removes itself, or the user cancelled it
	oj := w.cron.GetJob(off.ID)
	oj.Enabled = false
	_ = w.cron.UpdateJob(oj)
	ej := w.cron.GetJob(edited.ID)
	var p legacyPrice
	_ = json.Unmarshal([]byte(ej.Payload.Message), &p)
	p.Threshold = 65000
	b, _ := json.Marshal(p)
	ej.Payload.Message = string(b)
	_ = w.cron.UpdateJob(ej)

	rep := w.sync(Shadow)
	if rep.Removed != 2 || rep.Retuned != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if w.pair(gone.ID).specExists || w.pair(off.ID).specExists {
		t.Fatal("a spec outlived its job")
	}
	s, _ := w.specFor(edited.ID)
	var sp triggers.PriceCrossParams
	_ = json.Unmarshal(s.Params, &sp)
	if sp.Threshold != 65000 {
		t.Fatalf("the spec did not follow the edited job: %+v", sp)
	}
}

func TestSync_OneStuckJobDoesNotStopTheOthers(t *testing.T) {
	w := newWorld(t)
	a := w.price("price_alert:a", defaultPrice())
	b := w.price("price_alert:b", defaultPrice())
	w.sync(Shadow)
	// Remove the registry's row under the migrator: its Update for a fails, b works.
	sa, _ := w.specFor(a.ID)
	if err := w.reg.Remove(context.Background(), sa.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := MigrateAlertJobs(context.Background(), w.cron, w.reg, Live)
	_ = err
	if p := w.pair(b.ID); p.jobEnabled || p.specMode != triggers.ModeLive {
		t.Fatalf("b = %v (report %+v)", p, rep)
	}
}
