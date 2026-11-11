package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, tc := range []struct {
		name, root, p string
		want          bool
	}{
		{"root itself", root, root, true},
		{"child", root, filepath.Join(root, "workspace"), true},
		{"not yet existing child", root, filepath.Join(root, "a", "b", "c"), true},
		{"dot-dot escape", root, filepath.Join(root, "..", "x"), false},
		{"sibling with shared prefix", root, root + "-other", false},
		{"symlink out of root", root, filepath.Join(root, "escape", "x"), false},
		{"unrelated", root, outside, false},
		{"empty root", "", root, false},
		{"empty path", root, "", false},
	} {
		if got := PathInside(tc.root, tc.p); got != tc.want {
			t.Errorf("%s: PathInside(%q, %q) = %v, want %v", tc.name, tc.root, tc.p, got, tc.want)
		}
	}
}

// PathInsideFold ignores letter case, so a path the lock keeps out cannot be reached by
// spelling it differently on a case-insensitive volume (195-1).
func TestPathInsideFold(t *testing.T) {
	root := t.TempDir()
	lab := filepath.Join(root, "lab")
	if err := os.MkdirAll(lab, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, root, p string
		want          bool
	}{
		{"same", lab, lab, true},
		{"child", lab, filepath.Join(lab, "world"), true},
		{"other case", lab, filepath.Join(root, "Lab"), true},
		{"other case child", lab, filepath.Join(root, "LAB", "budget.json"), true},
		{"shared prefix", lab, filepath.Join(root, "laboratory"), false},
		{"sibling", lab, filepath.Join(root, "workspace"), false},
		{"dot-dot escape", lab, filepath.Join(lab, "..", "workspace"), false},
		{"empty root", "", lab, false},
		{"empty path", lab, "", false},
	} {
		if got := PathInsideFold(tc.root, tc.p); got != tc.want {
			t.Errorf("%s: PathInsideFold(%q, %q) = %v, want %v", tc.name, tc.root, tc.p, got, tc.want)
		}
	}
	// PathInside itself stays exact.
	if PathInside(lab, filepath.Join(root, "Lab")) && !caseInsensitive(t, root) {
		t.Error("PathInside folded case on a case-sensitive volume")
	}
}

// caseInsensitive reports whether dir's volume ignores letter case.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "caseprobe")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASEPROBE"))
	return err == nil
}
