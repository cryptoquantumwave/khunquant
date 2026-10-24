package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers/intents"
)

// The end-to-end tests of MR-302: the real assembly, the real engine, world model,
// registry, compiler and attention governor, with a fake clock under the clock source.

const gwTitleMarker = "ZXQ-INTENT-TITLE-5521"

func intentsOn(cfg *config.Config) {
	cfg.Proactive.Intents.Enabled = true
	cfg.Proactive.Attention.GroupWindowSeconds = 1
}

// intentsLive is a config that runs perception, live triggers, live attention and the
// intent ledger, with an owner.
func intentsLive() func(*config.Config) {
	return compose(liveWithPerception(), withTriggers(config.ProactiveModeLive), intentsOn)
}

func quickCompiler(t *testing.T) {
	t.Helper()
	prev := intentCompilerOptions
	intentCompilerOptions = func() []intents.Option { return []intents.Option{intents.WithDebounce(5 * time.Millisecond)} }
	t.Cleanup(func() { intentCompilerOptions = prev })
}

// useClockSource replaces the sources with the real clock source over a fake clock and
// the gateway's own timer store, so that reminders are scheduled, persisted and fired
// the way production does it.
func useClockSource(t *testing.T, clk *perceptiontest.Clock) {
	t.Helper()
	prev := perceptionSources
	perceptionSources = func(d assemblyDeps) ([]perception.Source, bool) {
		return []perception.Source{sources.NewClockSource(sources.ClockOptions{
			Clock: clk, Timers: clockTimerStore(d), Location: profileZone(d),
		})}, false
	}
	t.Cleanup(func() { perceptionSources = prev })
}

// advanceUntil moves the fake clock forward by d and then lets it tick a little until
// cond holds: a source that had not yet gone back to sleep when the clock moved
// sleeps on a timer that only the next tick fires, as a real one would.
func advanceUntil(t *testing.T, clk *perceptiontest.Clock, d time.Duration, cond func() bool) {
	t.Helper()
	clk.Advance(d)
	waitUntil(t, "the fake clock to be seen", func() bool {
		if cond() {
			return true
		}
		clk.Advance(2 * time.Second)
		return false
	})
}

func ownerCtx() context.Context {
	ctx := tools.WithToolOrigin(context.Background(), tools.OriginRemote)
	ctx = tools.WithToolSender(ctx, "telegram:1001")
	return tools.WithToolContext(ctx, "telegram", "1001")
}

func waitCompiler(t *testing.T, f *assemblyFixture) *intents.Compiler {
	t.Helper()
	waitUntil(t, "the intent compiler", func() bool {
		rt := f.services.Perception
		return rt != nil && rt.intentComp != nil && rt.intentComp.Running()
	})
	return f.services.Perception.intentComp
}

