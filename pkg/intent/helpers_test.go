package intent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// repoRoot walks up from the test's working directory to the directory that
// holds go.mod.
func repoRoot(t testing.TB) string {
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

func newTestStore(t testing.TB) *Store {
	t.Helper()
	s, err := NewStore(context.Background(), sqlitextest.Open(t))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

// needZone skips a test on a host without the zoneinfo database.
func needZone(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("host has no zoneinfo for %s: %v", name, err)
	}
	return loc
}
