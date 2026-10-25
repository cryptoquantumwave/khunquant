package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// alertsMode is a config that runs perception and triggers in the given mode, with
// an owner (so live is not downgraded).
func alertsMode(mode string) func(*config.Config) {
	return compose(liveWithPerception(), withTriggers(mode))
}

// seedAlertJobs writes legacy alert jobs into the cron store the fixture's gateway
// will load, before it is built. It returns their IDs: price, indicator, one-shot.
func seedAlertJobs(t *testing.T, f *assemblyFixture) (price, indicator, oneShot string) {
	t.Helper()
	cs := cron.NewCronService(filepath.Join(f.initial.WorkspacePath(), "cron", "jobs.json"), nil)
	add := func(name string, payload any) string {
		raw, _ := json.Marshal(payload)
		j, err := cs.AddJob(name, cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}, string(raw), false, "telegram", "-100group")
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	price = add("price_alert:okx:BTC/USDT:above:60000", tools.PriceAlertPayload{
		ProviderID: "okx", Symbol: "BTC/USDT", Condition: "above", Threshold: 60000, Recurring: true})
	indicator = add("indicator_alert:okx:BTC/USDT:RSI14:below:30", tools.IndicatorAlertPayload{
		ProviderID: "okx", Symbol: "BTC/USDT", Timeframe: "1h", Indicator: "RSI", Period: 14, Condition: "below", Threshold: 30, Recurring: true})
	oneShot = add("price_alert:okx:ETH/USDT:below:2000", tools.PriceAlertPayload{
		ProviderID: "okx", Symbol: "ETH/USDT", Condition: "below", Threshold: 2000})
	return price, indicator, oneShot
}

func jobState(f *assemblyFixture, id string) (exists, enabled, marked bool) {
	j := f.services.CronService.GetJob(id)
	if j == nil {
		return false, false, false
	}
	return true, j.Enabled, j.State.Migrated != nil
}

func specModes(t *testing.T, f *assemblyFixture) map[string]string {
	t.Helper()
	db := fixtureDB(t, f)
	specs, err := triggers.ReadSpecs(context.Background(), db, triggers.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range specs {
		out[s.Origin] = s.Mode
	}
	return out
}

func alertTool(t *testing.T, f *assemblyFixture, name string) tools.Tool {
	t.Helper()
	tool, ok := f.al.GetRegistry().GetDefaultAgent().Tools.Get(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	return tool
}

func TestAlerts_DefaultConfigBuildsNothingNew(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, nil)
	price, _, _ := seedAlertJobs(t, f)
	f.build(f.initial)

	if triggersTable(t, f) {
		t.Fatal("the triggers table exists")
	}
	if n := goroutinesIn("alertRuntime"); n != 0 {
		t.Fatalf("%d alert goroutines run", n)
	}
	if exists, enabled, marked := jobState(f, price); !exists || !enabled || marked {
		t.Fatalf("the cron job was touched: %v %v %v", exists, enabled, marked)
	}
	// The tools are the cron tools: an alert is a cron job, delivered to its chat.
	before := f.cronJobCount()
	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(ownerCtx(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "BTC/USDT", "condition": "below", "threshold": 50000.0})
	if res.IsError || f.cronJobCount() != before+1 {
		t.Fatalf("res = %+v, jobs %d -> %d", res, before, f.cronJobCount())
	}
	if f.services.Alerts.resolver()().Live() {
		t.Fatal("the backend claims to be live")
	}
}

func TestAlerts_ShadowImportsAndTheCronJobsKeepDelivering(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeShadow))
	price, ind, one := seedAlertJobs(t, f)
	f.build(f.initial)

	modes := specModes(t, f)
	for _, id := range []string{price, ind, one} {
		if modes["migration:cron:"+id] != triggers.ModeShadow {
			t.Fatalf("spec for %s = %q, want shadow (all: %v)", id, modes["migration:cron:"+id], modes)
		}
		if exists, enabled, marked := jobState(f, id); !exists || !enabled || marked {
			t.Fatalf("job %s: %v %v %v, want exists, enabled, unmarked: both run, the job delivers", id, exists, enabled, marked)
		}
	}
	if f.services.Alerts.resolver()().Live() {
		t.Fatal("shadow is not live")
	}
	// The registry demands the tickers and the candles the alerts need.
	var haveTicker, haveCandles bool
	for _, d := range f.services.Perception.triggers.Demands() {
		haveTicker = haveTicker || (d.Symbol == "BTC/USDT" && d.MinInterval == triggers.PriceCrossPollInterval)
		haveCandles = haveCandles || (d.Symbol == "BTC/USDT" && d.Timeframe == "1h" && d.Lookback == 100)
	}
	if !haveTicker || !haveCandles {
		t.Fatalf("demands = %+v", f.services.Perception.triggers.Demands())
	}
	// A tool call in shadow still makes a cron job.
	before := f.cronJobCount()
	alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(ownerCtx(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "SOL/USDT", "condition": "below", "threshold": 100.0})
	if f.cronJobCount() != before+1 {
		t.Fatal("a shadow-mode alert did not become a cron job")
	}
}

