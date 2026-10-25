package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/sweep"
	"github.com/cryptoquantumwave/khunquant/pkg/providers"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

func sweepConfig() func(*config.Config) {
	return compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withPerception,
		withHeartbeat(config.HeartbeatModeSweep))
}

// snapshotSeq numbers the snapshots emitted by emitMove.
var snapshotSeq int

// emitMove puts two portfolio snapshots through the running pipeline, BTC at from and
// then at to, and waits for it to work them off.
func emitMove(t *testing.T, rt *perceptionRuntime, from, to float64) {
	t.Helper()
	now := time.Now()
	snapshotSeq++
	rt.engine.Emitter().Emit(btcSnapshot(now.Add(-2*time.Minute), from, snapshotSeq))
	snapshotSeq++
	rt.engine.Emitter().Emit(btcSnapshot(now, to, snapshotSeq))
	if err := rt.engine.Settle(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func hasSweepMark(f *assemblyFixture) bool {
	rt := f.services.Perception
	if rt == nil || rt.worldModel == nil {
		return false
	}
	_, _, ok := rt.worldModel.store.MarkInfo(sweep.Mark)
	return ok
}

// With the default configuration nothing of the sweep exists: the heartbeat is the
// legacy one, no sweep state is made and no mark is created.
func TestSweepAssembly_DefaultConfigRunsNothingNew(t *testing.T) {
	f := newAssemblyFixture(t, nil)
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("default contract = %s", got)
	}
	if f.shared.sweep != nil {
		t.Fatal("sweep state was made by a default install")
	}
	if f.services.Perception != nil || hasSweepMark(f) {
		t.Fatal("perception or the sweep mark exists on a default install")
	}
	// The exact legacy and governed modes behave as before: neither builds a sweep.
	for _, mode := range []string{config.HeartbeatModeLegacy, config.HeartbeatModeGoverned} {
		if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withPerception,
			withHeartbeat(mode))); err != nil {
			t.Fatal(err)
		}
		if f.shared.sweep != nil || hasSweepMark(f) {
			t.Fatalf("%s mode created sweep state or its mark", mode)
		}
	}
}

// Sweep mode builds the sweep (a mark, the shared state) over a governed heartbeat,
// follows reloads, and is downgraded with a visible entry when what it needs is
// missing.
func TestSweepAssembly_ModeFollowsEffectiveAndReload(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	var governedBuilt int
	orig := newGovernedHandler
	newGovernedHandler = func(
		run proactiveRunner, out attentionSubmitter, interval time.Duration, tier proactive.ModelTier, toolNames []string,
	) heartbeat.ContextHandler {
		governedBuilt++
		return orig(run, out, interval, tier, toolNames)
	}
	t.Cleanup(func() { newGovernedHandler = orig })

	f := newAssemblyFixture(t, sweepConfig())
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractGoverned {
		t.Fatalf("sweep: contract = %s, want governed", got)
	}
	if governedBuilt != 0 || !hasSweepMark(f) || f.shared.sweep == nil {
		t.Fatalf("sweep not built: governed handlers %d, mark %v, state %v", governedBuilt, hasSweepMark(f), f.shared.sweep != nil)
	}

	// No perception: Effective says governed (with an entry), and a governed handler
	// is what is built.
	noPerception := compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withHeartbeat(config.HeartbeatModeSweep))
	cfg := f.config(noPerception)
	if eff := cfg.Proactive.Effective(); eff.HeartbeatMode != config.HeartbeatModeGoverned ||
		len(eff.Downgrades) != 1 || !strings.Contains(eff.Downgrades[0], "perception is off; heartbeat.mode sweep → governed") {
		t.Fatalf("effective: %+v", eff)
	}
	if err := f.reload(noPerception); err != nil {
		t.Fatal(err)
	}
	if governedBuilt != 1 || hasSweepMark(f) {
		t.Fatalf("without perception: governed handlers %d, mark %v", governedBuilt, hasSweepMark(f))
	}

	// Back to sweep.
	if err := f.reload(sweepConfig()); err != nil {
		t.Fatal(err)
	}
	if governedBuilt != 1 || !hasSweepMark(f) {
		t.Fatalf("after reloading to sweep: governed handlers %d, mark %v", governedBuilt, hasSweepMark(f))
	}

	// Attention to shadow: sweep → legacy.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeShadow), withPerception,
		withHeartbeat(config.HeartbeatModeSweep))); err != nil {
		t.Fatal(err)
	}
	if got := f.services.HeartbeatService.Contract(); got != heartbeat.ContractLegacy {
		t.Fatalf("sweep with shadow attention: contract = %s, want legacy", got)
	}
}

