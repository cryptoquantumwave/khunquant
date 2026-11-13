//go:build !windows

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A restore over a running instance takes it down only once the fixtures listing cannot
// refuse the start (AS2-6). It used to be checked by Up, after the old instance and its home
// were gone: a refused restore left no instance at all. The memory floor is still read
// after the Down: the old instance holds memory the new one takes over.
func TestRestore_RefusedLeavesTheRunningInstance(t *testing.T) {
	s, _ := snapSupervisor(t, 1)
	ctx := context.Background()
	a := setup(t, s, "a", UpOptions{})
	good, err := s.Snapshot(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	put(t, a.Home, "workspace/after.txt", "written after the snapshot")
	untouched := func(what string) {
		t.Helper()
		got, err := s.Get("a")
		if err != nil || got.State != StateRunning || got.PID != a.PID {
			t.Fatalf("%s: a = %+v, %v; want the instance that was running, pid %d", what, got, err, a.PID)
		}
		if b, err := os.ReadFile(filepath.Join(a.Home, "workspace", "after.txt")); err != nil || string(b) != "written after the snapshot" {
			t.Errorf("%s: the running instance's home was replaced: %q, %v", what, b, err)
		}
	}

	// A snapshot that holds no fixtures: Up has nothing to list.
	if err := os.RemoveAll(filepath.Join(a.Home, "workspace", "sandbox")); err != nil {
		t.Fatal(err)
	}
	bare, err := s.Snapshot(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Restore(ctx, bare.Manifest.ID, ""); err == nil || !strings.Contains(err.Error(), "fixtures") {
		t.Fatalf("restore of a snapshot with no fixtures: err = %v, want the fixtures listing to refuse it", err)
	}
	untouched("no fixtures in the snapshot")

	// Control: a restore nothing refuses does replace the running instance.
	info, err := s.Restore(ctx, good.Manifest.ID, "")
	if err != nil || info.Name != "a" || info.State != StateRunning {
		t.Fatalf("restore = %+v, %v", info, err)
	}
	if info.PID == a.PID {
		t.Errorf("the restored instance is the old process, pid %d", info.PID)
	}
	if _, err := os.Stat(filepath.Join(info.Home, "workspace", "after.txt")); err == nil {
		t.Error("the restored home still holds a file written after the snapshot")
	}
}

// A snapshot_delete waits for a restore that has checked the snapshot (PR6-5). In between
// it used to remove the files the restore was about to copy, after the old instance was
// already down. The port is chosen after the check and before the copy, so the delete is
// sent from there: it must still be waiting when the copy starts, and goes through once the
// restore is done.
func TestDeleteSnapshot_WaitsForARestore(t *testing.T) {
	s, _ := snapSupervisor(t, 2)
	ctx := context.Background()
	setup(t, s, "a", UpOptions{})
	si, err := s.Snapshot(ctx, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	id := si.Manifest.ID
	var once sync.Once
	deleted, done := make(chan error, 1), make(chan struct{})
	s.pickPort = func() (int, error) {
		once.Do(func() {
			go func() {
				deleted <- s.DeleteSnapshot(id)
				close(done)
			}()
			select {
			case <-done:
				t.Error("the delete went through between the restore's check and its copy")
			case <-time.After(300 * time.Millisecond):
			}
		})
		return freePort()
	}

	b, err := s.Restore(ctx, id, "b")
	if err != nil {
		t.Fatalf("restore with a delete of its snapshot waiting: %v", err)
	}
	files := tree(t, b.Home)
	if files["lab/boot"] != "3\n" || files["lab/clock.json"] != simAt || files["workspace/notes.json"] == "" {
		t.Errorf("restored files: boot %q clock %q notes %q", files["lab/boot"], files["lab/clock.json"], files["workspace/notes.json"])
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Errorf("the delete after the restore: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the delete did not go through after the restore")
	}
	if _, err := os.Stat(si.Dir); !os.IsNotExist(err) {
		t.Errorf("the deleted snapshot is still in the store: %v", err)
	}
}

// A restore moves the old home's path only in text files up to maxRewriteBytes (AS2-5): it
// reads such a file whole, in the supervisor, and the instance wrote it. A larger one is
// copied byte for byte.
func TestRestoreFile_RewritesOnlyUpToTheLimit(t *testing.T) {
	const oldHome, newHome = "/t/kq-sbx-a", "/t/kq-sbx-b"
	small := `{"workspace":"` + oldHome + `/workspace"}`
	big := strings.Repeat(small+"\n", 4)
	limit := maxRewriteBytes
	maxRewriteBytes = int64(len(small)) // a file of exactly the limit is still rewritten
	t.Cleanup(func() { maxRewriteBytes = limit })

	dir := t.TempDir()
	restored := func(name, body string) string {
		t.Helper()
		src, dst := filepath.Join(dir, "src-"+name), filepath.Join(dir, "dst-"+name)
		if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := restoreFile(src, dst, []string{oldHome}, newHome); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got, want := restored("small.json", small), `{"workspace":"`+newHome+`/workspace"}`; got != want {
		t.Errorf("a text file at the limit = %s, want %s", got, want)
	}
	if got := restored("big.jsonl", big); got != big {
		t.Errorf("a text file over the limit was not copied as it is:\n%s", got)
	}
	if got := restored("small.bin", small); got != small { // control: never a file that is not text
		t.Errorf("a file that is not text was rewritten: %s", got)
	}
	if limit != 16<<20 {
		t.Errorf("the limit is %d bytes, want 16 MiB", limit)
	}
}
