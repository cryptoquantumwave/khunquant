package calendar

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// workspaceWith writes the given files under <tmp>/calendar and returns the
// workspace path.
func workspaceWith(t *testing.T, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "calendar"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, "calendar", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

const goodSET = `{"market":"set","years":[2026],"holidays":["2026-10-13"]}`
const goodUS = `{"market":"us","years":[2026],"holidays":["2026-11-26"]}`

func TestLoadHolidays_MissingFile(t *testing.T) {
	buf := captureLogs(t)
	ws := workspaceWith(t, map[string]string{"us_holidays.json": goodUS})
	c := New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	logs := buf.String()
	if n := strings.Count(logs, "Holiday file is missing"); n != 1 {
		t.Fatalf("missing-file WARN logged %d times, want 1 (only SET is missing)\n%s", n, logs)
	}
	for _, want := range []string{"workspace/calendar/set_holidays.json", "Copy"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the WARN does not say how to get the file (%q missing):\n%s", want, logs)
		}
	}
	// SET: no holidays, so Tue 13 Oct is an ordinary day. US still has its list.
	if !c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("a market with no holiday file must count weekdays as open")
	}
	if c.IsOpen(US, utc(t, "2026-11-26T16:00:00Z")) {
		t.Error("the US list was lost because the SET file was missing")
	}
	if strings.Contains(logs, "does not cover") {
		t.Error("a market with no list must not also warn about coverage")
	}
}

func TestLoadHolidays_NoCalendarDirAtAll(t *testing.T) {
	buf := captureLogs(t)
	c := New()
	if err := c.LoadHolidays(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "Holiday file is missing"); n != 2 {
		t.Errorf("want one WARN per market (2), got %d", n)
	}
}

func TestLoadHolidays_EmptyWorkspaceIsACallerBug(t *testing.T) {
	if err := New().LoadHolidays(""); err == nil {
		t.Fatal("an empty workspace path must be an error")
	}
}

