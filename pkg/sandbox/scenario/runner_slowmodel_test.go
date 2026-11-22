package scenario

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
)

// A model that takes longer than the settle cap must not fail the run: the instance's settle
// waits for the call in flight, so the runner settles, advances and evaluates only after it.
// This is the runner end to end over a real lab: a component stays busy for 1.2 s with a
// 300 ms cap, and a model call is "in flight" meanwhile.
func TestRunner_SlowModelCallIsWaitedFor(t *testing.T) {
	sup := newFakeSup(t)
	var inFlight atomic.Bool
	sup.onUp = func(_ int, in *fakeInstance) {
		in.lab.Quiesce.SetExternalWait(inFlight.Load)
		inFlight.Store(true)
		in.busy.set(true)
		go func() {
			time.Sleep(1200 * time.Millisecond)
			in.busy.set(false)
			inFlight.Store(false)
		}()
	}
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - advance: 5m\n"))
	res := rep.Runs[0]
	if !res.Pass || res.Infra != "" || res.Failure != "" || len(res.Settle) != 0 {
		t.Fatalf("result = %+v: a slow model call is waited for, not failed", res)
	}
}

// Control: the same busy component with no model call in flight still fails the settle, so
// the wait is for the model and not a general softening of the cap.
func TestRunner_StuckComponentStillFailsTheSettle(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) { in.busy.set(true) }
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - advance: 5m\n"))
	res := rep.Runs[0]
	if res.Pass || rep.ExitCode() != ExitInfra || len(res.Settle) == 0 {
		t.Fatalf("result = %+v: a stuck component with no model call in flight is infrastructure", res)
	}
}

