package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
)

func withPerception(cfg *config.Config) { cfg.Proactive.Perception.Enabled = true }

func liveWithPerception() func(*config.Config) {
	return compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withPerception)
}

// goroutinesIn counts goroutines whose stack mentions substr.
func goroutinesIn(substr string) int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, substr) {
			n++
		}
	}
	return n
}

const (
	engineRunFrame = "engine.(*Engine).Run"
	superviseFrame = "perception.(*Manager).supervise"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// countingSource is a perception source that records how many are running.
type countingSource struct {
	name    string
	starts  atomic.Int32
	active  atomic.Int32
	maxSeen atomic.Int32
}

func (c *countingSource) Name() string { return c.name }
func (c *countingSource) Start(ctx context.Context, _ perception.Emitter) error {
	c.starts.Add(1)
	n := c.active.Add(1)
	for {
		m := c.maxSeen.Load()
		if n <= m || c.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	<-ctx.Done()
	c.active.Add(-1)
	return nil
}
func (c *countingSource) Reconcile(context.Context, perception.Emitter) error { return nil }
func (c *countingSource) Health() perception.SourceHealth                     { return perception.SourceHealth{} }

func useSources(t *testing.T, takeOver bool, srcs ...perception.Source) {
	t.Helper()
	prev := perceptionSources
	perceptionSources = func(assemblyDeps) ([]perception.Source, bool) { return srcs, takeOver }
	t.Cleanup(func() { perceptionSources = prev })
}

func TestPerception_DefaultConfigBuildsNothing(t *testing.T) {
	before := goroutinesIn(engineRunFrame) + goroutinesIn(superviseFrame)
	for name, mut := range map[string]func(*config.Config){
		"default":                    nil,
		"perception without enabled": withPerception, // proactive itself is off
		"enabled, attention off":     compose(withOwner("1001"), withPerception, withAttentionMode(config.ProactiveModeOff)),
		"enabled, no owner":          compose(enabled, withPerception, withAttentionMode(config.ProactiveModeOff)),
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mut)
			if f.services.Perception != nil || f.services.perceptionLive.Load() != nil {
				t.Fatal("perception built")
			}
			if n := goroutinesIn(engineRunFrame) + goroutinesIn(superviseFrame); n != before {
				t.Fatalf("%d perception goroutines running, want none", n-before)
			}
			code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
			if err != nil || code != http.StatusOK {
				t.Fatalf("stats: %d %v", code, err)
			}
			if strings.Contains(string(body), `"perception"`) {
				t.Fatalf("stats mention perception: %s", body)
			}
		})
	}
}

func TestPerception_BuiltWithLiveAttentionAndStopsClean(t *testing.T) {
	src := &countingSource{name: "fake"}
	useSources(t, false, src)
	base := goroutinesIn(engineRunFrame)
	f := newAssemblyFixture(t, liveWithPerception())
	rt := f.services.Perception
	if rt == nil || f.services.Attention == nil || f.services.attentionRun == nil {
		t.Fatal("perception, or the live outbox under it, was not built")
	}
	if goroutinesIn(engineRunFrame) != base+1 {
		t.Fatal("the engine goroutine is not running")
	}
	waitUntil(t, "the source", func() bool { return src.active.Load() == 1 })
	if _, isComposer := f.services.Attention.Composer.(*engine.Composer); !isComposer {
		t.Fatal("the outbox has no composer although perception is on")
	}

	// an event emitted by a source reaches the pipeline, and the stats route counts it.
	rt.engine.Emitter().Emit(events.Event{Kind: events.KindPerceptionMarketTick, Source: events.Source{Component: "t"}})
	waitUntil(t, "the event", func() bool { return rt.engine.Stats().Processed == 1 })
	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v", code, err)
	}
	var got busStatsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Perception == nil || got.Perception.Engine.Processed != 1 || got.Perception.Manager.Sources != 1 ||
		got.Perception.Sources["fake"].ConsecutiveErrors != 0 {
		t.Fatalf("perception stats = %+v (%s)", got.Perception, body)
	}
	// counts only.
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	assertNumbersOnly(t, "perception", raw["perception"])

	// stop: the engine and the source are gone. (stopServices does exactly this at
	// its perception step; the fixture cleanup runs it again, which must be harmless.)
	rt.stop()
	waitUntil(t, "no engine goroutine", func() bool { return goroutinesIn(engineRunFrame) == base })
	if src.active.Load() != 0 || goroutinesIn(superviseFrame) != 0 {
		t.Fatal("a source goroutine outlived the stop")
	}
}

// assertNumbersOnly fails if v holds anything but numbers, booleans and maps and
// arrays of them: no strings, so no event, payload or error text can be on the wire.
func assertNumbersOnly(t *testing.T, path string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			assertNumbersOnly(t, path+"."+k, e)
		}
	case []any:
		for _, e := range x {
			assertNumbersOnly(t, path+"[]", e)
		}
	case float64, bool:
	default:
		t.Errorf("%s is %T: the stats route must carry counts only", path, v)
	}
}

