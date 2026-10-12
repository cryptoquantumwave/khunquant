package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func withTriggers(mode string) func(*config.Config) {
	return func(cfg *config.Config) { cfg.Proactive.Triggers.Mode = mode }
}

// gwTrigger is the one trivial trigger kind of these tests: for every device event it
// returns one candidate, and it demands the ticker its parameters name. Production
// registers no kind.
type gwTrigger struct {
	id     string
	symbol string
}

func (g gwTrigger) ID() string         { return g.id }
func (gwTrigger) Kinds() []events.Kind { return []events.Kind{events.KindPerceptionDevice} }
func (g gwTrigger) Demands() []perception.Demand {
	return []perception.Demand{{Provider: "okx", Symbol: g.symbol, Kind: perception.DemandTicker}}
}

func (g gwTrigger) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, _ *triggers.State) ([]proactive.Candidate, error) {
	return []proactive.Candidate{{
		DedupKey: "gwtest:usb:plugged", Severity: proactive.SeverityNotice, Subject: "usb",
		Title: "a device was plugged in", Trust: triggers.MinInputTrust(d), Compose: proactive.ComposeTemplate,
		At: v.Now(),
	}}, nil
}

func useTriggerFactories(t *testing.T) {
	t.Helper()
	prev := triggerFactories
	triggerFactories = func(assemblyDeps) map[string]triggers.Factory {
		return map[string]triggers.Factory{"gwtest": func(s triggers.Spec) (triggers.Trigger, error) {
			var p struct {
				Symbol string `json:"symbol"`
			}
			if err := json.Unmarshal(s.Params, &p); err != nil {
				return nil, err
			}
			return gwTrigger{id: s.Kind + ":" + s.ID, symbol: p.Symbol}, nil
		}}
	}
	t.Cleanup(func() { triggerFactories = prev })
}