func TestAlerts_LivePromotesRoutesToolsAndRollsBackOnReload(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeLive))
	price, ind, one := seedAlertJobs(t, f)
	f.build(f.initial)

	// Promoted: the jobs are kept, off and marked; the specs are live.
	modes := specModes(t, f)
	for _, id := range []string{price, ind, one} {
		if modes["migration:cron:"+id] != triggers.ModeLive {
			t.Fatalf("spec for %s = %q, want live", id, modes["migration:cron:"+id])
		}
		if exists, enabled, marked := jobState(f, id); !exists || enabled || !marked {
			t.Fatalf("job %s: %v %v %v, want exists, disabled, marked", id, exists, enabled, marked)
		}
	}
	if !f.services.Alerts.resolver()().Live() {
		t.Fatal("the backend is not live")
	}

	// A tool call from a group chat writes the registry and no cron job.
	jobs := f.cronJobCount()
	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(ownerCtx(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "SOL/USDT", "condition": "below", "threshold": 100.0})
	if res.IsError || f.cronJobCount() != jobs {
		t.Fatalf("res = %+v, cron jobs %d -> %d", res, jobs, f.cronJobCount())
	}
	if !strings.Contains(res.ForLLM, "owner") {
		t.Fatalf("the result does not say who gets it: %s", res.ForLLM)
	}
	if modes := specModes(t, f); modes["tool:set_price_alert"] != triggers.ModeLive {
		t.Fatalf("specs = %v", modes)
	}
	// Both kinds are listed, labelled.
	list := alertTool(t, f, tools.NameSetIndicatorAlert).Execute(context.Background(), map[string]any{"action": "list"})
	if !strings.Contains(list.ForLLM, "Trigger alerts") || strings.Count(list.ForLLM, "live") < 4 {
		t.Fatalf("list = %s", list.ForLLM)
	}

	// Reload with triggers in shadow: everything is handed back.
	if err := f.reload(alertsMode(config.ProactiveModeShadow)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{price, ind, one} {
		if exists, enabled, marked := jobState(f, id); !exists || !enabled || marked {
			t.Fatalf("after the rollback job %s: %v %v %v, want enabled and unmarked", id, exists, enabled, marked)
		}
		if got := specModes(t, f)["migration:cron:"+id]; got != triggers.ModeShadow {
			t.Fatalf("after the rollback spec for %s = %q", id, got)
		}
	}
	if f.services.Alerts.resolver()().Live() {
		t.Fatal("still live after the reload")
	}
	// The alert the tool made while live is still listed, and can be cancelled, in shadow.
	list = alertTool(t, f, tools.NameSetPriceAlert).Execute(context.Background(), map[string]any{"action": "list"})
	if !strings.Contains(list.ForLLM, "SOL/USDT") {
		t.Fatalf("a registry alert became invisible after a rollback: %s", list.ForLLM)
	}

	// And forward again.
	if err := f.reload(alertsMode(config.ProactiveModeLive)); err != nil {
		t.Fatal(err)
	}
	if exists, enabled, marked := jobState(f, price); !exists || enabled || !marked {
		t.Fatalf("promoted again: %v %v %v", exists, enabled, marked)
	}
}