// What a sweep needs and the assembly does not have is a WARN and a governed
// heartbeat, never a sweep that cannot work.
func TestSweepHandler_MissingPiecesFallBackWithAWarning(t *testing.T) {
	eff := config.EffectiveModes{HeartbeatMode: config.HeartbeatModeSweep}
	run := &fakeRunner{}
	for name, s := range map[string]*gatewayServices{
		"no outbox":      {},
		"no perception":  {Attention: nil},
		"no world model": {Perception: &perceptionRuntime{}},
	} {
		logs := captureLogs(t)
		if h := sweepHandler(assemblyDeps{}, s, eff, run, time.Hour); h != nil {
			t.Errorf("%s: a sweep handler was built", name)
		}
		if !strings.Contains(logs.String(), "heartbeat.mode is sweep but") {
			t.Errorf("%s: no warning:\n%s", name, logs.String())
		}
	}
	if h := sweepHandler(assemblyDeps{}, &gatewayServices{}, config.EffectiveModes{HeartbeatMode: config.HeartbeatModeGoverned}, run, time.Hour); h != nil {
		t.Error("a sweep handler was built for a governed heartbeat")
	}
}

// The sweep's turn is the heartbeat's: the heartbeat kind, the owner's model tier and
// tool list, and the heartbeat deadline.
func TestSweepHandler_ModelTierToolsAndDeadlineAreTheOwners(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, sweepConfig())
	rt := f.services.Perception

	// A change after the mark, so that the sweep has something to ask about: the
	// first snapshot is a baseline, the second moves the total by 10%.
	emitMove(t, rt, 60000, 54000)

	run := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}}}}
	eff := config.EffectiveModes{
		HeartbeatMode: config.HeartbeatModeSweep, HeartbeatModel: proactive.ModelTierLight,
		HeartbeatTools: []string{tools.NameGetTicker, tools.NameGetTickers},
	}
	h := sweepHandler(assemblyDeps{Shared: f.shared}, f.services, eff, run, 2*time.Hour)
	if h == nil {
		t.Fatal("no sweep handler")
	}
	res := h(context.Background(), "the heartbeat prompt", "", "")
	if res == nil || res.IsError || !res.Silent {
		t.Fatalf("result = %+v", res)
	}
	reqs := run.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d turns, want 1", len(reqs))
	}
	q := reqs[0]
	if q.Kind != proactive.TurnHeartbeatLegacy || q.Model != proactive.ModelTierLight ||
		strings.Join(q.Tools, ",") != tools.NameGetTicker+","+tools.NameGetTickers ||
		q.Deadline != heartbeatDeadline(2*time.Hour) || !q.CallerSuppliesChanges {
		t.Fatalf("turn request = %+v", q)
	}
	if !strings.Contains(q.Prompt, "the heartbeat prompt") || !strings.Contains(q.Prompt, "Changes since your last look") {
		t.Fatalf("prompt:\n%s", q.Prompt)
	}

	// The default tier is the owner's normal model: the sweep never picks light by
	// itself.
	run2 := &fakeRunner{res: run.res}
	emitMove(t, rt, 54000, 40000)
	eff2 := config.DefaultConfig().Proactive.Effective()
	eff2.HeartbeatMode = config.HeartbeatModeSweep
	h2 := sweepHandler(assemblyDeps{Shared: f.shared}, f.services, eff2, run2, time.Hour)
	h2(context.Background(), "p", "", "")
	if got := run2.requests(); len(got) != 1 || got[0].Model != proactive.ModelTierPrimary || len(got[0].Tools) != 0 {
		t.Fatalf("default request: %+v", got)
	}
}

