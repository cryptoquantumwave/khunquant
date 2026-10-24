package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// resetDefaultCalendar leaves the process-wide calendar with no holidays once the
// test is over, so no other test sees what this one loaded.
func resetDefaultCalendar(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = calendar.LoadHolidays(t.TempDir()) })
}

func writeHolidayFiles(t *testing.T, workspace string) {
	t.Helper()
	dir := filepath.Join(workspace, "calendar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"set_holidays.json": `{"market":"set","years":[2026],"holidays":["2026-10-13"]}`,
		"us_holidays.json":  `{"market":"us","years":[2026],"holidays":["2026-11-26"]}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPerception_ClockSourceIsBuiltAndLoadsTheWorkspaceHolidays(t *testing.T) {
	resetDefaultCalendar(t)
	f := newAssemblyFixtureNoBuild(t, liveWithPerception())
	writeHolidayFiles(t, f.workspace)
	f.build(f.initial)

	rt := f.services.Perception
	if rt == nil || rt.clock == nil {
		t.Fatal("perception or its clock source was not built")
	}
	if got := rt.clock.Name(); got != "clock" {
		t.Fatalf("clock source name = %q", got)
	}
	// The workspace's holiday file reached the calendar the clock source reads.
	if calendar.IsOpen(calendar.SET, time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("the workspace's SET holiday was not loaded")
	}
	waitUntil(t, "the clock source's first computation", func() bool { return rt.clock.Stats().Wakes >= 1 })

	code, body, err := getStatus(f.url("/api/proactive/stats"), testMgmtToken)
	if err != nil || code != http.StatusOK {
		t.Fatalf("stats: %d %v", code, err)
	}
	var raw struct {
		Perception struct {
			Clock   map[string]any `json:"clock"`
			Sources map[string]any `json:"sources"`
		} `json:"perception"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Perception.Clock == nil {
		t.Fatalf("no clock stats in %s", body)
	}
	assertNumbersOnly(t, "perception.clock", raw.Perception.Clock) // counts only
	if _, ok := raw.Perception.Sources["clock"]; !ok {
		t.Errorf("the clock source is not in the source health map: %v", raw.Perception.Sources)
	}

	// A reload loads the files again: an edit takes effect.
	if err := os.WriteFile(filepath.Join(f.workspace, "calendar", "set_holidays.json"),
		[]byte(`{"market":"set","years":[2026],"holidays":["2026-10-14"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.reload(liveWithPerception()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !calendar.IsOpen(calendar.SET, time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)) ||
		calendar.IsOpen(calendar.SET, time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("a reload did not re-read the holiday file")
	}
}

func TestPerception_DefaultConfigDoesNotTouchTheCalendar(t *testing.T) {
	resetDefaultCalendar(t)
	// Arm the calendar with a marker holiday, then start a default gateway whose
	// workspace has a different file: if the build loaded files, the marker is gone.
	marker := t.TempDir()
	writeHolidayFiles(t, marker)
	if err := calendar.LoadHolidays(marker); err != nil {
		t.Fatal(err)
	}

	f := newAssemblyFixtureNoBuild(t, nil)
	dir := filepath.Join(f.workspace, "calendar")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "set_holidays.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.build(f.initial)

	if f.services.Perception != nil {
		t.Fatal("perception built on a default config")
	}
	if calendar.IsOpen(calendar.SET, time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("a default gateway re-loaded the holiday files")
	}
	if n := goroutinesIn("sources.(*ClockSource)"); n != 0 {
		t.Fatalf("%d clock goroutines on a default config", n)
	}
}

func TestProfileZone_FollowsTheProfile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	shared := newSharedResources()
	t.Cleanup(func() { _ = shared.Close() })
	d := assemblyDeps{Cfg: cfg, Shared: shared}

	zone := profileZone(d)
	if zone == nil {
		t.Fatal("no zone function although the database opens")
	}
	if got := zone(); got == nil || got.String() != intent.DefaultTimezone {
		t.Fatalf("zone with no stored profile = %v, want %s", got, intent.DefaultTimezone)
	}

	db, err := proactiveDB(d)
	if err != nil {
		t.Fatal(err)
	}
	store, err := intent.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"America/New_York", "Europe/London"} {
		p := intent.DefaultProfile()
		p.Timezone = name
		if _, err := store.PutProfile(context.Background(), p, "test", proactive.TrustUserAuthored); err != nil {
			t.Fatalf("put profile %s: %v", name, err)
		}
		if got := zone(); got == nil || got.String() != name {
			t.Fatalf("after the profile moved to %s the zone is %v", name, got)
		}
	}
}