func createIntent(t *testing.T, f *assemblyFixture, args map[string]any) (id, answer string) {
	t.Helper()
	res := alertTool(t, f, tools.NameCreateIntent).Execute(ownerCtx(), args)
	if res.IsError {
		t.Fatalf("create_intent: %s", res.ForLLM)
	}
	db := fixtureDB(t, f)
	store, err := intent.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListIntents(context.Background(), intent.IntentFilter{NewestFirst: true, Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	return rows[0].ID, res.ForLLM
}

func watchArgsFor(symbol string, below float64, title string) map[string]any {
	return map[string]any{
		"type": "watch", "title": title, "user_requested": true,
		"trigger": map[string]any{"kind": "price_cross", "params": map[string]any{
			"provider": "okx", "symbol": symbol, "direction": "below", "threshold": below}},
	}
}

func emitTick(rt *perceptionRuntime, symbol string, price float64) {
	now := time.Now()
	rt.engine.Emitter().Emit(events.Event{
		Kind: events.KindPerceptionMarketTick, Time: now, Source: events.Source{Component: "perception", Name: "market"},
		Payload: sources.MarketTick{Provider: "okx", Symbol: symbol, Last: price, At: now},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	})
}

type decisionRow struct {
	Disposition, Reason string
	Shadow              bool
	Candidate           proactive.Candidate
}

func waitDecision(t *testing.T, db *sql.DB, keyLike string) decisionRow {
	t.Helper()
	waitUntil(t, "the decision for "+keyLike, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE ?`, keyLike).Scan(&n)
		return n >= 1
	})
	var d decisionRow
	var shadow int
	var raw string
	if err := db.QueryRow(`SELECT disposition, reason, shadow, candidate_json FROM attention_decisions WHERE dedup_key LIKE ? ORDER BY rowid LIMIT 1`, keyLike).
		Scan(&d.Disposition, &d.Reason, &shadow, &raw); err != nil {
		t.Fatal(err)
	}
	d.Shadow = shadow != 0
	if err := json.Unmarshal([]byte(raw), &d.Candidate); err != nil {
		t.Fatalf("candidate_json: %v", err)
	}
	return d
}

func intentStatus(t *testing.T, f *assemblyFixture, id string) intent.Intent {
	t.Helper()
	store, err := intent.NewStore(context.Background(), fixtureDB(t, f))
	if err != nil {
		t.Fatal(err)
	}
	it, err := store.GetIntent(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func specOf(t *testing.T, f *assemblyFixture, id string) (triggers.Spec, bool) {
	t.Helper()
	specs, err := triggers.ReadSpecs(context.Background(), fixtureDB(t, f), triggers.ListFilter{IntentID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		return triggers.Spec{}, false
	}
	return specs[0], true
}

// assertTitleOnlyInQuote: the intent's title reaches the owner in the Quote and in no
// other field a person or a model reads.
func assertTitleOnlyInQuote(t *testing.T, c proactive.Candidate) {
	t.Helper()
	if c.Quote != gwTitleMarker {
		t.Errorf("Quote = %q, want the title", c.Quote)
	}
	ev, _ := json.Marshal(c.Evidence)
	for name, field := range map[string]string{
		"Title": c.Title, "Subject": c.Subject, "Detail": c.Detail, "DedupKey": c.DedupKey, "TriggerID": c.TriggerID, "Evidence": string(ev),
	} {
		if strings.Contains(field, gwTitleMarker) {
			t.Errorf("%s holds the intent's title: %q", name, field)
		}
	}
}

// recordingRunner is a model turn that records what it was asked.
type recordingRunner struct{ reqs []engine.TurnRequest }

func (r *recordingRunner) RunProactiveTurn(_ context.Context, req engine.TurnRequest) (engine.TurnResult, error) {
	r.reqs = append(r.reqs, req)
	return engine.TurnResult{}, nil
}

// Story 4: "Remind me if SOL goes under 120". The owner says it in a turn; it is
// active, compiles into a price_cross trigger, fires once when the price crosses, is
// delivered without a model, and the intent is done; its spec goes.
func TestIntentStory4_RemindMeIfSOLGoesUnder120(t *testing.T) {
	quickCompiler(t)
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, intentsLive())
	waitCompiler(t, f)
	rt := f.services.Perception

	id, answer := createIntent(t, f, watchArgsFor("SOL/USDT", 120, gwTitleMarker))
	if !strings.Contains(answer, "set up and watching") {
		t.Fatalf("the answer does not say it works: %s", answer)
	}
	waitUntil(t, "the compiled trigger", func() bool { _, ok := specOf(t, f, id); return ok })
	spec, _ := specOf(t, f, id)
	if spec.Kind != triggers.KindPriceCross || spec.Origin != "intent:"+id || spec.Mode != triggers.ModeLive {
		t.Fatalf("spec = %+v", spec)
	}

	// Step 4: a watch the assistant only inferred is a suggestion, never fires, and
	// has no trigger.
	sug := watchArgsFor("ETH/USDT", 2000, "inferred")
	sug["user_requested"] = false
	sugID, sugAnswer := createIntent(t, f, sug)
	if !strings.Contains(sugAnswer, "Suggested (not active)") {
		t.Fatalf("an inferred watch is active: %s", sugAnswer)
	}

	emitTick(rt, "ETH/USDT", 2100)
	emitTick(rt, "ETH/USDT", 1900)
	emitTick(rt, "SOL/USDT", 125)
	emitTick(rt, "SOL/USDT", 118)
	d := waitDecision(t, fixtureDB(t, f), "price_cross:"+id+":%")
	var n int
	if err := fixtureDB(t, f).QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key LIKE ?`, "price_cross:"+sugID+":%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("a suggestion produced %d decisions (%v)", n, err)
	}
	if _, ok := specOf(t, f, sugID); ok {
		t.Fatal("a suggestion has a trigger")
	}
	if d.Shadow || d.Disposition != string(proactive.DispNotify) {
		t.Fatalf("decision = %+v, want a live notify", d)
	}
	if d.Candidate.Compose != proactive.ComposeTemplate || d.Candidate.IntentID != id {
		t.Fatalf("candidate = %+v: it must be a template for this intent", d.Candidate)
	}
	assertTitleOnlyInQuote(t, d.Candidate)

	// FireBudget 1: the firing is counted when attention has decided, the intent is
	// done, and the next pass removes its spec.
	waitUntil(t, "the intent to be done", func() bool { return intentStatus(t, f, id).Status == intent.StatusDone })
	if got := intentStatus(t, f, id); got.FiresUsed != 1 {
		t.Fatalf("fires used = %d", got.FiresUsed)
	}
	waitUntil(t, "the spec to go", func() bool { _, ok := specOf(t, f, id); return !ok })

	// What a model is given about it: the title is not in the request.
	runner := &recordingRunner{}
	cand := d.Candidate
	cand.Compose = proactive.ComposeLLM
	if _, err := engine.NewComposer(runner).Compose(context.Background(),
		attention.Message{Title: cand.Title}, []attention.Item{{Candidate: cand}}); err != nil {
		t.Fatal(err)
	}
	if len(runner.reqs) != 1 {
		t.Fatalf("%d turns", len(runner.reqs))
	}
	asked, _ := json.Marshal(runner.reqs[0])
	if strings.Contains(string(asked), gwTitleMarker) {
		t.Fatalf("the title is in the request to the model: %s", asked)
	}

	// The owner can read how it ended.
	res := alertTool(t, f, tools.NameListIntents).Execute(ownerCtx(), map[string]any{"status": "done"})
	if !strings.Contains(res.ForLLM, "spent its 1 fires") {
		t.Fatalf("list_intents = %s", res.ForLLM)
	}
}

// A reminder fires at its time on a fake clock; a restart does not move it; the fired
// timer is gone and the intent done.
func TestIntentReminder_FiresAtItsTimeAndSurvivesARestart(t *testing.T) {
	quickCompiler(t)
	clk := perceptiontest.NewClock(time.Now())
	useClockSource(t, clk)
	f := newAssemblyFixture(t, intentsLive())
	waitCompiler(t, f)

	id, answer := createIntent(t, f, map[string]any{
		"type": "reminder", "title": gwTitleMarker, "user_requested": true, "params": map[string]any{"in": "2h"}})
	if !strings.Contains(answer, "reminded at the time") {
		t.Fatalf("the answer does not say it works: %s", answer)
	}
	var due time.Time
	waitUntil(t, "the timer", func() bool {
		tm, ok := f.services.Perception.clock.Get(id)
		due = tm.At
		return ok
	})
	if spec, ok := specOf(t, f, id); !ok || spec.Kind != triggers.KindTimeAt || spec.Origin != "intent:"+id {
		t.Fatalf("spec = %+v %v", spec, ok)
	}

	// The gateway restarts: a new generation reads the timer back and leaves it where it was.
	if err := f.reload(intentsLive()); err != nil {
		t.Fatal(err)
	}
	waitCompiler(t, f)
	waitUntil(t, "the timer after the restart", func() bool {
		tm, ok := f.services.Perception.clock.Get(id)
		return ok && tm.At.Equal(due)
	})
	waitUntil(t, "the clock to sleep", func() bool { return clk.Pending() > 0 })

	rt := f.services.Perception
	advanceUntil(t, clk, 2*time.Hour+time.Minute, func() bool { return rt.clock.Stats().TimerEvents >= 1 })
	d := waitDecision(t, fixtureDB(t, f), "time_at:"+id+":%")
	if d.Shadow || d.Disposition != string(proactive.DispNotify) || d.Candidate.Title != "Reminder" {
		t.Fatalf("decision = %+v", d)
	}
	if !d.Candidate.OwnerRequested {
		t.Fatal("a reminder the owner set is not marked as the owner's own")
	}
	assertTitleOnlyInQuote(t, d.Candidate)
	waitUntil(t, "the reminder to be done", func() bool { return intentStatus(t, f, id).Status == intent.StatusDone })
	waitUntil(t, "the timer to go", func() bool { _, ok := rt.clock.Get(id); return !ok })
	var n int
	if err := fixtureDB(t, f).QueryRow(`SELECT COUNT(*) FROM clock_timers`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d timers left in the table (%v)", n, err)
	}
}

// A reminder given as a local time with no zone is read in the owner's profile zone and
// fires then.
func TestReminder_FiresAtTimeInProfileTZ(t *testing.T) {
	quickCompiler(t)
	clk := perceptiontest.NewClock(time.Now())
	useClockSource(t, clk)
	f := newAssemblyFixture(t, intentsLive())
	waitCompiler(t, f)

	store, err := intent.NewStore(context.Background(), fixtureDB(t, f))
	if err != nil {
		t.Fatal(err)
	}
	p := intent.DefaultProfile()
	p.Timezone = "Asia/Tokyo" // not the machine's zone, not UTC, not the default Bangkok
	if _, err := store.PutProfile(context.Background(), p, "test", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	local := time.Now().In(tokyo).Add(90 * time.Minute).Truncate(time.Minute)
	id, _ := createIntent(t, f, map[string]any{
		"type": "reminder", "title": gwTitleMarker, "user_requested": true,
		"params": map[string]any{"at": local.Format("2006-01-02T15:04")}})

	waitUntil(t, "the timer", func() bool { _, ok := f.services.Perception.clock.Get(id); return ok })
	if tm, _ := f.services.Perception.clock.Get(id); !tm.At.Equal(local) {
		t.Fatalf("timer at %s, want %s (the same moment as %s in Tokyo)", tm.At, local.UTC(), local.Format(time.RFC3339))
	}
	rt := f.services.Perception
	advanceUntil(t, clk, 90*time.Minute+time.Minute, func() bool { return rt.clock.Stats().TimerEvents >= 1 })
	d := waitDecision(t, fixtureDB(t, f), "time_at:"+id+":%")
	if d.Candidate.Title != "Reminder" || !strings.Contains(d.Candidate.Detail, local.Format("2006-01-02 15:04")+" JST") {
		t.Fatalf("candidate = %+v: the due time is shown in the owner's zone", d.Candidate)
	}
}

// At 03:00 owner time a watch and a reminder from an active intent are delivered, not
// held for the digest; a standing order's firing is held like any other Notice.
func TestIntentQuietHours_WatchAndReminderAreDeliveredAStandingOrderIsHeld(t *testing.T) {
	quickCompiler(t)
	clk := perceptiontest.NewClock(time.Now())
	useClockSource(t, clk)
	f := newAssemblyFixture(t, intentsLive())
	waitCompiler(t, f)
	quietNowProfile(t, f)
	rt := f.services.Perception

	watch, _ := createIntent(t, f, watchArgsFor("SOL/USDT", 120, "a watch"))
	order := watchArgsFor("ETH/USDT", 2000, "an order")
	order["type"] = "standing_order"
	orderID, answer := createIntent(t, f, order)
	if !strings.Contains(answer, "Nothing is proposed or done automatically") {
		t.Fatalf("standing order answer = %s", answer)
	}
	rem, _ := createIntent(t, f, map[string]any{
		"type": "reminder", "title": "a reminder", "user_requested": true, "params": map[string]any{"in": "1h"}})
	for _, id := range []string{watch, orderID, rem} {
		id := id
		waitUntil(t, "the trigger for "+id, func() bool { _, ok := specOf(t, f, id); return ok })
	}
	waitUntil(t, "the timer", func() bool { _, ok := rt.clock.Get(rem); return ok })
	waitUntil(t, "the clock to sleep", func() bool { return clk.Pending() > 0 })

	emitTick(rt, "SOL/USDT", 125)
	emitTick(rt, "SOL/USDT", 118)
	emitTick(rt, "ETH/USDT", 2100)
	emitTick(rt, "ETH/USDT", 1900)
	advanceUntil(t, clk, time.Hour+time.Minute, func() bool { return rt.clock.Stats().TimerEvents >= 1 })

	db := fixtureDB(t, f)
	for name, key := range map[string]string{"watch": "price_cross:" + watch + ":%", "reminder": "time_at:" + rem + ":%"} {
		d := waitDecision(t, db, key)
		if d.Disposition != string(proactive.DispNotify) || d.Reason != attention.ReasonOwnerAlertQuiet || !d.Candidate.OwnerRequested {
			t.Errorf("%s: %s/%s owner=%v, want notify/%s", name, d.Disposition, d.Reason, d.Candidate.OwnerRequested, attention.ReasonOwnerAlertQuiet)
		}
	}
	o := waitDecision(t, db, "price_cross:"+orderID+":%")
	if o.Disposition == string(proactive.DispNotify) || o.Reason == attention.ReasonOwnerAlertQuiet || o.Candidate.OwnerRequested {
		t.Errorf("standing order: %s/%s owner=%v: it must be held like any Notice", o.Disposition, o.Reason, o.Candidate.OwnerRequested)
	}
	if !strings.HasPrefix(o.Candidate.Title, "Standing order: ") {
		t.Errorf("standing order title = %q", o.Candidate.Title)
	}
}

// Default config: nothing is compiled and nothing exists. Intents on with triggers off:
// the intent is recorded, nothing watches it, and the tool says so.
func TestIntents_NothingCompiledWithoutTriggers(t *testing.T) {
	quickCompiler(t)
	useSources(t, false, &countingSource{name: "fake"})

	t.Run("default config", func(t *testing.T) {
		f := newAssemblyFixture(t, nil)
		if rt := f.services.Perception; rt != nil {
			t.Fatal("perception was built")
		}
		if _, ok := f.al.GetRegistry().GetDefaultAgent().Tools.Get(tools.NameCreateIntent); ok {
			t.Fatal("the intent tools exist with the default config")
		}
		if triggersTable(t, f) {
			t.Fatal("the triggers table exists")
		}
	})

	t.Run("intents on, triggers off", func(t *testing.T) {
		f := newAssemblyFixture(t, compose(liveWithPerception(), intentsOn))
		rt := f.services.Perception
		if rt == nil || rt.triggers != nil || rt.intentComp != nil {
			t.Fatalf("runtime = %+v: no registry, so no compiler", rt)
		}
		id, answer := createIntent(t, f, watchArgsFor("SOL/USDT", 120, "w"))
		if !strings.Contains(answer, "nothing watches it") || strings.Contains(answer, "set up and watching") {
			t.Fatalf("the answer lets the owner believe it works: %s", answer)
		}
		if _, ok := specOf(t, f, id); ok || triggersTable(t, f) {
			t.Fatal("something was compiled")
		}
		res := alertTool(t, f, tools.NameListIntents).Execute(ownerCtx(), nil)
		if !strings.Contains(res.ForLLM, "nothing watches it") {
			t.Fatalf("list_intents = %s", res.ForLLM)
		}
	})
}

// A rig whose sources hold no clock source (a seam test) still runs: the compiler says
// that a reminder has no clock instead of failing.
func TestIntentReminder_WithoutAClockSourceSaysSo(t *testing.T) {
	quickCompiler(t)
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, intentsLive())
	comp := waitCompiler(t, f)
	id, _ := createIntent(t, f, map[string]any{
		"type": "reminder", "title": "r", "user_requested": true, "params": map[string]any{"in": "1h"}})
	waitUntil(t, "the note", func() bool { return comp.Note(id) == intents.NoteNoClock })
	res := alertTool(t, f, tools.NameListIntents).Execute(ownerCtx(), nil)
	if !strings.Contains(res.ForLLM, intents.NoteNoClock) {
		t.Fatalf("list_intents = %s", res.ForLLM)
	}
}
