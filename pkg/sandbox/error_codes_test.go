package sandbox

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// equitySimFiles are the files that answer Settrade (and Webull) with errors of their own.
var equitySimFiles = []string{"sim_settrade.go"}

// No real broker code is invented: every code the equity simulators send is an upper-case,
// dash-separated literal that starts with SBX-, which no venue uses.
func TestSandboxErrorCodesPrefixed(t *testing.T) {
	code := regexp.MustCompile(`"([A-Z][A-Z0-9]*(?:-[A-Z0-9]+)+)"`)
	for _, f := range equitySimFiles {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		found := code.FindAllStringSubmatch(string(src), -1)
		if len(found) < 8 {
			t.Fatalf("%s: %d error codes found, the scan is not reading what it should", f, len(found))
		}
		for _, m := range found {
			if !strings.HasPrefix(m[1], "SBX-") {
				t.Errorf("%s: code %q does not start with SBX-", f, m[1])
			}
		}
	}
}