// The sweep's turn gets no trade tool, exits included, whatever the tool list asks
// for: it runs on the real agent loop with "*" and the model is shown what it is
// offered.
func TestSweepHandler_NoTradeToolEvenWithEveryToolRequested(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	cfgFn := compose(sweepConfig(), func(c *config.Config) { c.Proactive.Heartbeat.Tools = []string{config.AllToolsName} })
	f := newAssemblyFixture(t, cfgFn)
	rec := &recordingProvider{}
	prev := createProvider
	createProvider = func(*config.Config) (providers.LLMProvider, string, error) { return rec, "", nil }
	t.Cleanup(func() { createProvider = prev })
	if err := f.reload(cfgFn); err != nil {
		t.Fatal(err)
	}
	rt := f.services.Perception
	emitMove(t, rt, 60000, 54000)

	eff := f.al.GetConfig().Proactive.Effective()
	if len(eff.HeartbeatTools) != 1 || eff.HeartbeatTools[0] != config.AllToolsName {
		t.Fatalf("precondition: tools = %v", eff.HeartbeatTools)
	}
	h := sweepHandler(assemblyDeps{Shared: f.shared}, f.services, eff, f.al, 30*time.Minute)
	if res := h(context.Background(), "heartbeat prompt", "", ""); res == nil || res.IsError {
		t.Fatalf("result = %+v", res)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.tools) == 0 {
		t.Fatal("the model was never called")
	}
	offered := rec.tools[0]
	if len(offered) < 5 {
		t.Fatalf("with every tool requested only %v were offered", offered)
	}
	for _, name := range offered {
		switch tools.ClassOf(name) {
		case tools.ClassTrade, tools.ClassFunds, tools.ClassSystem, tools.ClassAutomation, tools.ClassPropose:
			t.Errorf("the sweep's turn is offered %q (class %v)", name, tools.ClassOf(name))
		}
		if name == tools.NameMessage {
			t.Errorf("the sweep's turn is offered %q", name)
		}
	}
}

// Every trigger kind the registry can run in this build has a class in the sweep's
// table, so that a new kind is never silently invisible to the no-duplicates rule.
func TestSweepKinds_EveryRegisteredTriggerKindHasAClass(t *testing.T) {
	known := map[string]bool{}
	for _, k := range sweep.KnownKinds() {
		known[k] = true
	}
	d := assemblyDeps{Cfg: config.DefaultConfig(), Shared: newSharedResources()}
	d.Alerts = newAlertRuntime(d)
	factories := triggerFactories(d)
	if len(factories) < 10 {
		t.Fatalf("only %d kinds registered: %v", len(factories), factories)
	}
	for kind := range factories {
		if !known[kind] {
			t.Errorf("trigger kind %q has no class in pkg/proactive/sweep/topics.go (kindClass)", kind)
		}
	}
}

