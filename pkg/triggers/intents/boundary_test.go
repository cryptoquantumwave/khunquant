package intents

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// TestIntents_ImportBoundary pins what the compiler may import: the ledger, the
// registry, the clock's vocabulary and the cron service's schedule function. It
// reaches no provider (so no order can be placed or changed), no tool, no channel, no
// notify service, no attention governor, no configuration and no perception source or
// pipeline engine. It sits next to pkg/triggers/migrate because it must see both
// pkg/intent and pkg/triggers, which pkg/triggers itself may not.
func TestIntents_ImportBoundary(t *testing.T) {
	perceptiontest.AssertImports(t, perceptiontest.Boundary{
		Root:    "pkg/triggers/intents",
		Allowed: []string{"pkg/cron", "pkg/intent", "pkg/logger", "pkg/perception", "pkg/proactive", "pkg/triggers"},
		Forbidden: []string{
			"pkg/agent", "pkg/tools", "pkg/providers", "pkg/notify", "pkg/channels", "pkg/attention",
			"pkg/config", "pkg/snapshot", "pkg/perception/sources", "pkg/proactive/engine",
			"pkg/dca", "pkg/exchanges",
		},
	})
}

func TestIntents_NoCrossPackageCallsInTransaction(t *testing.T) {
	sqlitextest.AssertNoCrossPackageCallsInTx(t, "pkg/triggers/intents")
}

// TestIntentOrigin_OnlyTheCompilerWritesIt is the dependency OwnerAlertSpec rests on:
// a spec with an "intent:" origin breaks quiet hours, so it must be written only by
// code that writes it for an ACTIVE intent. This lists every non-test source file in
// the module that builds or names such an origin and fails on a new one: whoever adds
// a writer must prove the same thing and add the file here.
func TestIntentOrigin_OnlyTheCompilerWritesIt(t *testing.T) {
	root := moduleRoot(t)
	allowed := map[string]string{
		"pkg/triggers/owner_alert.go":     "defines the origins and the OwnerAlertSpec rule",
		"pkg/triggers/intents/compile.go": "writes the origin, for an intent the compiler is given",
		"pkg/triggers/intents/compiler.go": "writes the spec, only for an ACTIVE or paused intent read from the ledger, " +
			"and removes the spec of any other",
	}
	needles := []string{"IntentOrigin(", "IntentOrderOrigin(", "IntentOriginPrefix", "IntentOrderOriginPrefix", `"intent:"`, `"intent-order:"`, "`intent:`"}
	var found []string
	for _, dir := range []string{"pkg", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for _, n := range needles {
				if strings.Contains(string(b), n) {
					rel, _ := filepath.Rel(root, path)
					found = append(found, filepath.ToSlash(rel))
					break
				}
			}
			return nil
		})
	}
	sort.Strings(found)
	for _, f := range found {
		if _, ok := allowed[f]; !ok {
			t.Errorf("%s names an intent origin; only %v may. Prove it writes the origin only for an active intent and list it in this test", f, keys(allowed))
		}
	}
	for f := range allowed {
		if !contains(found, f) {
			t.Errorf("%s is listed as a writer but no longer names an intent origin; remove it from the list", f)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
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
			t.Fatal("go.mod not found above the test's working directory")
		}
		dir = parent
	}
}
