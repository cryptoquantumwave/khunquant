package scenario

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labclient"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/supervisor"
)

func runOne(t *testing.T, sup Instances, f *File) *Report {
	t.Helper()
	rep, err := (&Runner{Sup: sup, SettleCap: 300 * time.Millisecond}).Run(context.Background(), f)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func journalKinds(t *testing.T, in *fakeInstance, prefix string) []string {
	t.Helper()
	c, err := labclient.New(in.srv.URL, fakeToken)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := call(context.Background(), c, "inspect", "journal", map[string]any{"kinds": []string{prefix}, "limit": 500})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Entries []struct{ Kind string } }
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range out.Entries {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// The blocks go to their groups in the documented order, the steps follow in sim time,
// and every step is marked.
func TestRunner_AppliesBlocksAndWalksTheTimeline(t *testing.T) {
	sup := newFakeSup(t)
	f := scenarioOf(t, `
profile: {timezone: UTC}
market: {BTC/USDT: {venue: binance, model: set, price: 98000}}
account: {binance/main: {balances: {USDT: 100}}}
llm_script: [{match: {turn: user}, respond: {text: hi}}]
setup:
  - call_tool: {name: set_price_alert, args: {a: 1}}
timeline:
  - at: "+5m"
    chat.channel: {state: down}
  - advance: 10m
  - at: "+30m"
    market.path: {symbol: BTC/USDT, to: 100500, over: 3m}
  - run_until: "+1h"
`)
	rep := runOne(t, sup, f)
	res := rep.Runs[0]
	if res.Infra != "" || res.Failure != "" || len(res.Settle) != 0 || !res.Pass {
		t.Fatalf("result = %+v", res)
	}
	in := sup.order[0]
	got := journalKinds(t, in, "fake.*")
	want := []string{"fake.call_tool.", "fake.market.set", "fake.account.seed", "fake.llm.script_put", "fake.call_tool.", "fake.chat.set_state", "fake.market.path"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("group calls = %v\nwant %v (profile, market, account, llm_script, then the steps)", got, want)
	}
	if len(res.Steps) != 5 || res.Steps[0].Kind != "call_tool" || res.Steps[0].Phase != "setup" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	last := res.Steps[len(res.Steps)-1]
	if want := f.StartTime().Add(time.Hour); !last.Sim.Equal(want) {
		t.Errorf("run_until +1h ended at %v, want %v", last.Sim, want)
	}
	if res.Steps[1].Sim.Sub(f.StartTime()) != 5*time.Minute {
		t.Errorf("at +5m: the step ran at %v", res.Steps[1].Sim)
	}
	if sup.downs[in.name] {
		t.Error("a passing run must not keep its home")
	}
}

// A settle that does not pass fails the run with the busy names (exit 3), and a lossy
// journal fails it even when nothing else is wrong.
func TestRunner_ImplicitSettledAndDrops(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) { in.busy.set(true) }
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - advance: 5m\n"))
	res := rep.Runs[0]
	if res.Pass || len(res.Settle) != 1 || res.Settle[0] != "test-component" || !strings.Contains(res.Infra, "not settled") {
		t.Fatalf("result = %+v, want a settle failure naming test-component", res)
	}
	if rep.ExitCode() != ExitInfra {
		t.Errorf("exit = %d, want 3", rep.ExitCode())
	}

	sup = newFakeSup(t)
	sup.onUp = func(_ int, in *fakeInstance) { in.j.AddDropSource(func() uint64 { return 1 }) }
	rep = runOne(t, sup, scenarioOf(t, "timeline:\n  - advance: 5m\n"))
	res = rep.Runs[0]
	if res.Pass || res.Drops != 1 || res.Infra != "" || rep.ExitCode() != ExitFailed {
		t.Fatalf("result = %+v (exit %d), want a failed run with 1 drop", res, rep.ExitCode())
	}
	for _, e := range res.Expect {
		if e.ID == "journal_drops" && (e.OK || !strings.Contains(e.Got, "1 dropped")) {
			t.Errorf("journal_drops = %+v", e)
		}
	}
}

func TestRunner_KnownBugSkippedUnlessEnv(t *testing.T) {
	sup := newFakeSup(t)
	f := scenarioOf(t, "status: known_bug\nknown_bug: H8\n")
	rep, err := (&Runner{Sup: sup}).Run(context.Background(), f)
	if err != nil || rep.Skipped != "known_bug H8" || len(rep.Runs) != 0 || sup.ups() != 0 || rep.ExitCode() != 0 {
		t.Fatalf("default: %+v (%v), %d instances", rep, err, sup.ups())
	}
	rep, err = (&Runner{Sup: sup, KnownBugs: true, SettleCap: 300 * time.Millisecond}).Run(context.Background(), f)
	if err != nil || rep.Skipped != "" || len(rep.Runs) != 1 || sup.ups() != 1 {
		t.Fatalf("with the switch: %+v (%v), %d instances", rep, err, sup.ups())
	}
}

// 5 runs at pass_rate 0.8 with 4 passing pass, each on a fresh instance.
func TestRunner_RunsAndPassRate(t *testing.T) {
	sup := newFakeSup(t)
	sup.onUp = func(n int, in *fakeInstance) {
		if n == 3 {
			in.j.AddDropSource(func() uint64 { return 1 }) // the third run is lossy
		}
	}
	rep := runOne(t, sup, scenarioOf(t, "runs: 5\npass_rate: 0.8\n"))
	if sup.ups() != 5 || len(rep.Runs) != 5 || rep.Passed != 4 || !rep.Pass || rep.ExitCode() != 0 {
		t.Fatalf("%d instances, %d runs, %d passed, pass=%v, exit %d", sup.ups(), len(rep.Runs), rep.Passed, rep.Pass, rep.ExitCode())
	}
	if rep.Runs[2].Pass || rep.Runs[2].Run != 3 {
		t.Errorf("run 3 = %+v", rep.Runs[2])
	}

	sup = newFakeSup(t)
	sup.onUp = func(n int, in *fakeInstance) {
		if n <= 2 {
			in.j.AddDropSource(func() uint64 { return 1 })
		}
	}
	if rep := runOne(t, sup, scenarioOf(t, "runs: 5\npass_rate: 0.8\n")); rep.Pass || rep.ExitCode() != ExitFailed {
		t.Errorf("3 of 5 at 0.8 must fail: pass=%v exit %d", rep.Pass, rep.ExitCode())
	}
}

func TestRunNotAvailableWhenUnlocked(t *testing.T) {
	sup := newFakeSup(t)
	sup.unlocked = true
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - advance: 5m\n"))
	res := rep.Runs[0]
	if res.Pass || !strings.Contains(res.Infra, "locked:true") || len(res.Steps) != 0 || rep.ExitCode() != ExitInfra {
		t.Fatalf("result = %+v, want the runner to refuse an instance that is not locked, before any step", res)
	}
}

func TestRunner_InstanceThatWillNotStart(t *testing.T) {
	sup := newFakeSup(t)
	sup.upErr = supervisor.ErrTooMany
	rep := runOne(t, sup, scenarioOf(t, ""))
	if rep.Runs[0].Pass || !strings.Contains(rep.Runs[0].Infra, "did not start") || rep.ExitCode() != ExitInfra {
		t.Fatalf("result = %+v", rep.Runs[0])
	}
}

// A feature the instance lacks fails the run before it starts, by name (exit 2).
func TestRunner_RequirementsNameThePR(t *testing.T) {
	sup := newFakeSup(t)
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - fault.add: {kind: latency}\n"))
	res := rep.Runs[0]
	if res.Pass || len(res.Issues) != 1 || !strings.Contains(res.Issues[0].Msg, "SBX-13") || rep.ExitCode() != ExitInvalid {
		t.Fatalf("result = %+v, exit %d", res, rep.ExitCode())
	}
	if len(res.Steps) != 0 {
		t.Error("no step may run when the scenario needs what the instance lacks")
	}
}

// config.patch returns only after the gateway's config.reload entry; a patch that
// changes nothing waits for nothing; the lock's exit 3 is reported with its reason.
func TestConfigPatch_WaitsForReload(t *testing.T) {
	sup := newFakeSup(t)
	patched := make(chan *fakeInstance, 1)
	release := make(chan struct{})
	sup.onPatch = func(in *fakeInstance, _ map[string]any) (bool, error) {
		patched <- in
		go func() {
			<-release
			in.j.Append(labjournal.Entry{Kind: "config.reload", Payload: json.RawMessage(`{"ok":true}`)})
		}()
		return true, nil
	}
	f := scenarioOf(t, "timeline:\n  - config.patch: {heartbeat: {interval: 45}}\n  - advance: 1m\n")
	var done atomic.Bool
	out := make(chan *Report, 1)
	go func() { out <- runOne(t, sup, f); done.Store(true) }()
	<-patched
	time.Sleep(200 * time.Millisecond) // a negative check: only a runner that does not wait could fail it
	if done.Load() {
		t.Fatal("the step returned before config.reload")
	}
	close(release)
	if rep := <-out; !rep.Runs[0].Pass || len(rep.Runs[0].Steps) != 2 {
		t.Fatalf("result = %+v", rep.Runs[0])
	}

	// Nothing changed: no reload is coming, so nothing is waited for.
	sup = newFakeSup(t)
	sup.onPatch = func(*fakeInstance, map[string]any) (bool, error) { return false, nil }
	if rep := runOne(t, sup, f); !rep.Runs[0].Pass {
		t.Fatalf("an unchanged patch: %+v", rep.Runs[0])
	}

	// No reload entry in time: the step fails, and says why.
	old := configReloadWait
	configReloadWait = 400 * time.Millisecond
	defer func() { configReloadWait = old }()
	sup = newFakeSup(t)
	sup.onPatch = func(*fakeInstance, map[string]any) (bool, error) { return true, nil }
	res := runOne(t, sup, f).Runs[0]
	if res.Pass || !strings.Contains(res.Failure, "no config.reload") {
		t.Fatalf("a patch that is never reloaded: %+v", res)
	}
}

// patch -> the lock refuses the reload -> exit 3 -> reported with the gateway's words.
func TestConfigPatch_RefusedByTheLockExit3(t *testing.T) {
	sup := newFakeSup(t)
	sup.logs = []string{"ERR sandbox lock: config refused under the sandbox lock: model_list[1]: protocol \"openai\" is not allowed"}
	sup.onPatch = func(in *fakeInstance, _ map[string]any) (bool, error) {
		sup.exit(in, 3, supervisor.ExitLockInvariant)
		return true, nil
	}
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - config.patch: {model_list: [{model_name: x, model: openai/gpt-5}]}\n"))
	res := rep.Runs[0]
	if res.Pass || rep.ExitCode() != ExitInfra || !strings.Contains(res.Infra, "lock_invariant") || !strings.Contains(res.Infra, "exit 3") ||
		!strings.Contains(res.Infra, `protocol "openai" is not allowed`) {
		t.Fatalf("result = %+v (exit %d)", res, rep.ExitCode())
	}
}

// A market step with no venue goes to the venue the market block priced the symbol on.
func TestRunner_MarketStepDefaultsToTheBlocksVenue(t *testing.T) {
	sup := newFakeSup(t)
	rep := runOne(t, sup, scenarioOf(t, `
market: {BTC/USDT: {venue: binance, model: set, price: 98000}}
timeline:
  - market.path: {symbol: BTC/USDT, to: 100500, over: 3m}
  - market.shock: {symbol: BTC/USDT, venue: okx, pct: 1}
`))
	if !rep.Runs[0].Pass {
		t.Fatalf("result = %+v", rep.Runs[0])
	}
	var got []string
	for _, k := range journalPayloads(t, sup.order[0], "fake.market.*") {
		got = append(got, k)
	}
	if len(got) != 3 || !strings.Contains(got[1], `"venue":"binance"`) || !strings.Contains(got[2], `"venue":"okx"`) {
		t.Errorf("market calls = %v, want set, path (venue binance from the block) and shock (okx as written)", got)
	}
}

// A tool the lab ran that failed fails the step: call_tool answers 200 with is_error.
func TestRunner_ToolErrorFailsTheStep(t *testing.T) {
	sup := newFakeSup(t)
	res := runOne(t, sup, scenarioOf(t, "timeline:\n  - call_tool: {name: fail_tool}\n  - advance: 1m\n")).Runs[0]
	if res.Pass || !strings.Contains(res.Failure, "the tool said no") || !strings.Contains(res.Failure, "timeline step 0 (call_tool)") || len(res.Steps) != 0 {
		t.Fatalf("result = %+v", res)
	}
}

// A locked reload that fails is journaled before the gateway exits: the report waits for
// the exit, so it carries the status and is infrastructure.
func TestConfigPatch_FailedReloadWaitsForTheExit(t *testing.T) {
	sup := newFakeSup(t)
	sup.onPatch = func(in *fakeInstance, _ map[string]any) (bool, error) {
		in.j.Append(labjournal.Entry{Kind: "config.reload", Payload: json.RawMessage(`{"ok":false,"error":"model_list[1]: protocol \"openai\" is not allowed"}`)})
		go func() {
			time.Sleep(150 * time.Millisecond) // the gateway takes a moment to exit after the entry
			sup.exit(in, 3, supervisor.ExitLockInvariant)
		}()
		return true, nil
	}
	rep := runOne(t, sup, scenarioOf(t, "timeline:\n  - config.patch: {model_list: []}\n"))
	res := rep.Runs[0]
	if rep.ExitCode() != ExitInfra || !strings.Contains(res.Infra, "exit 3") || !strings.Contains(res.Infra, "lock_invariant") || !strings.Contains(res.Infra, "protocol \"openai\"") {
		t.Fatalf("result = %+v (exit %d)", res, rep.ExitCode())
	}
}

func journalPayloads(t *testing.T, in *fakeInstance, kinds string) []string {
	t.Helper()
	es, err := in.j.Query(context.Background(), labjournal.Query{Kinds: []string{kinds}})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, string(e.Payload))
	}
	return out
}