func triggersTable(t *testing.T, f *assemblyFixture) bool {
	t.Helper()
	db := fixtureDB(t, f)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'triggers'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func fixtureDB(t *testing.T, f *assemblyFixture) *sql.DB {
	t.Helper()
	db, err := proactiveDB(assemblyDeps{Cfg: f.initial, Shared: f.shared})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTriggers_DefaultConfigCreatesNoTableAndNoRegistry(t *testing.T) {
	for name, mut := range map[string]func(*config.Config){
		"default":                         nil,
		"perception on, triggers off":     liveWithPerception(),
		"triggers shadow, perception off": compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withTriggers(config.ProactiveModeShadow)),
		"triggers shadow, proactive off":  compose(withTriggers(config.ProactiveModeShadow), withPerception),
	} {
		t.Run(name, func(t *testing.T) {
			useSources(t, false, &countingSource{name: "fake"})
			before := goroutinesIn(engineRunFrame)
			f := newAssemblyFixture(t, mut)
			if rt := f.services.Perception; rt != nil && rt.triggers != nil {
				t.Fatal("a trigger registry was built")
			}
			if triggersTable(t, f) {
				t.Fatal("the triggers table exists")
			}
			if n := goroutinesIn("pkg/triggers"); n != 0 {
				t.Fatalf("%d trigger goroutines", n)
			}
			if f.services.Perception == nil && goroutinesIn(engineRunFrame) != before {
				t.Fatal("an engine goroutine runs although perception is off")
			}
			code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
			if err != nil || code != http.StatusOK || strings.Contains(string(body), `"triggers"`) {
				t.Fatalf("stats: %d %v %s", code, err, body)
			}
		})
	}
}

func TestTriggers_BuiltWhenOnAndTheMarketSourceReadsItsDemands(t *testing.T) {
	useTriggerFactories(t)
	var seen perception.DemandSource
	prev := perceptionSources
	perceptionSources = func(d assemblyDeps) ([]perception.Source, bool) {
		seen = d.Demands
		return []perception.Source{&countingSource{name: "fake"}}, false
	}
	t.Cleanup(func() { perceptionSources = prev })

	f := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)))
	rt := f.services.Perception
	if rt == nil || rt.triggers == nil {
		t.Fatal("the registry was not built")
	}
	if !triggersTable(t, f) {
		t.Fatal("no triggers table")
	}
	if seen != perception.DemandSource(rt.triggers) {
		t.Fatalf("the market source is given %T, not the registry", seen)
	}
	if _, err := rt.triggers.Add(context.Background(), triggers.Spec{
		ID: "s1", Kind: "gwtest", Mode: triggers.ModeLive, Origin: "test", Params: json.RawMessage(`{"symbol":"BTC/USDT"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if ds := seen.Demands(); len(ds) != 1 || ds[0].Symbol != "BTC/USDT" {
		t.Fatalf("demands = %+v", ds)
	}

	// A kind with no factory is refused.
	if _, err := rt.triggers.Add(context.Background(), triggers.Spec{Kind: "price_cross", Mode: triggers.ModeShadow, Origin: "t"}); err == nil {
		t.Fatal("an unregistered kind was accepted")
	}
}

// Production registers the two crossing kinds and nothing else.
func TestTriggerFactories_ProductionRegistersTheCrossingKinds(t *testing.T) {
	got := triggerFactories(assemblyDeps{})
	if len(got) != 2 || got[triggers.KindPriceCross] == nil || got[triggers.KindIndicatorCross] == nil {
		t.Fatalf("production registers %d trigger kinds: %v", len(got), got)
	}
}

// A shadow-mode trigger is evaluated, its candidate goes through the real governor,
// and the owner is told nothing: the decision is logged as shadow.
func TestTriggers_ShadowEndToEndLoggedNotDelivered(t *testing.T) {
	useTriggerFactories(t)
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)))
	rt := f.services.Perception
	if rt == nil || rt.triggers == nil {
		t.Fatal("the registry was not built")
	}
	// The spec itself is live; the global shadow mode caps it.
	if _, err := rt.triggers.Add(context.Background(), triggers.Spec{
		ID: "s1", Kind: "gwtest", Mode: triggers.ModeLive, Origin: "test", Params: json.RawMessage(`{"symbol":"BTC/USDT"}`),
	}); err != nil {
		t.Fatal(err)
	}

	// An envelope-only diff: the device event is a kind the world model does not
	// track, and it reaches the trigger all the same.
	rt.engine.Emitter().Emit(events.Event{
		Kind: events.KindPerceptionDevice, Source: events.Source{Component: "perception", Name: "devices"},
		Trust: events.TrustPtr(proactive.TrustSystem),
	})
	db := fixtureDB(t, f)
	waitUntil(t, "the decision", func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM attention_decisions WHERE dedup_key = 'gwtest:usb:plugged'`).Scan(&n)
		return n == 1
	})
	var disp, reason, trig string
	var shadow int
	if err := db.QueryRow(`SELECT disposition, reason, shadow, trigger_id FROM attention_decisions WHERE dedup_key = 'gwtest:usb:plugged'`).
		Scan(&disp, &reason, &shadow, &trig); err != nil {
		t.Fatal(err)
	}
	if disp != string(proactive.DispLog) || reason != "shadow" || shadow != 1 || trig != "gwtest:s1" {
		t.Fatalf("decision = %s/%s shadow=%d trigger=%s", disp, reason, shadow, trig)
	}
	if st := rt.engine.Stats(); st.StageErrors != 0 || st.StagePanics != 0 || st.Candidates != 1 || st.Decisions != 1 {
		t.Fatalf("engine stats = %+v", st)
	}

	// The stats route carries the registry's counters, as numbers only.
	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v", code, err)
	}
	var got busStatsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Perception == nil || got.Perception.Triggers == nil || got.Perception.Triggers.Evaluated != 1 ||
		got.Perception.Triggers.Candidates != 1 || got.Perception.Triggers.Specs != 1 {
		t.Fatalf("trigger stats = %+v (%s)", got.Perception, body)
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	assertNumbersOnly(t, "perception.triggers", raw["perception"].(map[string]any)["triggers"])
}

// The adapter does not evaluate what names no event: a nil Diff, a foreign Diff and a
// nil View.
func TestTriggerStage_IgnoresWhatItCannotRoute(t *testing.T) {
	useTriggerFactories(t)
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, compose(liveWithPerception(), withTriggers(config.ProactiveModeShadow)))
	rt := f.services.Perception
	if _, err := rt.triggers.Add(context.Background(), triggers.Spec{
		ID: "s1", Kind: "gwtest", Mode: triggers.ModeLive, Origin: "test", Params: json.RawMessage(`{"symbol":"BTC/USDT"}`),
	}); err != nil {
		t.Fatal(err)
	}
	st := triggerStage{r: rt.triggers}
	view := rt.worldModel.store.View()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		v any
		d any
	}{
		"nil diff":     {view, nil},
		"foreign diff": {view, 42},
		"nil view":     {nil, worldmodel.Diff{Event: events.Event{Kind: events.KindPerceptionDevice}}},
		"foreign view": {"view", worldmodel.Diff{Event: events.Event{Kind: events.KindPerceptionDevice}}},
	} {
		if cs, err := st.Evaluate(ctx, tc.v, tc.d); err != nil || len(cs) != 0 {
			t.Errorf("%s: %v, %v", name, cs, err)
		}
	}
	if cs, err := st.Evaluate(ctx, view, worldmodel.Diff{Event: events.Event{Kind: events.KindPerceptionDevice}}); err != nil || len(cs) != 1 {
		t.Fatalf("a routable diff: %v, %v", cs, err)
	}
}
