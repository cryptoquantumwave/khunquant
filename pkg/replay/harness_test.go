package replay

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
)

// The replay of every scenario is the same bytes however often it runs: --repeat holds
// on the real pipeline, not only on a stub.
func TestRun_SameEventsSameReport(t *testing.T) {
	for _, name := range scenarioNames(t) {
		t.Run(name, func(t *testing.T) {
			in := loadScenario(t, name)
			if _, err := Repeat(3, func() (*Report, error) { return Run(context.Background(), in) }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Repeat catches a run that differs, and says where.
func TestRepeat_DetectsANondeterministicRun(t *testing.T) {
	in := loadScenario(t, "oscillating_price")
	calls := 0
	_, err := Repeat(3, func() (*Report, error) {
		calls++
		rep, err := Run(context.Background(), in)
		if err == nil && calls == 2 {
			rep.Totals.WouldSend++ // the injected nondeterminism
		}
		return rep, err
	})
	var nd *ErrNondeterministic
	if !errors.As(err, &nd) || nd.Run != 2 || !strings.Contains(nd.Detail, "would_send") {
		t.Fatalf("err = %v, want run 2 to differ in would_send", err)
	}
	if calls != 2 {
		t.Errorf("%d runs, want it to stop at the first difference", calls)
	}
}

// A policy without the trigger the labels expect misses the critical alert: critical
// recall is below 100% and the report says so.
func TestRun_CriticalRecallBelowOneIsReported(t *testing.T) {
	in := loadScenario(t, "dn_margin_squeeze")
	in.Policy.Specs = nil
	rep, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CriticalRecallOK() || rep.Labels.CriticalRecall != 0 || rep.Labels.CriticalLabels != 2 {
		t.Fatalf("labels = %+v", rep.Labels)
	}
	if len(rep.Sends) != 0 {
		t.Errorf("sends with no trigger: %+v", rep.Sends)
	}
}

// A policy change shows in the report: a Critical that is repeated every half hour by
// default is said once when the repeat interval is three hours, and none is missed.
func TestRun_APolicyChangeChangesWhatIsSent(t *testing.T) {
	in := loadScenario(t, "dn_margin_squeeze")
	base, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	att := in.Policy.attention()
	att.CriticalRepeatMinutes = 180
	in.Policy.Attention = &att
	calm, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if base.Totals.WouldSend != 5 || calm.Totals.WouldSend != 3 || !calm.CriticalRecallOK() {
		t.Fatalf("repeat 180 min sent %d (was %d), critical ok %v", calm.Totals.WouldSend, base.Totals.WouldSend, calm.CriticalRecallOK())
	}
}

// Messages are grouped as the outbox groups them: a notice waits out the group window, a
// protective Critical sends it at once with everything already waiting (one message),
// and with no window every decision is a message of its own.
func TestRun_GroupsMessagesByTheWindow(t *testing.T) {
	in := loadScenario(t, "flash_crash")
	rep, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sends) != 3 {
		t.Fatalf("%d sends", len(rep.Sends))
	}
	notice, critical, warning := rep.Sends[0], rep.Sends[1], rep.Sends[2]
	if notice.Message != critical.Message || !notice.At.Equal(critical.At) || critical.Message == warning.Message {
		t.Errorf("the notice and the Critical should go out together, the warning later: %+v", rep.Sends)
	}
	if want := at(12); !critical.At.Equal(want) {
		t.Errorf("the Critical went out at %v, want %v: it is never held for the window", critical.At, want)
	}
	if lat := warning.At.Sub(at(24)); lat < 0 || lat > time.Minute+time.Second {
		t.Errorf("the warning went out %v after its decision, want within the 60 s window", lat)
	}

	att := in.Policy.attention()
	att.GroupWindowSeconds = 0
	in.Policy.Attention = &att
	solo, err := Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if solo.Sends[0].Message == solo.Sends[1].Message {
		t.Errorf("with no window every send is its own message: %+v", solo.Sends)
	}
}

func TestScore_PrecisionRecallAndLatency(t *testing.T) {
	d := time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)
	sends := []Send{
		{At: d.Add(2 * time.Minute), DedupKey: "a:1", Subject: "X"},  // inside the first label
		{At: d.Add(30 * time.Minute), DedupKey: "a:1", Subject: "X"}, // outside every window: wrong
		{At: d.Add(3 * time.Minute), DedupKey: "b:1", Subject: "Y"},  // inside a should-not-fire window: wrong
	}
	labels := []Label{
		{Match: "a:", From: d, To: d.Add(10 * time.Minute), ShouldFire: true},
		{Match: "Y", From: d, To: d.Add(10 * time.Minute), ShouldFire: false},
		{Match: "c:", From: d, To: d.Add(10 * time.Minute), ShouldFire: true, Critical: true}, // never sent
	}
	lr := score(labels, sends)
	if lr.Precision != 0.333 || lr.Recall != 0.5 || lr.CriticalRecall != 0 || lr.WrongSends != 2 {
		t.Errorf("%+v", lr)
	}
	if lr.Latency.Count != 1 || lr.Latency.Max != 120 {
		t.Errorf("latency = %+v", lr.Latency)
	}
	if lr.Results[1].OK || !lr.Results[0].OK || lr.Results[2].OK {
		t.Errorf("results = %+v", lr.Results)
	}
}

func TestLoad_FailsLoudlyOnNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadEvents(dir); !errors.Is(err, ErrNoEvents) {
		t.Errorf("empty directory: %v", err)
	}
	if _, err := LoadEvents(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Error("a missing file loaded")
	}
	if _, err := Run(context.Background(), Input{}); !errors.Is(err, ErrNoEvents) {
		t.Errorf("Run with no events: %v", err)
	}
	// A directory of day files loads in name order, whole.
	in := loadScenario(t, "oscillating_price")
	if len(in.Lines) != 200 {
		t.Fatalf("%d lines", len(in.Lines))
	}
	if _, err := LoadPolicy(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("a missing policy loaded")
	}
	if p, err := LoadPolicy(CurrentPolicy); err != nil || !p.BuiltinPortfolio {
		t.Errorf("current policy = %+v, %v", p, err)
	}
}

// The clock only moves with the events and only forwards.
func TestFakeClock_NeverGoesBackwards(t *testing.T) {
	c := &fakeClock{now: t0}
	c.advance(t0.Add(time.Hour))
	c.advance(t0)
	if !c.Now().Equal(t0.Add(time.Hour)) {
		t.Errorf("now = %v", c.Now())
	}
	if c.After(time.Second) != nil {
		t.Error("After can fire")
	}
}

// pkg/replay cannot reach anything that places an order, calls a tool, messages the
// owner or talks to a model.
func TestReplay_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root: "pkg/replay",
		Allowed: []string{
			"pkg/attention", "pkg/config", "pkg/deltaneutral/planhealth", "pkg/events", "pkg/intent", "pkg/logger",
			"pkg/perception", "pkg/perception/calendar", "pkg/proactive", "pkg/proactive/engine", "pkg/replay", "pkg/sqlitex", "pkg/triggers", "pkg/worldmodel",
		},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/approval", "pkg/control",
		},
	})
}
