package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTZDataIsEmbedded fails when this binary stops importing time/tzdata, which
// is what lets a host without a zoneinfo database load a profile's time zone. It
// reads the package's own non-test source files from the working directory (a Go
// test runs in the package directory), so it needs no path outside the package.
func TestTZDataIsEmbedded(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var parsed int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		parsed++
		for _, imp := range f.Imports {
			if imp.Path.Value == `"time/tzdata"` && imp.Name != nil && imp.Name.Name == "_" {
				return
			}
		}
	}
	if parsed == 0 {
		t.Fatal("found no source files to inspect")
	}
	t.Fatal(`no file of this package has a blank import of "time/tzdata"; a host without a zoneinfo ` +
		"database could not load a profile time zone and quiet hours would fall back to UTC")
}

// TestLoadLocationWithoutHostZoneinfoDir loads Asia/Bangkok with $ZONEINFO
// pointing at a directory that does not exist.
//
// What this proves: a zone loads in a binary that links time/tzdata when the
// directory $ZONEINFO names is absent (LoadLocation then goes on to the system
// directories and, last, the embedded copy).
// What it does not prove: that the embedded copy was the one used. The standard
// library also searches the system zoneinfo directories, so on a machine that
// has them (every developer machine and CI runner) the host's copy answers.
// TestTZDataIsEmbedded is the guard against the import being removed.
func TestLoadLocationWithoutHostZoneinfoDir(t *testing.T) {
	t.Setenv("ZONEINFO", filepath.Join(t.TempDir(), "does-not-exist"))

	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatalf("LoadLocation(Asia/Bangkok): %v", err)
	}
	_, offset := time.Date(2026, time.January, 1, 12, 0, 0, 0, loc).Zone()
	if offset != 7*60*60 {
		t.Errorf("Asia/Bangkok offset = %ds, want 25200", offset)
	}
}
