package agent

import "testing"

// Allow-path entries are anchored at the start of the path, so an entry naming
// a directory no longer matches a path that merely contains it.
func TestCompilePatternsAnchorsAtTheStart(t *testing.T) {
	patterns := compilePatterns([]string{"/home/me/data", `.*\.csv$`, "^/already/anchored"})

	matches := func(path string) bool {
		for _, re := range patterns {
			if re.MatchString(path) {
				return true
			}
		}
		return false
	}

	for path, want := range map[string]bool{
		"/home/me/data/report.txt":         true,  // the directory it names
		"/tmp/evil/home/me/data/x":         false, // merely contains it
		"/etc/cron.d/../../home/me/data/x": false,
		"/any/where/prices.csv":            true, // ".*" entries keep their meaning
		"/already/anchored/file":           true,
	} {
		if got := matches(path); got != want {
			t.Errorf("%s: matched = %v, want %v", path, got, want)
		}
	}
}
