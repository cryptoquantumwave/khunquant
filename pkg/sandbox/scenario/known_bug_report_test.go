package scenario

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The nightly known-bug job runs active and known_bug files together and has to tell a
// regression from an expected red (TRUST-7): an active scenario that fails fails the run,
// a known_bug one that fails does not, and one that unexpectedly passes is a warning only.
func TestReport_KnownBugClassification(t *testing.T) {
	failed := &Result{Scenario: "s", Run: 1}
	passed := &Result{Scenario: "s", Run: 1, Pass: true}
	infra := &Result{Scenario: "s", Run: 1, Infra: "the instance died"}
	mk := func(status Status, res *Result) *Report {
		rep := &Report{Scenario: "s", Status: status, Runs: []*Result{res}}
		if status == StatusKnownBug {
			rep.KnownBug = "H8"
		}
		if res.Pass {
			rep.Passed, rep.Pass = 1, true
		}
		return rep
	}
	for _, c := range []struct {
		name      string
		rep       *Report
		mayFail   bool // --known-bugs-may-fail
		wantGate  int
		wantFixed bool
	}{
		{"active fail, nightly", mk(StatusActive, failed), true, ExitFailed, false},
		{"status unset (active) fail, nightly", mk("", failed), true, ExitFailed, false},
		{"active fail, plain run", mk(StatusActive, failed), false, ExitFailed, false},
		{"known_bug fail, nightly", mk(StatusKnownBug, failed), true, 0, false},
		{"known_bug fail, plain run", mk(StatusKnownBug, failed), false, ExitFailed, false},
		{"known_bug pass, nightly: a warning, not a failure", mk(StatusKnownBug, passed), true, 0, true},
		{"active pass", mk(StatusActive, passed), true, 0, false},
		{"known_bug infrastructure failure still fails", mk(StatusKnownBug, infra), true, ExitInfra, false},
		{"known_bug skipped is not a fix", &Report{Scenario: "s", Status: StatusKnownBug, KnownBug: "H8", Skipped: "known_bug H8"}, true, 0, false},
	} {
		if got := c.rep.GateExitCode(c.mayFail); got != c.wantGate {
			t.Errorf("%s: GateExitCode = %d, want %d", c.name, got, c.wantGate)
		}
		if got := c.rep.KnownBugFixed(); got != c.wantFixed {
			t.Errorf("%s: KnownBugFixed = %v, want %v", c.name, got, c.wantFixed)
		}
	}
}

// The heading names the status, so an expected red and a regression read differently; an
// active scenario's heading is unchanged.
func TestReport_HeadingShowsKnownBug(t *testing.T) {
	known := &Report{Scenario: "orders-x", Status: StatusKnownBug, KnownBug: "S116A-1", Runs: []*Result{{Scenario: "orders-x", Run: 1}}}
	if md := known.Markdown(); !strings.HasPrefix(md, "# Scenario orders-x [known_bug S116A-1]: FAIL\n") {
		t.Errorf("known_bug heading:\n%s", md)
	}
	active := &Report{Scenario: "orders-x", Status: StatusActive, Runs: []*Result{{Scenario: "orders-x", Run: 1}}}
	if md := active.Markdown(); !strings.HasPrefix(md, "# Scenario orders-x: FAIL\n") {
		t.Errorf("active heading:\n%s", md)
	}
}

// A known_bug file's report carries the defect id and its status, from the file.
func TestRunner_ReportCarriesKnownBug(t *testing.T) {
	sup := newFakeSup(t)
	f := scenarioOf(t, "status: known_bug\nknown_bug: H8\n")
	rep, err := (&Runner{Sup: sup, KnownBugs: true, SettleCap: 300 * time.Millisecond}).Run(context.Background(), f)
	if err != nil || rep.Status != StatusKnownBug || rep.KnownBug != "H8" || rep.Tag() != "[known_bug H8]" {
		t.Fatalf("report = %+v (%v)", rep, err)
	}
	if !strings.HasPrefix(rep.Markdown(), "# Scenario "+rep.Scenario+" [known_bug H8]: ") {
		t.Errorf("heading: %q", rep.Markdown())
	}
}