func TestLoadHolidays_HostileFilesFailClosed(t *testing.T) {
	deep := strings.Repeat("[", 30000)
	bad := map[string]string{
		"empty file":                "",
		"not json":                  "not json at all",
		"json null":                 "null",
		"a bare list (spec shape)":  `["2026-10-13"]`,
		"truncated":                 `{"years":[2026],"holidays":["2026-10-13"`,
		"deeply nested":             deep,
		"wrong type for holidays":   `{"years":[2026],"holidays":"2026-10-13"}`,
		"wrong type for a date":     `{"years":[2026],"holidays":[20261013]}`,
		"wrong type for years":      `{"years":["2026"],"holidays":["2026-10-13"]}`,
		"years missing":             `{"holidays":["2026-10-13"]}`,
		"years empty":               `{"years":[],"holidays":["2026-10-13"]}`,
		"holidays missing":          `{"years":[2026]}`,
		"misspelt holidays":         `{"years":[2026],"holidys":["2026-10-13"]}`,
		"unknown field":             `{"years":[2026],"holidays":[],"extra":1}`,
		"trailing data":             `{"years":[2026],"holidays":[]} {}`,
		"unparsable date":           `{"years":[2026],"holidays":["2026-13-40"]}`,
		"loose date format":         `{"years":[2026],"holidays":["2026-1-5"]}`,
		"date with time":            `{"years":[2026],"holidays":["2026-10-13T00:00:00Z"]}`,
		"empty date":                `{"years":[2026],"holidays":[""]}`,
		"duplicate date":            `{"years":[2026],"holidays":["2026-10-13","2026-10-13"]}`,
		"date outside years":        `{"years":[2026],"holidays":["2027-01-01"]}`,
		"year out of range (low)":   `{"years":[1066],"holidays":[]}`,
		"year out of range (high)":  `{"years":[99999],"holidays":[]}`,
		"duplicate year":            `{"years":[2026,2026],"holidays":[]}`,
		"this is the US file":       `{"market":"us","years":[2026],"holidays":["2026-10-13"]}`,
		"market of the wrong shape": `{"market":5,"years":[2026],"holidays":[]}`,
		"too many years":            `{"years":[` + years(25) + `],"holidays":[]}`,
		"too many entries":          `{"years":[2026],"holidays":[` + strings.Repeat(`"2026-01-01",`, 600) + `"2026-01-01"]}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			buf := captureLogs(t)
			ws := workspaceWith(t, map[string]string{"set_holidays.json": body, "us_holidays.json": goodUS})
			c := New()
			if err := c.LoadHolidays(ws); err != nil {
				t.Fatalf("a bad file must not be an error: %v", err)
			}
			if !strings.Contains(buf.String(), "Holiday file is unusable") {
				t.Errorf("no WARN for the bad file:\n%s", buf.String())
			}
			// Nothing of a bad file is applied, not even its good prefix.
			if !c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
				t.Error("a bad file applied a holiday")
			}
			// The other market is unaffected.
			if c.IsOpen(US, utc(t, "2026-11-26T16:00:00Z")) {
				t.Error("a bad SET file cleared the US list")
			}
		})
	}
}

func years(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strconv.Itoa(2000 + i)
	}
	return strings.Join(parts, ",")
}

func TestLoadHolidays_GoodFileLoads(t *testing.T) {
	buf := captureLogs(t)
	ws := workspaceWith(t, map[string]string{"set_holidays.json": goodSET, "us_holidays.json": goodUS})
	c := New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "Holiday file") {
		t.Errorf("a good pair of files logged a warning:\n%s", buf.String())
	}
	if c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("the listed SET holiday is open")
	}
	if !c.IsOpen(SET, utc(t, "2026-10-14T04:00:00Z")) {
		t.Error("the day after the holiday is closed")
	}
}

func TestLoadHolidays_FileSizeCap(t *testing.T) {
	buf := captureLogs(t)
	// Valid JSON that would be a good file if it were small: padding goes in "note".
	pad := strings.Repeat("x", MaxHolidayFileBytes)
	big := `{"years":[2026],"note":"` + pad + `","holidays":["2026-10-13"]}`
	if len(big) <= MaxHolidayFileBytes {
		t.Fatal("test file is not over the cap")
	}
	ws := workspaceWith(t, map[string]string{"set_holidays.json": big, "us_holidays.json": goodUS})
	c := New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "larger than") {
		t.Errorf("no size WARN:\n%s", buf.String())
	}
	if !c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("an oversized file was applied")
	}

	// Exactly at the cap is still read.
	ok := `{"years":[2026],"note":"`
	tail := `","holidays":["2026-10-13"]}`
	ok += strings.Repeat("x", MaxHolidayFileBytes-len(ok)-len(tail)) + tail
	if len(ok) != MaxHolidayFileBytes {
		t.Fatalf("test file is %d bytes, want exactly the cap", len(ok))
	}
	ws = workspaceWith(t, map[string]string{"set_holidays.json": ok, "us_holidays.json": goodUS})
	c = New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("a file exactly at the cap was refused")
	}
}

func TestLoadHolidays_NotARegularFile(t *testing.T) {
	buf := captureLogs(t)
	ws := workspaceWith(t, map[string]string{"us_holidays.json": goodUS})
	if err := os.Mkdir(filepath.Join(ws, "calendar", "set_holidays.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not a regular file") {
		t.Errorf("a directory at the file's path was not refused:\n%s", buf.String())
	}
}

func TestLoadHolidays_ReloadReplacesAndClears(t *testing.T) {
	captureLogs(t)
	ws := workspaceWith(t, map[string]string{"set_holidays.json": goodSET, "us_holidays.json": goodUS})
	c := New()
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Fatal("holiday not applied")
	}
	// The user breaks the file; the next load must not keep the old list.
	if err := os.WriteFile(filepath.Join(ws, "calendar", "set_holidays.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if !c.IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("a reload with a broken file kept the stale list")
	}
}

func TestPackageLevelFunctionsUseDefault(t *testing.T) {
	captureLogs(t)
	ws := workspaceWith(t, map[string]string{"set_holidays.json": goodSET, "us_holidays.json": goodUS})
	t.Cleanup(func() { Default.tables.Store(nil) })
	if err := LoadHolidays(ws); err != nil {
		t.Fatal(err)
	}
	if IsOpen(SET, utc(t, "2026-10-13T04:00:00Z")) {
		t.Error("package-level IsOpen ignored the loaded holiday")
	}
	if len(Sessions(SET, utc(t, "2026-10-14T04:00:00Z"))) != 2 {
		t.Error("package-level Sessions wrong")
	}
	if _, phase := NextBoundary(SET, utc(t, "2026-10-12T09:30:00Z")); phase != "open" {
		t.Error("package-level NextBoundary wrong")
	}
}

func FuzzParseHolidays(f *testing.F) {
	f.Add([]byte(goodSET))
	f.Add([]byte(`{"years":[2026],"holidays":["2026-02-30"]}`))
	f.Add([]byte(strings.Repeat("[", 1000)))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseHolidays(SET, data) // must never panic
	})
}
