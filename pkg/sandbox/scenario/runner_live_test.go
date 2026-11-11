package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
)

// A live run is checked by the runner itself (live_model, 193-1): when no model call was
// answered, or the budget refused one, the run is infrastructure (exit 3) whatever its
// expectations say. The journal in these tests is written by the real simulated model in
// live mode (providers/sim) with a stand-in for the real model, so its entries are the ones
// a live instance writes.

// simJournal hands the engine's entries to a fake instance's journal, as the gateway does.
type simJournal struct{ j *labjournal.Journal }

func (s simJournal) Record(kind, subject string, payload json.RawMessage) {
	s.j.Append(labjournal.Entry{Kind: kind, Source: "sim", Subject: subject, Payload: payload})
}

// stubModel stands in for the real model, with no network: a nil err answers.
type stubModel struct{ err error }

func (m stubModel) Chat(context.Context, []protocoltypes.Message, []protocoltypes.ToolDefinition, string, map[string]any) (*protocoltypes.LLMResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &protocoltypes.LLMResponse{Content: "SOL fell 8% in the last hour.", FinishReason: "stop",
		Usage: &protocoltypes.UsageInfo{PromptTokens: 1000, CompletionTokens: 200}}, nil
}

// liveCalls makes one heartbeat model call per outcome (nil: the model answers, an error:
// the call fails) through a live engine whose budget is usd, journaling into the instance,
// and returns what each call returned. At 3/15 USD per million tokens an answered call
// costs 0.006 USD.
func liveCalls(t *testing.T, in *fakeInstance, usd float64, outcomes ...error) []error {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineOptions{Journal: simJournal{in.j}, Mode: sim.ModeLive, BudgetUSD: usd,
		TurnInfo: func(context.Context) sim.CallInfo { return sim.CallInfo{Turn: "heartbeat", TurnID: "t1"} }})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []protocoltypes.Message{{Role: "system", Content: "SYS"}, {Role: "user", Content: "heartbeat"}}
	var errs []error
	for _, o := range outcomes {
		lo := &sim.LiveOptions{Inner: stubModel{o}, InnerModel: "m", Price: sim.Price{In: 3, Out: 15}}
		_, err := e.ChatWith(context.Background(), lo, msgs, nil, "m", map[string]any{sim.OptionCacheKey: "main", "max_tokens": 100})
		errs = append(errs, err)
	}
	return errs
}

// liveRunner runs scenarios as `sandbox run --llm live` does with a key set.
func liveRunner(sup *fakeSup) *Runner {
	return &Runner{Sup: sup, SettleCap: 300 * time.Millisecond, KnownBugs: true, LLMModel: "anthropic-messages/m", LLMBudget: "2USD",
		Getenv: func(k string) string {
			if k == sandbox.EnvLLMKey {
				return "sk-test-not-a-real-key"
			}
			return ""
		}}
}

// h9Like heads a live known_bug scenario such as H9, which the opt-in workflow runs with
// KQ_SCENARIO_KNOWN_BUGS=1, reading exit 1 as "the bug is still there".
const h9Like = "llm: live\nstatus: known_bug\nknown_bug: H9\n"

// liveModel is the run's live_model check.
func liveModel(t *testing.T, res *Result) ExpectResult {
	t.Helper()
	for _, e := range res.Expect {
		if e.ID == "live_model" {
			return e
		}
	}
	t.Fatalf("no live_model check in %+v", res.Expect)
	return ExpectResult{}
}

// ownExpectationsHeld says whether every expectation the file wrote held: without the live
// check such a run passes.
func ownExpectationsHeld(res *Result) bool {
	return !slices.ContainsFunc(res.Expect, func(e ExpectResult) bool { return e.Kind != "implicit" && !e.OK })
}

