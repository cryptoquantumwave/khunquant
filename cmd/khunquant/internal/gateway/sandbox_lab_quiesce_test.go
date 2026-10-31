package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labjournal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// acceptanceQuiescers are the names the plan requires on a locked instance.
var acceptanceQuiescers = []string{
	"engine", "perception", "outbox", "bus", "channels", "agent", "cognition", "cron", "notify", "orderfeed",
	"worldmodel", "journal", "approval", "mandate_sync", "feedback", "policy", "replay", "reflection", "hindsight",
	"intents", "simchat", "exchange_sim", "gateway.reload", "labjournal",
}

func labIdle(t *testing.T, f *lockedFixture) map[string]bool {
	t.Helper()
	code, out := labPost(t, f, "inspect", `{"op":"idle"}`)
	if code != http.StatusOK {
		t.Fatalf("inspect idle = %d %v", code, out)
	}
	got := map[string]bool{}
	for _, q := range out["quiescers"].([]any) {
		m := q.(map[string]any)
		got[m["name"].(string)] = m["idle"].(bool)
	}
	return got
}

// startAgent runs the fixture's agent loop, which the assembly fixtures leave stopped.
func startAgent(t *testing.T, f *lockedFixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.al.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// `inspect idle` lists every acceptance name, all idle on a quiet instance, and
// `inspect settle` answers settled:true.
func TestLabSettle_IdleListsEveryComponent(t *testing.T) {
	f := newLockedFixture(t, liveWithPerception())
	idle := labIdle(t, f)
	for _, name := range acceptanceQuiescers {
		isIdle, ok := idle[name]
		if !ok {
			t.Errorf("no quiescer %q (have %v)", name, idle)
		} else if !isIdle {
			t.Errorf("quiescer %q is busy on a quiet instance", name)
		}
	}
	code, out := labPost(t, f, "inspect", `{"op":"settle"}`)
	if code != http.StatusOK || out["settled"] != true {
		t.Fatalf("settle on an idle instance = %d %v", code, out)
	}
}

// An advance waits for the reply of an agent turn, which is still on its way through the
// bus and a slow channel when the first step ends; the reply is in the journal's outbox
// the moment the advance returns, with no wait of the test's own.
func TestLabSettle_AdvanceWaitsForAgentTurn(t *testing.T) {
	t.Setenv(lab.EnvClockStart, "2026-03-10T09:00:00Z")
	t.Setenv(lab.EnvClockMode, "manual")
	f := newLockedFixture(t)
	startAgent(t, f)
	if code, out := labPost(t, f, "chat", `{"op":"set_state","state":"slow","delay":"1200ms"}`); code != http.StatusOK {
		t.Fatalf("set_state = %d %v", code, out)
	}
	if code, out := labPost(t, f, "chat", `{"op":"send","as":"stranger","text":"hello there"}`); code != http.StatusOK {
		t.Fatalf("send = %d %v", code, out)
	}
	start := time.Now()
	code, out := labPost(t, f, "clock", `{"op":"advance","by":"2m"}`)
	if code != http.StatusOK || out["done"] != true {
		t.Fatalf("advance = %d %v", code, out)
	}
	if wall := time.Since(start); wall < 900*time.Millisecond {
		t.Fatalf("the advance returned after %v: it did not wait for the slow reply", wall)
	}
	if msgs := chatOutbox(t, f.url("/api/sandbox/lab/chat"), "dm:stranger"); len(msgs) != 1 {
		t.Fatalf("the reply is not in the outbox when the advance returns: %v", msgs)
	}
	if err := f.lab.Journal.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	outs, err := f.lab.Journal.Query(context.Background(), labjournal.Query{Kinds: []string{"chat.out"}})
	if err != nil || len(outs) != 1 {
		t.Fatalf("chat.out entries = %d, %v; want 1", len(outs), err)
	}
}

// A settle that cannot pass says who is busy and never reports a quiet instance, so an
// assertion that nothing was sent cannot be made too early; once the reply is out the
// settle passes and the reply is there exactly once.
func TestLabSettle_NegativeAssertionIsReal(t *testing.T) {
	f := newLockedFixture(t)
	startAgent(t, f)
	labPost(t, f, "chat", `{"op":"set_state","state":"slow","delay":"1500ms"}`)
	labPost(t, f, "chat", `{"op":"send","as":"stranger","text":"hello there"}`)

	_, out := labPost(t, f, "inspect", `{"op":"settle","wall_cap":"100ms"}`)
	busy, _ := out["busy"].([]any)
	if out["settled"] != false || len(busy) == 0 {
		t.Fatalf("settle with a reply still on its way = %v", out)
	}
	if got := len(chatOutbox(t, f.url("/api/sandbox/lab/chat"), "dm:stranger")); got != 0 {
		t.Fatalf("the reply was already out (%d): the scenario proves nothing", got)
	}
	names := map[string]bool{}
	for _, b := range busy {
		names[b.(string)] = true
	}
	if !names["channels"] && !names["simchat"] && !names["agent"] && !names["bus"] {
		t.Fatalf("busy = %v, want the path the reply is on", busy)
	}

	if _, out := labPost(t, f, "inspect", `{"op":"settle"}`); out["settled"] != true {
		t.Fatalf("settle after the reply = %v", out)
	}
	if got := len(chatOutbox(t, f.url("/api/sandbox/lab/chat"), "dm:stranger")); got != 1 {
		t.Fatalf("outbox has %d replies after a settled instance, want 1", got)
	}
}

// A reload in progress keeps the instance busy under its own name, whatever the
// (retired) generation says.
func TestLabSettle_ReloadIsBusy(t *testing.T) {
	f := newLockedFixture(t)
	f.shared.reloading.Add(1)
	if labIdle(t, f)["gateway.reload"] {
		t.Fatal("gateway.reload is idle during a reload")
	}
	f.shared.reloading.Add(-1)
	f.shared.retireLabGeneration()
	for _, q := range f.lab.Quiesce.Snapshot() {
		if !q.Idle {
			t.Errorf("%s is busy between generations", q.Name)
		}
	}
	if code, _, _ := postJSON(f.url("/api/sandbox/lab/inspect"), lockTestToken, `{"op":"settle"}`); code != http.StatusServiceUnavailable {
		t.Errorf("inspect settle between generations = %d, want 503 like the other generation-scoped ops", code)
	}
}

// Unlocked, nothing is registered: no lab, no registry, no inspect route or tool.
func TestQuiesce_NotRegisteredWhenUnlocked(t *testing.T) {
	f := newAssemblyFixture(t, enableAllRoutes)
	if f.shared.lab != nil {
		t.Fatal("an unlocked gateway built a lab")
	}
	for _, op := range []string{`{"op":"idle"}`, `{"op":"settle"}`} {
		if code, body, err := postJSON(f.url("/api/sandbox/lab/inspect"), testSandboxToken, op); err != nil || code != http.StatusNotFound {
			t.Fatalf("POST inspect %s = %d (%v) %s, want 404", op, code, err, body)
		}
	}
	if mcpTools(t, f, testDevMCPToken)["sandbox_inspect"] {
		t.Fatal("an unlocked gateway lists sandbox_inspect")
	}
}