// settleLoop asks again while a model call is in flight when the op gives up (its calls last
// 25 s at most), and stops when a settle passes, when nothing is in flight, or when the
// context ends.
func TestSettleLoop_AsksAgainWhileTheModelIsInFlight(t *testing.T) {
	calls := 0
	once := func() (settleOut, error) {
		calls++
		if calls < 4 {
			return settleOut{Busy: []string{"agent"}, ModelInFlight: true}, nil
		}
		return settleOut{Settled: true}, nil
	}
	if err := settleLoop(context.Background(), once); err != nil || calls != 4 {
		t.Fatalf("err %v after %d calls, want nil after 4", err, calls)
	}

	var se *SettleError
	calls = 0
	if err := settleLoop(context.Background(), func() (settleOut, error) {
		calls++
		return settleOut{Busy: []string{"agent"}, WallMS: 10000}, nil
	}); !errors.As(err, &se) || calls != 1 || len(se.Busy) != 1 {
		t.Fatalf("err %v after %d calls: no model in flight means one ask and a SettleError", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	calls = 0
	if err := settleLoop(ctx, func() (settleOut, error) {
		calls++
		cancel()
		return settleOut{Busy: []string{"agent"}, ModelInFlight: true}, nil
	}); !errors.As(err, &se) || calls != 1 {
		t.Fatalf("err %v after %d calls: a cancelled context ends the loop", err, calls)
	}
}

// slowStub is a model whose call never ends within its limit.
type slowStub struct{}

func (slowStub) Chat(ctx context.Context, _ []protocoltypes.Message, _ []protocoltypes.ToolDefinition, _ string, _ map[string]any) (*protocoltypes.LLMResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A model call that runs over its wall limit makes the run infrastructure (exit 3), not a
// failed expectation, even when the other calls were answered and every expectation held.
func TestRunner_LiveCallTimeoutIsInfra(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		e, err := sim.NewEngine(sim.EngineOptions{Journal: simJournal{in.j}, Mode: sim.ModeLive, BudgetTok: 1_000_000,
			TurnInfo: func(context.Context) sim.CallInfo { return sim.CallInfo{Turn: "heartbeat", TurnID: "t1"} }})
		if err != nil {
			t.Fatal(err)
		}
		msgs := []protocoltypes.Message{{Role: "system", Content: "SYS"}, {Role: "user", Content: "heartbeat"}}
		opts := map[string]any{sim.OptionCacheKey: "main", "max_tokens": 100}
		ok := &sim.LiveOptions{Inner: stubModel{}, InnerModel: "m", Price: sim.Price{In: 3, Out: 15}}
		if _, err := e.ChatWith(context.Background(), ok, msgs, nil, "m", opts); err != nil {
			t.Fatal(err)
		}
		slow := &sim.LiveOptions{Inner: slowStub{}, InnerModel: "m", Price: sim.Price{In: 3, Out: 15}, Timeout: 30 * time.Millisecond}
		if _, err := e.ChatWith(context.Background(), slow, msgs, nil, "m", opts); !errors.Is(err, sim.ErrCallTimeout) {
			t.Fatalf("the slow call returned %v, want ErrCallTimeout", err)
		}
	}
	rep, err := liveRunner(sup).Run(context.Background(), scenarioOf(t, h9Like))
	if err != nil {
		t.Fatal(err)
	}
	res := rep.Runs[0]
	if !ownExpectationsHeld(res) {
		t.Fatalf("the file's own expectations must hold here: %+v", res.Expect)
	}
	if res.Pass || rep.ExitCode() != ExitInfra || rep.Verdict() != "infra" || !strings.Contains(res.Infra, "ran past the wall timeout") {
		t.Fatalf("exit %d (%s), infra %q: a model call that timed out must be infrastructure", rep.ExitCode(), rep.Verdict(), res.Infra)
	}
	if lm := liveModel(t, res); lm.OK || !strings.Contains(lm.Got, "1 of 2 model calls answered") || !strings.Contains(lm.Got, "--llm-call-timeout") {
		t.Errorf("live_model = %+v", lm)
	}
}

// A local server needs no key: the runner does not skip a live file for the missing key when
// the model is openai/<id> on a loopback base, and still does for any other.
func TestRunner_LocalModelIsNotSkippedForWantOfAKey(t *testing.T) {
	noKey := func(string) string { return "" }
	for name, c := range map[string]struct {
		model, base string
		skipped     bool
	}{
		"openai on loopback":    {"openai//models/x", "http://127.0.0.1:8080/v1", false},
		"openai, no base":       {"openai/gpt-x", "", true},
		"anthropic on loopback": {"anthropic-messages/m", "http://127.0.0.1:8080/v1", true},
	} {
		sup := newFakeSup(t)
		r := &Runner{Sup: sup, SettleCap: 300 * time.Millisecond, KnownBugs: true, LLMModel: c.model, LLMBudget: "1000tok", LLMAPIBase: c.base, Getenv: noKey}
		rep, err := r.Run(context.Background(), scenarioOf(t, h9Like))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(rep.Skipped, "no key"); got != c.skipped {
			t.Errorf("%s: skipped = %q, want skipped=%v", name, rep.Skipped, c.skipped)
		}
	}
}

// The runner hands the base and the call timeout to the instance, for live files only.
func TestRunner_PassesTheAPIBaseAndTimeoutToLiveInstances(t *testing.T) {
	sup := newFakeSup(t)
	r := &Runner{Sup: sup, SettleCap: 300 * time.Millisecond, KnownBugs: true, LLMModel: "openai/m", LLMBudget: "1000tok",
		LLMAPIBase: "http://127.0.0.1:8080/v1", LLMCallTimeout: "90s", Getenv: func(string) string { return "" }}
	if _, err := r.Run(context.Background(), scenarioOf(t, h9Like)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), scenarioOf(t, "")); err != nil { // a scripted file
		t.Fatal(err)
	}
	if len(sup.upOpts) != 2 {
		t.Fatalf("%d ups", len(sup.upOpts))
	}
	if o := sup.upOpts[0]; o.LLMAPIBase != "http://127.0.0.1:8080/v1" || o.LLMCallTimeout != "90s" || o.LLM != "live" {
		t.Errorf("live up options = %+v", o)
	}
	if o := sup.upOpts[1]; o.LLMAPIBase != "" || o.LLMCallTimeout != "" || o.LLMModel != "" {
		t.Errorf("a scripted file got model options: %+v", o)
	}
}

// A run_until that runs out of wall time is the model's slowness on a live run: infrastructure,
// never a failed step; on a scripted run it stays a failed step.
func TestRunner_LiveRunUntilOutOfWallTimeIsInfra(t *testing.T) {
	const timeline = "timeline:\n  - run_until: {until: {message: {to: owner, contains: [never]}}, step: 1m, max_sim: 6h, max_wall: 1ms}\n"
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		if errs := liveCalls(t, in, 2, nil); errs[0] != nil {
			t.Fatalf("the engine returned %v, want an answer", errs)
		}
	}
	rep, err := liveRunner(sup).Run(context.Background(), scenarioOf(t, h9Like+timeline))
	if err != nil {
		t.Fatal(err)
	}
	if res := rep.Runs[0]; res.Pass || res.Failure != "" || !strings.Contains(res.Infra, "max_wall") || rep.ExitCode() != ExitInfra {
		t.Fatalf("exit %d, result %+v: running out of wall time on a live run is infrastructure", rep.ExitCode(), res)
	}
	if rep := runOne(t, newFakeSup(t), scenarioOf(t, timeline)); rep.Runs[0].Failure == "" || rep.Runs[0].Infra != "" {
		t.Fatalf("a scripted run that ran out of wall time must stay a failed step: %+v", rep.Runs[0])
	}
}