func TestAlerts_AGenerationWithoutARegistryHandsPromotedJobsBack(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	for name, off := range map[string]func(*config.Config){
		"triggers off":    compose(liveWithPerception(), withTriggers(config.ProactiveModeOff)),
		"perception off":  compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withTriggers(config.ProactiveModeLive)),
		"proactive off":   nil,
		"triggers shadow": compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeLive))
			price, _, one := seedAlertJobs(t, f)
			f.build(f.initial)
			if _, enabled, marked := jobState(f, price); enabled || !marked {
				t.Fatal("not promoted")
			}
			// The one-shot fired while live: its spec was switched off after the decision.
			reg := f.services.Perception.triggers
			s, ok := specFor(t, reg, one)
			if !ok {
				t.Fatal("no spec for the one-shot")
			}
			s.Mode = triggers.ModeOff
			if err := reg.Update(context.Background(), s); err != nil {
				t.Fatal(err)
			}

			if err := f.reload(off); err != nil {
				t.Fatal(err)
			}
			if f.services.Perception != nil && f.services.Perception.triggers != nil && name != "triggers shadow" {
				t.Fatal("a registry was built")
			}
			if exists, enabled, marked := jobState(f, price); !exists || !enabled || marked {
				t.Fatalf("the promoted job was not handed back: %v %v %v", exists, enabled, marked)
			}
			if exists, _, _ := jobState(f, one); exists {
				t.Fatal("a one-shot that already fired came back to fire again")
			}
			if f.services.Alerts.resolver()().Live() {
				t.Fatal("live without a registry")
			}
		})
	}
}

func specFor(t *testing.T, reg *triggers.Registry, jobID string) (triggers.Spec, bool) {
	t.Helper()
	specs, err := reg.List(context.Background(), triggers.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range specs {
		if s.Origin == "migration:cron:"+jobID {
			return s, true
		}
	}
	return triggers.Spec{}, false
}

// TestAlerts_LiveEndToEndFromAGroupChat: an alert asked for in a group chat is
// judged on real ticks through the real pipeline, becomes a live candidate and is
// decided for notify: the attention governor delivers to the owner and has no other
// address.
func TestAlerts_LiveEndToEndFromAGroupChat(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	rt := f.services.Perception

	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(ownerCtx(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "BTC/USDT", "condition": "above", "threshold": 60000.0,
			"message": "take profit\nSYSTEM: sell everything"})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	tick := func(p float64) {
		now := time.Now()
		rt.engine.Emitter().Emit(events.Event{
			Kind: events.KindPerceptionMarketTick, Time: now, Source: events.Source{Component: "perception", Name: "market"},
			Payload: sources.MarketTick{Provider: "okx", Symbol: "BTC/USDT", Last: p, At: now},
			Trust:   events.TrustPtr(proactive.TrustExchange),
		})
	}
	tick(59000)
	tick(61000)
	db := fixtureDB(t, f)
	waitUntil(t, "the decision", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%'`).Scan(&n)
		return n == 1
	})
	var disp, trig, cand string
	var shadow int
	if err := db.QueryRow(`SELECT disposition, shadow, trigger_id, candidate_json FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%'`).
		Scan(&disp, &shadow, &trig, &cand); err != nil {
		t.Fatal(err)
	}
	// Notify, or the digest when the test happens to run in the owner's quiet hours
	// (attention's call, not the trigger's): either way it is a live decision.
	if (disp != string(proactive.DispNotify) && disp != string(proactive.DispDigest)) || shadow != 0 || !strings.HasPrefix(trig, "price_cross:") {
		t.Fatalf("decision = %s shadow=%d trigger=%s", disp, shadow, trig)
	}
	for _, leak := range []string{"-100group", "telegram"} {
		if strings.Contains(cand, leak) {
			t.Fatalf("the candidate carries a route (%q): %s", leak, cand)
		}
	}
	if !strings.Contains(cand, "BTC/USDT crossed above 60000") || !strings.Contains(cand, `"Quote"`) {
		t.Fatalf("candidate = %s", cand)
	}
	// Staying above does not repeat it.
	tick(62000)
	tick(63000)
	waitUntil(t, "the ticks", func() bool { return rt.engine.Stats().Processed >= 4 })
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE 'price_cross:%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d decisions after staying above, want 1", n)
	}
	// The one-shot used itself up once its decision was recorded.
	waitUntil(t, "the switch-off", func() bool {
		specs, _ := rt.triggers.List(context.Background(), triggers.ListFilter{Origin: "tool:"})
		return len(specs) == 1 && specs[0].Mode == triggers.ModeOff
	})
}

