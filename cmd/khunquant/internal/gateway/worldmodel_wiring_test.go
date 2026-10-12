package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func wmTables(t *testing.T, f *assemblyFixture) int {
	t.Helper()
	db, err := proactiveDB(assemblyDeps{Cfg: f.initial, Shared: f.shared})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'wm\_%' ESCAPE '\'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWorldModel_DefaultConfigCreatesNoTable(t *testing.T) {
	for name, mut := range map[string]func(*config.Config){
		"default":                      nil,
		"proactive without perception": compose(withOwner("1001"), withAttentionMode("live")),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mut)
			if f.services.Perception != nil {
				t.Fatal("perception built")
			}
			if n := wmTables(t, f); n != 0 {
				t.Fatalf("%d world model tables exist although perception is off", n)
			}
			if f.al.GetRegistry().GetDefaultAgent().ContextBuilder == nil {
				t.Fatal("no context builder")
			}
			code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
			if err != nil || code != http.StatusOK || strings.Contains(string(body), "worldmodel") {
				t.Fatalf("stats: %d %v %s", code, err, body)
			}
		})
	}
}

func TestWorldModel_BuiltWithPerceptionAppliesEventsAndCheckpointsOnStop(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, liveWithPerception())
	rt := f.services.Perception
	if rt == nil || rt.worldModel == nil {
		t.Fatal("the world model was not built with perception")
	}
	if n := wmTables(t, f); n != 4 {
		t.Fatalf("%d world model tables, want 4", n)
	}
	em := rt.engine.Emitter()
	now := time.Now().UTC()

	// The real source payload types, as the sources emit them.
	em.Emit(events.Event{
		Kind: events.KindPerceptionMarketTick, Time: now, Source: events.Source{Component: "perception", Name: "market"},
		Payload: sources.MarketTick{Provider: "binance", Symbol: "BTC/USDT", Last: 62000, At: now},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	})
	em.Emit(snapshotEventFor(now.Add(-time.Minute), 1000))
	em.Emit(snapshotEventFor(now, 1100))
	waitUntil(t, "the events", func() bool { return rt.engine.Stats().Processed == 3 })

	store := rt.worldModel.store
	p, _, ok := store.View().Price("binance", "BTC/USDT")
	if !ok || p != 62000 {
		t.Fatalf("the tick did not reach the model: %v %v", p, ok)
	}
	if rt.engine.Stats().StageErrors != 0 || rt.engine.Stats().StagePanics != 0 {
		t.Fatalf("engine stats = %+v", rt.engine.Stats())
	}

	// The stats route carries the model's counters, as numbers only.
	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v", code, err)
	}
	var got busStatsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Perception == nil || got.Perception.WorldModel == nil || got.Perception.WorldModel.Applied != 3 || got.Perception.WorldModel.Series != 1 {
		t.Fatalf("world model stats = %+v (%s)", got.Perception, body)
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	assertNumbersOnly(t, "perception.worldmodel", raw["perception"].(map[string]any)["worldmodel"])

	// A reload stops the first generation (final checkpoint) before the second is
	// built, and the second starts from what the first left.
	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	next := f.services.Perception
	if next == nil || next.worldModel == nil || next.worldModel.store == store {
		t.Fatal("the world model was not rebuilt")
	}
	p, at, ok := next.worldModel.store.View().Price("binance", "BTC/USDT")
	if !ok || p != 62000 || at.Sub(now).Abs() > time.Millisecond {
		t.Fatalf("restored price = %v at %v (ok=%v), want 62000 at %v", p, at, ok, now)
	}
	pv, ok := next.worldModel.store.View().Portfolio()
	if !ok || pv.TotalUSD != 1100 {
		t.Fatalf("restored portfolio = %+v", pv)
	}
	if next.worldModel.store.Stats().RestoredRows == 0 {
		t.Fatal("nothing restored")
	}
}