// The budget refused a call: the run is infrastructure, although the model answered once
// and every expectation of the file held.
func TestRunner_LiveBudgetRefusalIsInfra(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		// The second call reserves what the first cost, 0.006, which 0.007 cannot cover twice.
		if errs := liveCalls(t, in, 0.007, nil, nil); errs[0] != nil || !errors.Is(errs[1], sim.ErrBudget) {
			t.Fatalf("the engine returned %v, want an answer and then the budget refusal", errs)
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
	if res.Pass || rep.ExitCode() != ExitInfra || rep.Verdict() != "infra" || !strings.Contains(res.Infra, "the budget refused 1") {
		t.Fatalf("exit %d (%s), infra %q: a run the budget cut short must be infrastructure", rep.ExitCode(), rep.Verdict(), res.Infra)
	}
	if lm := liveModel(t, res); lm.OK || !strings.Contains(lm.Got, "1 of 2 model calls answered") {
		t.Errorf("live_model = %+v", lm)
	}
	if len(res.LLMCalls) != 2 {
		t.Errorf("the report holds %d model calls, want both", len(res.LLMCalls))
	}
}

// No call was answered (a wrong model id, a bad key, an unreachable host), or none was made:
// the run is infrastructure, and the report names the model's error.
func TestRunner_LiveWithNoAnsweredCallIsInfra(t *testing.T) {
	notFound := errors.New("anthropic: 404 not_found_error: model: claude-sonnet-4.6")
	for name, tc := range map[string]struct {
		outcomes []error
		want     string
	}{
		"every call failed": {[]error{notFound, notFound}, "0 of 2 model calls answered; first error: anthropic: 404"},
		"no call was made":  {nil, "no model call was made"},
	} {
		sup := newFakeSup(t)
		sup.onUp = func(_ int, in *fakeInstance) {
			if errs := liveCalls(t, in, 2, tc.outcomes...); slices.ContainsFunc(errs, func(err error) bool { return err == nil }) {
				t.Fatalf("%s: the engine answered a call: %v", name, errs)
			}
		}
		rep, err := liveRunner(sup).Run(context.Background(), scenarioOf(t, h9Like))
		if err != nil {
			t.Fatal(err)
		}
		res := rep.Runs[0]
		if !ownExpectationsHeld(res) {
			t.Fatalf("%s: the file's own expectations must hold here: %+v", name, res.Expect)
		}
		if res.Pass || rep.ExitCode() != ExitInfra || !strings.Contains(res.Infra, tc.want) {
			t.Errorf("%s: exit %d, infra %q: a run the model did not serve must be infrastructure", name, rep.ExitCode(), res.Infra)
		}
		if !strings.Contains(rep.Markdown(), tc.want) {
			t.Errorf("%s: the report does not say why:\n%s", name, rep.Markdown())
		}
	}
}

// One call failed (a transient 529) and one was answered: not infrastructure, and the run
// passes on its expectations.
func TestRunner_LiveTransientErrorIsNotInfra(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		if errs := liveCalls(t, in, 2, errors.New("anthropic: 529 overloaded_error"), nil); errs[0] == nil || errs[1] != nil {
			t.Fatalf("the engine returned %v, want a failure and then an answer", errs)
		}
	}
	rep, err := liveRunner(sup).Run(context.Background(), scenarioOf(t, h9Like))
	if err != nil {
		t.Fatal(err)
	}
	res := rep.Runs[0]
	if !res.Pass || res.Infra != "" || rep.ExitCode() != 0 {
		t.Fatalf("exit %d, result %+v: one failed call among answered ones must not fail the run", rep.ExitCode(), res)
	}
	if lm := liveModel(t, res); !lm.OK || lm.Got != "1 of 2 model calls answered" {
		t.Errorf("live_model = %+v", lm)
	}
}

// A run the model served still fails on its own expectations as a finding (exit 1): the
// check does not turn a real failure into infrastructure.
func TestRunner_LiveFindingStaysAFinding(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) {
		if errs := liveCalls(t, in, 2, nil); errs[0] != nil {
			t.Fatalf("the engine returned %v, want an answer", errs)
		}
	}
	f := scenarioOf(t, h9Like+"expect:\n  - id: told\n    message: {to: owner, contains: [SOL], count: \">= 1\"}\n")
	rep, err := liveRunner(sup).Run(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if res := rep.Runs[0]; res.Pass || res.Infra != "" || rep.ExitCode() != ExitFailed {
		t.Fatalf("exit %d, result %+v: an unmet expectation of a served run is a finding", rep.ExitCode(), res)
	}
}

// The check is keyed on the file's llm: a scripted file is not checked, whether it makes no
// model call or runs on a live instance whose journal holds a refusal or failed calls (the
// record and budget E2Es run scripted files on live instances).
func TestRunner_ScriptedRunHasNoLiveCheck(t *testing.T) {
	for name, journal := range map[string]func(*fakeInstance){
		"no model call":     nil,
		"a budget refusal":  func(in *fakeInstance) { liveCalls(t, in, 0.007, nil, nil) },
		"every call failed": func(in *fakeInstance) { liveCalls(t, in, 2, errors.New("anthropic: 401 authentication_error")) },
	} {
		sup := newFakeSup(t)
		if journal != nil {
			sup.onUp = func(_ int, in *fakeInstance) { journal(in) }
		}
		rep, err := liveRunner(sup).Run(context.Background(), scenarioOf(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		res := rep.Runs[0]
		if !res.Pass || res.Infra != "" || rep.ExitCode() != 0 {
			t.Errorf("%s: exit %d, result %+v: a scripted run must not be checked as a live one", name, rep.ExitCode(), res)
		}
		for _, e := range res.Expect {
			if e.ID == "live_model" {
				t.Errorf("%s: a scripted run has a live_model check", name)
			}
		}
	}
}
