package devmcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// TestPersistFixturesToDiskRejectsTraversal is the regression guard for path
// traversal in fixture writes. Both the gateway HTTP handler and MCP tools call
// persistFixturesToDisk, which validates the venue name via sandbox.ValidateVenueName
// before any filepath.Join, so traversal attempts are rejected consistently.
func TestPersistFixturesToDiskRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	fixturesDir := filepath.Join(root, "sandbox")
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries := []sandbox.FixtureEntry{{
		Method:     "GET",
		PathPrefix: "/api/v3/account",
		Response:   sandbox.Response{Status: 200, Body: []byte(`{"ok":true}`)},
	}}

	for _, venue := range []string{
		"../escaped",
		"../../escaped",
		"okx/../../escaped",
	} {
		t.Run(venue, func(t *testing.T) {
			err := persistFixturesToDisk(fixturesDir, venue, entries)
			if err != nil {
				t.Logf("rejected: %v", err)
				return
			}

			written := filepath.Join(fixturesDir, venue, "fixtures.json")
			clean := filepath.Clean(written)
			rel, relErr := filepath.Rel(fixturesDir, clean)
			escaped := relErr != nil || rel == ".." || filepath.IsAbs(rel) ||
				len(rel) >= 3 && rel[:3] == "../"

			if escaped {
				if _, statErr := os.Stat(clean); statErr == nil {
					t.Errorf("venue %q wrote outside the fixtures directory: %s", venue, clean)
				} else {
					t.Errorf("venue %q was accepted and resolved outside the fixtures "+
						"directory (%s)", venue, clean)
				}
			}
		})
	}
}

// TestPersistFixturesToDiskAcceptsNormalVenue is the control: a legitimate venue
// name must still round-trip to <fixturesDir>/<venue>/fixtures.json.
func TestPersistFixturesToDiskAcceptsNormalVenue(t *testing.T) {
	fixturesDir := t.TempDir()
	entries := []sandbox.FixtureEntry{{
		Method:     "GET",
		PathPrefix: "/api/v3/account",
		Response:   sandbox.Response{Status: 200, Body: []byte(`{"ok":true}`)},
	}}

	if err := persistFixturesToDisk(fixturesDir, "binance", entries); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixturesDir, "binance", "fixtures.json")); err != nil {
		t.Fatalf("expected fixtures.json for venue binance: %v", err)
	}
}