func TestPerception_ShadowAttentionBuildsTheEngineWithoutDelivery(t *testing.T) {
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeShadow), withPerception))
	if f.services.Perception == nil {
		t.Fatal("perception not built although the outbox exists")
	}
	if f.services.attentionRun != nil {
		t.Fatal("a delivery Run loop in shadow mode")
	}
}

func TestPerception_ReloadDoesNotDuplicateSources(t *testing.T) {
	src := &countingSource{name: "fake"}
	useSources(t, false, src)
	f := newAssemblyFixture(t, liveWithPerception())
	first := f.services.Perception
	waitUntil(t, "the source", func() bool { return src.active.Load() == 1 })

	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	second := f.services.Perception
	if second == nil || second == first {
		t.Fatal("perception not rebuilt on reload")
	}
	waitUntil(t, "the rebuilt source", func() bool { return src.starts.Load() == 2 && src.active.Load() == 1 })
	if src.maxSeen.Load() != 1 {
		t.Fatalf("%d copies of the source ran at once", src.maxSeen.Load())
	}
	if goroutinesIn(engineRunFrame) != 1 {
		t.Fatalf("%d engine goroutines after a reload, want 1", goroutinesIn(engineRunFrame))
	}
	select {
	case <-first.engDone:
	default:
		t.Fatal("the old engine outlived the reload")
	}
	if first.manager.Stats().Sources != 1 || second.manager.Stats().Sources != 1 {
		t.Fatal("source counts off")
	}

	// reloading perception off removes it, with no goroutine left behind.
	if err := f.reload(compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if f.services.Perception != nil {
		t.Fatal("perception survived being switched off")
	}
	waitUntil(t, "everything stopped", func() bool {
		return src.active.Load() == 0 && goroutinesIn(engineRunFrame) == 0 && goroutinesIn(superviseFrame) == 0
	})
}

func TestPerception_DeviceServiceIsKeptUnlessTheAdapterTakesOver(t *testing.T) {
	devicesOn := func(cfg *config.Config) {
		cfg.Devices.Enabled = true
		cfg.Devices.MonitorUSB = true
	}
	t.Run("default keeps the legacy service", func(t *testing.T) {
		f := newAssemblyFixture(t, compose(liveWithPerception(), devicesOn))
		if f.services.Perception == nil || f.services.DeviceService == nil {
			t.Fatalf("perception %v, device service %v: the legacy service must keep running until the adapter has a consumer",
				f.services.Perception != nil, f.services.DeviceService != nil)
		}
	})
	t.Run("a wired adapter replaces it", func(t *testing.T) {
		useSources(t, true, &countingSource{name: "devices:fake"})
		f := newAssemblyFixture(t, compose(liveWithPerception(), devicesOn))
		if f.services.Perception == nil || !f.services.Perception.devicesTakenOver {
			t.Fatal("take-over not recorded")
		}
		if f.services.DeviceService != nil {
			t.Fatal("the legacy device service was started next to the adapter")
		}
	})
	t.Run("perception off leaves devices alone", func(t *testing.T) {
		f := newAssemblyFixture(t, compose(withOwner("1001"), devicesOn))
		if f.services.Perception != nil || f.services.DeviceService == nil {
			t.Fatal("device service missing")
		}
	})
}

// A stub of the agent for the composer adapter: what the gateway hands the model.
type fakeAgent struct {
	mu   sync.Mutex
	reqs []agent.ProactiveTurnRequest
	res  agent.ProactiveTurnResult
	err  error
}

func (f *fakeAgent) RunProactiveTurn(_ context.Context, req agent.ProactiveTurnRequest) (agent.ProactiveTurnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.res, f.err
}

func TestAgentTurnRunner_AdaptsTheRequestAndLeavesDefaults(t *testing.T) {
	fa := &fakeAgent{res: agent.ProactiveTurnResult{
		Decisions: []proactive.Decision{{Kind: proactive.DecideNotify, Text: "hi"}}, FinalText: "f", ContractViolated: true,
	}}
	r := agentTurnRunner{run: fa}
	cands := []proactive.Candidate{{DedupKey: "k", ForcedSkills: []string{"s"}}}
	res, err := r.RunProactiveTurn(context.Background(), engine.TurnRequest{
		Kind: proactive.TurnProactive, Prompt: "p", Candidates: cands, ForcedSkills: []string{"s"},
	})
	if err != nil || len(res.Decisions) != 1 || res.FinalText != "f" || !res.ContractViolated {
		t.Fatalf("result = %+v, %v", res, err)
	}
	got := fa.reqs[0]
	if got.Kind != proactive.TurnProactive || got.Prompt != "p" || got.Model != "" || len(got.Tools) != 0 ||
		got.Deadline != 0 || len(got.ForcedSkills) != 1 || got.ForcedSkills[0] != "s" || len(got.Candidates) != 1 {
		t.Fatalf("agent request = %+v: model, tools and deadline must stay at the turn's defaults", got)
	}
	fa.err = errors.New("down")
	if _, err := r.RunProactiveTurn(context.Background(), engine.TurnRequest{Kind: proactive.TurnProactive}); err == nil {
		t.Fatal("error swallowed")
	}
}
