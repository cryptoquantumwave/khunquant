package feedback

import (
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Trades in the 24 hours after an alert, on alert days, against the trades of days with
// no alert; contract symbols (":") are counted apart as the leverage proxy.
func TestHarmMetric_PostAlertTrades(t *testing.T) {
	f := newFixture(t)
	f.fb.fillsSince = time.Time{}
	ctx := context.Background()
	f.clock.t = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	d := func(day, hour int) time.Time { return time.Date(2026, 3, day, hour, 0, 0, 0, time.UTC) }

	f.insertDelivered("alert-1", "SOL/USDT", "", d(1, 20))
	fills := []struct {
		id, sym string
		at      time.Time
	}{
		{"before", "BTC/USDT", d(1, 8)},                                   // alert day, before the alert: not after one
		{"after", "SOL/USDT", d(1, 21)},                                   // within 24h after the alert
		{"next-morning", "BTC/USDT:USDT", d(2, 9)},                        // quiet day, still inside the alert's 24h
		{"next-evening", "ETH/USDT", d(2, 21)},                            // quiet day, outside it
		{"quiet", "BTC/USDT", d(5, 10)},                                   // quiet day
		{"today", "BTC/USDT", d(10, 1)},                                   // today is not a complete day
		{"old", "BTC/USDT", time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC)}, // before the 30 days
	}
	for _, fl := range fills {
		f.fb.logFill(ctx, fl.id, fl.sym, fl.at)
	}

	m, err := f.fb.HarmMetric(ctx, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Days) != HarmDays || m.Days[0].Day != "2026-02-08" || m.Days[HarmDays-1].Day != "2026-03-09" {
		t.Fatalf("days = %d from %s to %s, want the 30 complete days before today", len(m.Days), m.Days[0].Day, m.Days[len(m.Days)-1].Day)
	}
	if m.AlertDays != 1 || m.QuietDays != 29 {
		t.Fatalf("alert days %d, quiet days %d, want 1 and 29", m.AlertDays, m.QuietDays)
	}
	if m.PostAlertTrades != 1 || m.PostAlertContractTrades != 0 {
		t.Fatalf("post-alert trades %d (contract %d), want 1 (0): only the trade 1h after the alert", m.PostAlertTrades, m.PostAlertContractTrades)
	}
	if m.QuietTrades != 3 || m.QuietContractTrades != 1 {
		t.Fatalf("quiet-day trades %d (contract %d), want 3 (1)", m.QuietTrades, m.QuietContractTrades)
	}
	if m.PostAlertPerAlertDay != 1 || m.QuietPerQuietDay != 3.0/29 || m.QuietContractPerQuietDay != 1.0/29 {
		t.Fatalf("rates = %v %v %v", m.PostAlertPerAlertDay, m.QuietPerQuietDay, m.QuietContractPerQuietDay)
	}

	// Days before the fills log began are unknown, not empty: they are left out.
	f.fb.fillsSince = d(5, 0)
	m, err = f.fb.HarmMetric(ctx, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Days) != 5 || m.Days[0].Day != "2026-03-05" || m.AlertDays != 0 || m.QuietTrades != 1 {
		t.Fatalf("with a late fills log: %d days from %s, alert days %d, quiet trades %d", len(m.Days), m.Days[0].Day, m.AlertDays, m.QuietTrades)
	}
}

// The pure function: owner-local days, overlapping alerts count a trade once, an empty
// metric has zero rates rather than NaN.
func TestHarm_PureFunction(t *testing.T) {
	bkk := time.FixedZone("ICT", 7*3600)
	first := time.Date(2026, 3, 1, 0, 0, 0, 0, bkk)
	last := time.Date(2026, 3, 4, 0, 0, 0, 0, bkk)
	at := func(day, hour int) time.Time { return time.Date(2026, 3, day, hour, 0, 0, 0, bkk) }
	alerts := []time.Time{at(2, 23), at(2, 1), at(2, 2)} // unsorted, overlapping windows
	fills := []fillAt{{at(2, 5), false}, {at(3, 0), true}, {at(3, 1), false}, {at(3, 3), false}}
	m := harm(alerts, fills, first, last, bkk)
	if len(m.Days) != 3 || m.AlertDays != 1 || m.QuietDays != 2 {
		t.Fatalf("days %d, alert days %d, quiet days %d", len(m.Days), m.AlertDays, m.QuietDays)
	}
	if m.Days[1].Alerts != 3 || m.Days[1].PostAlertTrades != 1 || m.PostAlertTrades != 1 {
		t.Fatalf("alert day = %+v, total %d", m.Days[1], m.PostAlertTrades)
	}
	// Day 3 has no alert: its three trades are the quiet baseline, whatever shadowed them.
	if m.QuietTrades != 3 || m.QuietContractTrades != 1 {
		t.Fatalf("quiet trades %d (contract %d), want 3 (1)", m.QuietTrades, m.QuietContractTrades)
	}
	if e := harm(nil, nil, first, last, bkk); e.PostAlertPerAlertDay != 0 || e.QuietPerQuietDay != 0 || e.QuietDays != 3 {
		t.Fatalf("empty metric = %+v", e)
	}
}

// Nothing that decides may read the harm metric: pkg/attention, pkg/triggers and
// pkg/policy never import pkg/feedback (the metric's only way in). The dashboard reads it.
func TestHarmMetricIsNotADecisionInput(t *testing.T) {
	root := repoRoot(t)
	checked := 0
	for _, dir := range []string{"pkg/attention", "pkg/triggers", "pkg/policy"} {
		before := checked
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			checked++
			if importsFeedback(t, src) {
				t.Errorf("%s imports pkg/feedback: the harm metric and the hindsight rows must not reach a decision", path)
			}
			return nil
		})
		if err != nil || checked == before {
			t.Fatalf("%s: no Go files read (%v): the guard is looking in the wrong place", dir, err)
		}
	}
	if checked < 20 {
		t.Fatalf("checked %d files: the guard is not reading the packages", checked)
	}
}

func importsFeedback(t *testing.T, src []byte) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/pkg/feedback") {
			return true
		}
	}
	return false
}

// The guard itself fails on an import and passes without one.
func TestHarmGuardDetectsTheImport(t *testing.T) {
	bad := "package a\nimport (\n\t\"fmt\"\n\tfb \"github.com/cryptoquantumwave/khunquant/pkg/feedback\"\n)\nvar _ = fb.Component\n"
	if !importsFeedback(t, []byte(bad)) {
		t.Fatal("the guard missed an import of pkg/feedback")
	}
	if importsFeedback(t, []byte("package a\nimport \"github.com/cryptoquantumwave/khunquant/pkg/feedbackish\"\n")) {
		t.Fatal("the guard flagged a different package")
	}
}

// repoRoot walks up from the package directory to go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test's directory")
		}
		dir = parent
	}
}
