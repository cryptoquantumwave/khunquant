//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
)

// The store is trusted only as this user's own private directory (AS2-3): a restore copies
// what it holds into a home as the developer. In a temp directory shared with other users,
// one that somebody else made first, or opened to others, or a link, is refused by every
// operation.
func TestSnapshotStore_MustBePrivateToThisUser(t *testing.T) {
	s, base := snapSupervisor(t, 2)
	setup(t, s, "a", UpOptions{})
	si, err := s.Snapshot(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "kq-sbx-snapshots")
	refused := func(what string) {
		t.Helper()
		if _, err := s.ListSnapshots(); !errors.Is(err, ErrStoreNotPrivate) {
			t.Errorf("%s: list err = %v, want %v", what, err, ErrStoreNotPrivate)
		}
		if _, err := s.Snapshot(context.Background(), "a", ""); !errors.Is(err, ErrStoreNotPrivate) {
			t.Errorf("%s: snapshot err = %v, want %v", what, err, ErrStoreNotPrivate)
		}
		if _, err := s.Restore(context.Background(), si.Manifest.ID, "b"); !errors.Is(err, ErrStoreNotPrivate) {
			t.Errorf("%s: restore err = %v, want %v", what, err, ErrStoreNotPrivate)
		}
		if err := s.DeleteSnapshot(si.Manifest.ID); !errors.Is(err, ErrStoreNotPrivate) {
			t.Errorf("%s: delete err = %v, want %v", what, err, ErrStoreNotPrivate)
		}
		if _, err := os.Stat(filepath.Join(base, "kq-sbx-b")); err == nil {
			t.Errorf("%s: a refused restore made a home", what)
		}
	}

	if err := os.Chmod(store, 0o755); err != nil { // readable by others; 0777 would be writable
		t.Fatal(err)
	}
	refused("a store open to others")
	if err := os.Chmod(store, 0o700); err != nil {
		t.Fatal(err)
	}

	// A link where the store should be, to a directory that is private.
	moved := filepath.Join(base, "elsewhere")
	if err := os.Rename(store, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, store); err != nil {
		t.Fatal(err)
	}
	refused("a link as the store")
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, store); err != nil {
		t.Fatal(err)
	}

	// Control: the private store works again.
	if list, err := s.ListSnapshots(); err != nil || len(list) != 1 {
		t.Fatalf("list on the private store = %d, %v", len(list), err)
	}
	if info, err := s.Restore(context.Background(), si.Manifest.ID, "b"); err != nil || info.Name != "b" {
		t.Fatalf("restore from the private store = %+v, %v", info, err)
	}
}

// apply writes as the developer, so it checks each path itself (AS2-3): a manifest entry
// that leaves the home, or a link in place of a file, is refused, and nothing is written
// outside the home. Restore's Verify refuses both too; this is the write's own guard, for a
// store that changed between the check and the copy.
func TestRestoreApply_NeverWritesOutsideTheHome(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "store", "a-1")
	home := filepath.Join(base, "homes", "kq-sbx-b")
	for _, d := range []string{filepath.Join(dir, labsnap.HomeDir), home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The file a "../escape.txt" entry names, where apply would look for it.
	if err := os.WriteFile(filepath.Join(dir, "escape.txt"), []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := &restoreSpec{dir: dir, m: labsnap.Manifest{ID: "a-1", Home: "/old/home", Files: []labsnap.FileEntry{{Path: "../escape.txt"}}}}
	if err := escape.apply(home, 1); err == nil {
		t.Error("a manifest path that leaves the home was applied")
	}
	if _, err := os.Stat(filepath.Join(base, "homes", "escape.txt")); err == nil {
		t.Error("apply wrote outside the home")
	}

	// A link in place of a listed file: the developer's file must not be copied in.
	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("the-developers-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, labsnap.HomeDir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	link := &restoreSpec{dir: dir, m: labsnap.Manifest{ID: "a-1", Home: "/old/home", Files: []labsnap.FileEntry{{Path: "notes.txt"}}}}
	if err := link.apply(home, 1); err == nil {
		t.Error("a link in the snapshot was applied")
	}
	if b, err := os.ReadFile(filepath.Join(home, "notes.txt")); err == nil {
		t.Errorf("the link's target was copied into the home: %q", b)
	}
}