func snapshotEventFor(at time.Time, total float64) events.Event {
	return events.Event{
		Kind: events.KindPerceptionPortfolioSnapshot, Time: at, Source: events.Source{Component: "perception", Name: "portfolio"},
		Payload: sources.PortfolioSnapshotPayload{
			TakenAt: at, Quote: "USDT", TotalValueUSD: total, ByClass: map[string]float64{"crypto": total},
			Positions: []sources.PortfolioPosition{{Asset: "BTC", Source: "binance", Class: "crypto", Value: total, Qty: 1}},
		},
		Trust: events.TrustPtr(proactive.TrustSystem),
	}
}

func TestWorldModel_ChangesSectionReachesBackgroundPromptsOnly(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, liveWithPerception())
	rt := f.services.Perception
	em := rt.engine.Emitter()
	now := time.Now().UTC()
	em.Emit(snapshotEventFor(now.Add(-time.Minute), 1000))
	em.Emit(snapshotEventFor(now, 1100))
	waitUntil(t, "the events", func() bool { return rt.engine.Stats().Processed == 2 })

	cb := f.al.GetRegistry().GetDefaultAgent().ContextBuilder
	build := func(m agent.PromptMode) string {
		return cb.BuildMessagesWithOptions(nil, "", "hi", nil, "telegram", "42", agent.BuildOptions{Mode: m})[0].Content
	}
	const marker = "Portfolio total (USD): 1,000 -> 1,100"
	if !strings.Contains(build(agent.PromptProactive), marker) || !strings.Contains(build(agent.PromptLight), marker) {
		t.Fatal("the background prompts lack the changes section")
	}
	if strings.Contains(build(agent.PromptFull), "Changes since your last look") {
		t.Fatal("a user turn carries the changes section")
	}
	// A reload keeps it (the agent loop holds the section; the new generation sets it).
	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatal(err)
	}
	cb = f.al.GetRegistry().GetDefaultAgent().ContextBuilder
	if !strings.Contains(build(agent.PromptProactive), marker) {
		t.Fatal("the section is gone after a reload")
	}
	// Switching perception off removes it, and the full prompt never changed.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode("live"))); err != nil {
		t.Fatal(err)
	}
	cb = f.al.GetRegistry().GetDefaultAgent().ContextBuilder
	if strings.Contains(build(agent.PromptProactive), "Changes since your last look") {
		t.Fatal("the section outlived the world model")
	}
}