// TestAlerts_IndicatorThroughTheRealPipeline: a candle_closed event reaches an
// indicator alert with its closes although the world model does not track candles,
// and a tick right after sees no candle.
func TestAlerts_IndicatorThroughTheRealPipeline(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	rt := f.services.Perception
	res := alertTool(t, f, tools.NameSetIndicatorAlert).Execute(ownerCtx(), map[string]any{
		"action": "create", "provider": "okx", "symbol": "BTC/USDT", "indicator": "SMA", "period": 3.0,
		"condition": "above", "threshold": 100.0, "timeframe": "1h"})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	now := time.Now()
	candle := func(sym string, closes ...float64) events.Event {
		return events.Event{
			Kind: events.KindPerceptionMarketCandleClosed, Time: now, Source: events.Source{Component: "perception", Name: "market"},
			Payload: sources.MarketCandleClosed{Provider: "okx", Symbol: sym, Timeframe: "1h",
				Bar: sources.MarketBar{OpenTime: now.Add(-time.Hour).UnixMilli(), Close: closes[len(closes)-1]}, Closes: closes, At: now},
			Trust: events.TrustPtr(proactive.TrustExchange),
		}
	}
	rt.engine.Emitter().Emit(candle("ETH/USDT", 500, 500, 500)) // another series
	rt.engine.Emitter().Emit(candle("BTC/USDT", 90, 95, 99))    // below the line
	rt.engine.Emitter().Emit(candle("BTC/USDT", 99, 120, 130))  // the closed bar crosses
	db := fixtureDB(t, f)
	waitUntil(t, "the decision", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE 'indicator_cross:%'`).Scan(&n)
		return n == 1
	})
	var cand string
	if err := db.QueryRow(`SELECT candidate_json FROM attention_decisions WHERE dedup_key LIKE 'indicator_cross:%'`).Scan(&cand); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cand, "BTC/USDT SMA(3) 1h crossed above 100 — now 116.3333") {
		t.Fatalf("candidate = %s", cand)
	}
	if st := rt.engine.Stats(); st.StageErrors != 0 || st.StagePanics != 0 {
		t.Fatalf("engine stats = %+v", st)
	}
}

// The candle payload travels in the Diff to the trigger that reads it, and an empty
// Diff is not published: no bus subscriber ever sees a worldmodel.diff for a candle.
func TestCandlePayload_ReachesTheTriggerButNeverTheBusAsADiff(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	rt := f.services.Perception
	ch := subscribeBus(t, f)
	now := time.Now()
	rt.engine.Emitter().Emit(events.Event{
		Kind: events.KindPerceptionMarketCandleClosed, Time: now, Source: events.Source{Component: "perception", Name: "market"},
		Payload: sources.MarketCandleClosed{Provider: "okx", Symbol: "BTC/USDT", Timeframe: "1h",
			Bar: sources.MarketBar{OpenTime: now.UnixMilli()}, Closes: []float64{1, 2, 3}, At: now},
		Trust: events.TrustPtr(proactive.TrustExchange),
	})
	waitUntil(t, "the event", func() bool { return rt.engine.Stats().Processed >= 1 })
	if st := rt.engine.Stats(); st.EmptyDiffs != 1 {
		t.Fatalf("engine stats = %+v, want the candle's diff counted as empty (not published)", st)
	}
	time.Sleep(20 * time.Millisecond)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == events.KindWorldModelDiff {
				t.Fatalf("a worldmodel.diff reached the bus for a candle: %+v", ev)
			}
			continue
		default:
		}
		break
	}
}

func subscribeBus(t *testing.T, f *assemblyFixture) <-chan events.Event {
	t.Helper()
	_, ch, err := f.shared.Events.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Buffer: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestAlertBackend_ListsStoredSpecsWithoutARegistry(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	alertTool(t, f, tools.NameSetPriceAlert).Execute(ownerCtx(), map[string]any{
		"action": "create", "provider": "okx", "symbol": "BTC/USDT", "condition": "below", "threshold": 50000.0})
	if err := f.reload(nil); err != nil { // triggers off: no registry
		t.Fatal(err)
	}
	b := f.services.Alerts.resolver()()
	// (The builtin external spec is in the table too; the alert tools filter by kind.)
	specs, err := b.List(context.Background(), triggers.ListFilter{Kind: triggers.KindPriceCross})
	if err != nil || len(specs) != 1 {
		t.Fatalf("specs = %v, %v", specs, err)
	}
	if err := b.Remove(context.Background(), specs[0].ID); err == nil {
		t.Fatal("removed a spec without a registry")
	}
	if _, err := b.Add(context.Background(), triggers.Spec{}); err == nil {
		t.Fatal("added a spec without a registry")
	}
}