// The sweep prompt, as the heartbeat service wraps HEARTBEAT.md, with a sweep
// service that runs on a workspace with no task of the owner's.
func TestSweepService_RunsWithoutTasks(t *testing.T) {
	ws := t.TempDir()
	hs := heartbeat.NewHeartbeatService(ws, 30, true, nil)
	hs.SetContract(heartbeat.ContractGoverned)
	hs.SetRunWithoutTasks(true)
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(heartbeat.TemplateV2()), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := make(chan string, 1)
	hs.SetContextHandler(func(_ context.Context, p, _, _ string) *tools.ToolResult {
		prompt <- p
		return tools.SilentResult("ok")
	})
	if err := hs.Start(); err != nil {
		t.Fatal(err)
	}
	defer hs.Stop()
	select {
	case p := <-prompt:
		if !strings.Contains(p, heartbeat.V2Marker) {
			t.Fatalf("prompt: %.200s", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep service did not run on an idle HEARTBEAT.md")
	}
}

// The sweep reads the subject of a topic out of its dedup key. This builds the key of
// every trigger kind with the trigger package's own key functions and shows what the
// sweep makes of it: only an asset symbol or a plan number comes out, never another
// word of the key ("margin_critical", "stale1", an account, a direction).
func TestSweepTopicSubject_RealKeyFormatsOfAllTenKinds(t *testing.T) {
	hash := []byte{0x1a, 0x2b, 0x3c, 0x4d}
	for name, tc := range map[string]struct {
		key, logAsset string
		asset, plan   string
	}{
		triggers.KindMoveVsVol:            {triggers.MoveKey("BTC", "1h", 1), "BTC", "BTC", ""},
		triggers.KindMoveVsVol + "/key":   {triggers.MoveKey("AAPL", "1h", 2), "", "AAPL", ""},
		triggers.KindPriceCross:           {triggers.PriceCrossKey("binance", "BTC/USDT", "above", "70000"), "BTC", "BTC", ""},
		triggers.KindPriceCross + "/key":  {triggers.PriceCrossKey("binance", "SOL/USDT", "below", "100"), "", "SOL", ""},
		triggers.KindIndicatorCross:       {triggers.IndicatorCrossKey("binance", "ETH/USDT", "1h", "rsi", 14, "above", "70"), "", "ETH", ""},
		triggers.KindDrawdown:             {triggers.DrawdownKey("day", 2), "", "", ""},
		triggers.KindDrawdown + "/rebase": {triggers.DrawdownRebaseKey("2026-03-10"), "", "", ""},
		triggers.KindAllocationDrift:      {triggers.DriftKey("crypto", "over"), "", "", ""},
		triggers.KindDataStale:            {triggers.StaleKey("portfolio", 1), "", "", ""},
		triggers.KindDataStale + "/ok":    {triggers.StaleClearKey("futures", 1), "", "", ""},
		triggers.KindLiquidationDistance:  {triggers.LiquidationKey("okx/main", "ETH/USDT:USDT", "long", "critical"), "ETH", "ETH", ""},
		triggers.KindLiquidationDistance + "/key": {
			triggers.LiquidationKey("okx/main", "BTC/USDT:USDT", "short", "warning"), "", "BTC", ""},
		triggers.KindDNHealth: {triggers.DNHealthKey("3", "margin_critical"), "", "", "3"},
		triggers.KindDCADue:   {triggers.DCADueKey(12, 1773118800000), "", "", "12"},
		triggers.KindExternal: {triggers.ExternalKey("deploy-42", hash), "", "", ""},
		// only the asset: the event type and the direction are not read as subjects
		triggers.KindNewsRelevant: {triggers.NewsKey("SOL", "security", "down"), "", "SOL", ""},
		// the intent's id is not an asset or a plan: the sweep reads no subject from it
		triggers.KindGoalDiscrepancy: {triggers.GoalKey("01HABCDEF"), "", "", ""},
	} {
		topic := sweep.Topic{Key: tc.key, Kind: sweep.KindOf(tc.key), Asset: tc.logAsset}
		wantKind := strings.SplitN(name, "/", 2)[0]
		if topic.Kind != wantKind {
			t.Errorf("%s: key %q has kind %q", name, tc.key, topic.Kind)
		}
		if a, p := sweep.TopicSubject(topic); a != tc.asset || p != tc.plan {
			t.Errorf("%s: key %q gives asset %q, plan %q; want %q, %q", name, tc.key, a, p, tc.asset, tc.plan)
		}
	}
}

// The sweep's counters are on the loopback stats route with the engine's and the
// registry's, counts only, so that "how many model calls did the heartbeat make"
// can be read off a running gateway.
func TestSweepStats_OnTheStatsRoute(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, compose(sweepConfig(), func(c *config.Config) { c.Heartbeat.Enabled = false }))
	rt := f.services.Perception

	sweeps := func() *sweepStats {
		t.Helper()
		code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
		if err != nil || code != 200 {
			t.Fatalf("stats: %d %v", code, err)
		}
		var got busStatsResponse
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		if p, ok := raw["perception"].(map[string]any); ok && p["sweep"] != nil {
			assertNumbersOnly(t, "perception.sweep", p["sweep"])
		}
		return got.Perception.Sweep
	}
	run := &fakeRunner{res: agent.ProactiveTurnResult{Decisions: []proactive.Decision{{Kind: proactive.DecideSilent}}}}
	eff := config.DefaultConfig().Proactive.Effective()
	eff.HeartbeatMode = config.HeartbeatModeSweep
	h := sweepHandler(assemblyDeps{Shared: f.shared}, f.services, eff, run, time.Hour)
	if h == nil {
		t.Fatal("no sweep handler")
	}
	if s := sweeps(); s == nil || *s != (sweepStats{}) {
		t.Fatalf("before any sweep: %+v", s)
	}

	h(context.Background(), "p", "", "") // nothing changed since the mark: no model call
	emitMove(t, rt, 60000, 54000)
	h(context.Background(), "p", "", "") // a 10% move: one model call
	h(context.Background(), "p", "", "") // the mark moved: nothing new

	want := sweepStats{Sweeps: 3, NoChanges: 2, ModelCalls: 1}
	if s := sweeps(); s == nil || *s != want {
		t.Fatalf("sweep stats = %+v, want %+v", s, want)
	}
	if n := len(run.requests()); n != 1 {
		t.Fatalf("%d model calls", n)
	}
}
