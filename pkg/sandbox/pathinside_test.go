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