func TestWorldModel_StageAdapterBoxesDiffsAndForwardsHousekeeping(t *testing.T) {
	db := sqlitextest.Open(t)
	store, err := worldmodel.NewStore(context.Background(), db, perception.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	stage := worldModelStage{s: store}
	now := time.Now().UTC()

	// A tracked event that changes nothing gives a nil Diff (the engine publishes
	// nothing for it).
	tick := events.Event{Kind: events.KindPerceptionMarketTick, Time: now,
		Payload: sources.MarketTick{Provider: "binance", Symbol: "BTC/USDT", Last: 1, At: now}}
	if _, err := stage.Apply(context.Background(), tick); err != nil {
		t.Fatal(err)
	}
	tick.Time = now.Add(time.Second)
	d, err := stage.Apply(context.Background(), tick)
	if err != nil || d != nil {
		t.Fatalf("a tick inside a bucket: %v %v", d, err)
	}
	// An event of a kind the model does not track is still passed on, as an
	// envelope, so the evaluator is told it happened.
	d, err = stage.Apply(context.Background(), events.Event{ID: "d1", Kind: events.KindPerceptionDevice, Time: now, Payload: "usb"})
	if err != nil {
		t.Fatal(err)
	}
	if wd, ok := d.(worldmodel.Diff); !ok || wd.Event.ID != "d1" || wd.Event.Kind != events.KindPerceptionDevice || wd.Event.Payload != nil {
		t.Fatalf("untracked event: %T %+v", d, d)
	}
	// One that does gives a worldmodel.Diff the evaluator can unbox once.
	if _, err := stage.Apply(context.Background(), snapshotEventFor(now.Add(-time.Minute), 1000)); err != nil {
		t.Fatal(err)
	}
	d, err = stage.Apply(context.Background(), snapshotEventFor(now, 1100))
	if err != nil {
		t.Fatal(err)
	}
	wd, ok := d.(worldmodel.Diff)
	if !ok || wd.Empty() {
		t.Fatalf("diff = %T %+v", d, d)
	}
	if _, ok := stage.View().(worldmodel.View); !ok {
		t.Fatalf("view = %T", stage.View())
	}
	var _ engine.Housekeeper = stage

	stage.Housekeep(context.Background(), now.Add(worldmodel.CheckpointEvery+time.Second))
	waitUntil(t, "the checkpoint the engine's housekeeping asked for", func() bool { return store.Stats().Checkpoints == 1 })
	if err := store.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestWorldModel_PayloadMirrorsMatchTheSources pins the world model's mirror types
// to the real payloads: every JSON field the mirror reads exists on the source's
// struct with the same name, every field of the source's struct the mirror leaves
// out is listed here on purpose, and a real payload decodes to the right state.
func TestWorldModel_PayloadMirrorsMatchTheSources(t *testing.T) {
	type pair struct {
		name           string
		source, mirror any
		skipped        []string // fields of the source the model does not read, on purpose
	}
	for _, p := range []pair{
		{"tick", sources.MarketTick{}, worldmodel.TickPayload{}, []string{"bid", "ask", "exchange_at"}},
		{"portfolio position", sources.PortfolioPosition{}, worldmodel.PortfolioPositionPayload{}, nil},
		{"portfolio", sources.PortfolioSnapshotPayload{}, worldmodel.PortfolioPayload{}, nil},
		{"futures position", sources.FuturesPosition{}, worldmodel.FuturesPositionPayload{}, nil},
		{"futures", sources.FuturesPositionsPayload{}, worldmodel.FuturesPayload{}, nil},
		{"day start", sources.ClockDayStartPayload{}, worldmodel.DayStartPayload{}, []string{"late", "skipped_days"}},
	} {
		src, mir := jsonTags(p.source), jsonTags(p.mirror)
		for tag := range mir {
			if !src[tag] {
				t.Errorf("%s: the mirror reads %q, which the source's payload does not have", p.name, tag)
			}
		}
		skipped := map[string]bool{}
		for _, s := range p.skipped {
			skipped[s] = true
		}
		for tag := range src {
			if !mir[tag] && !skipped[tag] {
				t.Errorf("%s: the source's payload has %q, which the model neither reads nor lists as skipped", p.name, tag)
			}
		}
	}
	// The health payload has two producers; the mirror is the union of both.
	mir := jsonTags(worldmodel.HealthPayload{})
	for _, p := range []any{sources.SourceHealthPayload{}, sources.MarketHealth{}} {
		for tag := range jsonTags(p) {
			if !mir[tag] {
				t.Errorf("health: %T has %q, which the mirror lacks", p, tag)
			}
		}
	}

	// And real payloads decode to the right state.
	store, err := worldmodel.NewStore(context.Background(), nil, perception.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	apply := func(ev events.Event) {
		t.Helper()
		if _, err := store.Apply(context.Background(), ev); err != nil {
			t.Fatalf("%s: %v", ev.Kind, err)
		}
	}
	apply(events.Event{Kind: events.KindPerceptionFuturesPositions, Time: now, Source: events.Source{Name: "futures"},
		Payload: sources.FuturesPositionsPayload{Provider: "okx", Account: "main", At: now, Positions: []sources.FuturesPosition{{
			Symbol: "BTC/USDT:USDT", Side: "long", Contracts: 135, ContractSize: 0.01, ContractSizeKnown: true,
			MarkPrice: 60000, MarkPriceKnown: true, MarginRatio: 0.8, MarginRatioKnown: true, NotionalUSD: 8100, NotionalKnown: true,
		}}}})
	fv, ok := store.View().FuturesPosition("okx/main", "BTC/USDT:USDT")
	if !ok || fv.Contracts != 135 || !fv.ContractSizeKnown || fv.ContractSize != 0.01 || !fv.MarkPriceKnown || fv.LiqPriceKnown ||
		!fv.MarginRatioKnown || fv.MarginRatio != 0.8 || !fv.NotionalKnown || fv.NotionalUSD != 8100 || fv.UnrealizedKnown {
		t.Fatalf("futures = %+v", fv)
	}
	apply(events.Event{Kind: events.KindPerceptionSourceHealth, Time: now, Source: events.Source{Name: "futures"},
		Payload: sources.SourceHealthPayload{Source: "futures", State: "degraded", Reason: "unreadable", FailedAccounts: []string{"okx/main"}}})
	if st, _ := store.View().SourceState("futures"); !st.Degraded || len(st.Failed) != 1 {
		t.Fatalf("source health = %+v", st)
	}
	apply(events.Event{Kind: events.KindPerceptionSourceHealth, Time: now, Source: events.Source{Name: "market"},
		Payload: sources.MarketHealth{Source: "market", Provider: "binance", Healthy: false, Failures: 3, Reason: "rate_limited", At: now}})
	if st, _ := store.View().SourceState("market"); !st.Degraded || st.Failed[0] != "binance" {
		t.Fatalf("market health = %+v", st)
	}
	apply(events.Event{Kind: events.KindPerceptionClockDayStart, Time: now, Source: events.Source{Name: "clock"},
		Payload: sources.ClockDayStartPayload{Date: now.Format("2006-01-02"), Zone: "Asia/Bangkok", At: now}})
	apply(events.Event{Kind: events.KindPerceptionPortfolioSnapshot, Time: now, Source: events.Source{Name: "portfolio"},
		Payload: sources.PortfolioSnapshotPayload{
			TakenAt: now, Quote: "USDT", TotalValueUSD: 900, ByClass: map[string]float64{"crypto": 900},
			Positions: []sources.PortfolioPosition{{Asset: "BTC", Source: "binance", Class: "crypto", Value: 900, Qty: 0.01}},
			Unpriced:  []string{"binance/main:XYZ"},
		}})
	pv, ok := store.View().Portfolio()
	if !ok || pv.TotalUSD != 900 || pv.UnpricedCount != 1 || len(pv.Positions) != 1 || pv.Positions[0].Venue != "binance" || pv.Positions[0].Qty != 0.01 {
		t.Fatalf("portfolio = %+v", pv)
	}
	// The order feed's own type goes through as it is.
	apply(events.Event{Kind: events.KindPerceptionOrderFilled, Time: now, Source: events.Source{Name: "orderfeed"},
		Payload: orderfeed.OrderEvent{Provider: "binance", Account: "main", Symbol: "BTC/USDT", Side: "buy", Status: "closed",
			Filled: 1, FilledKnown: true, AmountUnit: orderfeed.UnitBase, At: now}})
	if _, ok := store.View().LastFill("binance/main", "BTC/USDT"); !ok {
		t.Fatal("fill not recorded")
	}
	// A simulated order is never a fill even under the filled kind.
	apply(events.Event{Kind: events.KindPerceptionOrderFilled, Time: now, Source: events.Source{Name: "orderfeed"},
		Payload: orderfeed.OrderEvent{Provider: "binance", Account: "main", Symbol: "ETH/USDT", Side: "buy", Status: "closed", Paper: true, At: now}})
	if _, ok := store.View().LastFill("binance/main", "ETH/USDT"); ok {
		t.Fatal("a paper order became a fill")
	}
}

// jsonTags returns the JSON field names of a struct.
func jsonTags(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		out[strings.Split(tag, ",")[0]] = true
	}
	return out
}
