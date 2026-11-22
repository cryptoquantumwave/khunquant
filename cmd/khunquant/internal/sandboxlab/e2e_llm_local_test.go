//go:build !windows

package sandboxlab

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim/simtest"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/scenario"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

// H9-0 end to end, with the real binary and a fake local model (an OpenAI-compatible server on
// loopback that answers slowly). Nothing reads or sets a real key: the starter's environment
// holds no KQ_SANDBOX_LLM_KEY at all, which is the point of the local path.

const localE2EModel = "openai//models/local-test"

const localScenario = `version: 1
scenario: local-slow-model
clock: {start: "2026-10-20T09:00:00+07:00"}
llm: live
setup:
  - chat: {as: owner, text: "hello"}
timeline:
  - advance: 2m
expect:
  - id: owner-got-the-reply
    message: {to: owner, contains: ["slow local answer"], count: ">= 1"}
`

func localSup(t *testing.T, bin string) *supervisor.Supervisor {
	t.Helper()
	sandboxEnv(t)
	m := map[string]string{"KQ_SANDBOX_LLM_MODELS": localE2EModel} // no key, no prices
	sup, err := supervisor.New(supervisor.Config{Binary: bin, Getenv: func(k string) string { return m[k] }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Close(context.Background()) })
	return sup
}

func localRunner(t *testing.T, sup *supervisor.Supervisor, base, callTimeout string) *scenario.Runner {
	t.Helper()
	return &scenario.Runner{Sup: sup, Timeout: 4 * time.Minute, FixturesFrom: repoFixtures(t), SettleCap: 10 * time.Second,
		LLMModel: localE2EModel, LLMBudget: "20000000tok", LLMAPIBase: base, LLMCallTimeout: callTimeout,
		Getenv: func(string) string { return "" }}
}

// A model that takes 13 s to answer, longer than the 10 s settle cap and the 20 s advance
// budget's first half: the run neither fails its settle nor reads the owner's reply too early.
// It needs no key, spends tokens (not dollars) and the owner is told.
func TestE2E_LocalSlowModelIsWaitedFor(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	fake.Reply = func([]byte) string { return "slow local answer" }
	fake.Delay = 13 * time.Second
	f, err := scenario.Parse(strings.NewReader(localScenario))
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	rep := runScenario(t, localRunner(t, localSup(t, bin), fake.Base(), ""), f)
	res := rep.Runs[0]
	if !rep.Pass || res.Infra != "" || res.Failure != "" || len(res.Settle) != 0 {
		t.Fatalf("a slow model must be waited for: pass=%v infra=%q failure=%q settle=%v\n%s", rep.Pass, res.Infra, res.Failure, res.Settle, rep.Markdown())
	}
	if took := time.Since(began); took < 13*time.Second {
		t.Errorf("the run took %v: the model's 13 s were not waited for", took)
	}
	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("the model was never called")
	}
	for _, r := range reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-sandbox-local-no-key" {
			t.Errorf("the local server was sent %q, want the fixed placeholder", got)
		}
	}
}

// A model that never answers in time: the call is cut at the wall limit and the run is
// infrastructure (exit 3), not a failed expectation.
func TestE2E_LocalModelTimeoutIsInfra(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	fake.Delay = time.Minute
	f, err := scenario.Parse(strings.NewReader(localScenario))
	if err != nil {
		t.Fatal(err)
	}
	rep := runScenario(t, localRunner(t, localSup(t, bin), fake.Base(), "3s"), f)
	res := rep.Runs[0]
	if res.Pass || rep.ExitCode() != scenario.ExitInfra || !strings.Contains(res.Infra, "ran past the wall timeout") {
		t.Fatalf("exit %d, infra %q, failure %q: a model call that timed out must be infrastructure\n%s", rep.ExitCode(), res.Infra, res.Failure, rep.Markdown())
	}
}

// The same inside an advance: a heartbeat fires at +30m and its model call takes 24 s, longer
// than an advance call's 20 s wall budget. The first call is cut inside the settle after the
// step (done:false), the runner calls again, the instance settles that step before it moves,
// and the clock never runs on over the call in flight. The run passes, and the call is in the
// journal at +30m of sim time whatever wall time it took.
func TestE2E_LocalSlowModelInsideAnAdvance(t *testing.T) {
	bin := e2eBinary(t)
	fake := simtest.New(t)
	fake.Reply = func([]byte) string { return "HEARTBEAT_OK" }
	fake.Delay = 24 * time.Second
	const body = `version: 1
scenario: local-slow-heartbeat
clock: {start: "2026-10-20T09:00:00+07:00"}
llm: live
instance:
  config: { heartbeat: { enabled: true, interval: 30 } }
  workspace: { HEARTBEAT.md: "# Heartbeat tasks\n- Say HEARTBEAT_OK.\n" }
timeline:
  - advance: 31m
expect:
  - id: the-heartbeat-ran-at-30m-of-sim-time
    llm: {turn: heartbeat_legacy}
    count: ">= 1"
    within: {from: "+29m", to: "+31m"}
`
	f, err := scenario.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	rep := runScenario(t, localRunner(t, localSup(t, bin), fake.Base(), ""), f)
	res := rep.Runs[0]
	if !rep.Pass || res.Infra != "" || res.Failure != "" || len(res.Settle) != 0 {
		t.Fatalf("a 24 s model call inside an advance must be waited for: pass=%v infra=%q failure=%q settle=%v\n%s", rep.Pass, res.Infra, res.Failure, res.Settle, rep.Markdown())
	}
	if took := time.Since(began); took < 24*time.Second {
		t.Errorf("the run took %v: the model's 24 s were not waited for", took)
	}
}
