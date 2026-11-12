package lab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

func take(t *testing.T, l *Lab, id string) (map[string]any, error) {
	t.Helper()
	args := json.RawMessage(`{"op":"take","id":"` + id + `"}`)
	res, err := l.Call(context.Background(), "snapshot", "take", args)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func snapLab(t *testing.T) *Lab {
	t.Helper()
	l := manualLab(t)
	if err := l.AddGroup(l.SnapshotGroup()); err != nil {
		t.Fatal(err)
	}
	return l
}

func writeHome(t *testing.T, l *Lab, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(l.Home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A realtime or scaled clock is refused with ErrClockRunning, a manual one passes, and a
// refused take leaves nothing behind.
func TestSnapshotTake_RefusesRunningClock(t *testing.T) {
	l := snapLab(t)
	if err := l.Clock.SetMode("realtime"); err != nil {
		t.Fatal(err)
	}
	_, err := take(t, l, "c-1")
	if err == nil || !strings.Contains(err.Error(), ErrClockRunning.Error()) {
		t.Fatalf("realtime: err = %v, want %v", err, ErrClockRunning)
	}
	if _, err := os.Stat(filepath.Join(l.SnapshotsDir(), "c-1")); err == nil {
		t.Error("a refused take left a directory")
	}
	if err := l.Clock.SetMode("manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := take(t, l, "c-2"); err != nil {
		t.Fatalf("manual: %v", err)
	}
}

// A SQLite file is found by its header, not its name; -wal and -shm files are skipped; a
// text file called .db is copied as it is.
func TestSnapshotTake_FindsSQLiteByHeader(t *testing.T) {
	l := snapLab(t)
	dat := filepath.Join(l.Home, "workspace", "store.dat")
	db, err := sqlitex.Open(dat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE k (v TEXT); INSERT INTO k VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	writeHome(t, l, "workspace/notes.db", []byte("just text"))
	writeHome(t, l, "workspace/other.db-wal", []byte("wal"))
	writeHome(t, l, "workspace/other.db-shm", []byte("shm"))

	if _, err := take(t, l, "h-1"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(l.SnapshotsDir(), "h-1")
	m, err := labsnap.Verify(dir, config.FormatVersion())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range m.Files {
		have[f.Path] = true
	}
	if !have["workspace/store.dat"] || !have["workspace/notes.db"] || have["workspace/other.db-wal"] || have["workspace/other.db-shm"] {
		t.Fatalf("files = %v", m.Files)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "home", "workspace", "notes.db")); string(b) != "just text" {
		t.Errorf("the text .db was not copied as it is: %q", b)
	}
	if !isSQLite(filepath.Join(dir, "home", "workspace", "store.dat")) {
		t.Fatal("store.dat is not a SQLite file in the snapshot")
	}
	cp, err := sqlitex.Open(filepath.Join(dir, "home", "workspace", "store.dat"), sqlitex.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := cp.QueryRow(`SELECT v FROM k`).Scan(&v); err != nil || v != "kept" {
		t.Errorf("backed-up row = %q, %v", v, err)
	}
	_ = cp.Close()
}

// An instance that is not settled is not snapshotted, and the error names the busy component.
func TestSnapshotTake_RefusesUnsettled(t *testing.T) {
	l := snapLab(t)
	old := snapshotSettleCap
	snapshotSettleCap = 30 * time.Millisecond
	t.Cleanup(func() { snapshotSettleCap = old })
	l.Quiesce.Register(quiesce.Func("agent", func() bool { return false }))
	_, err := take(t, l, "u-1")
	if err == nil || !strings.Contains(err.Error(), "not settled") || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("err = %v, want not settled naming agent", err)
	}
}

// A home over the cap fails with "too large" before anything is written; the id cannot
// leave <home>/lab/snapshots; a link in the workspace is not followed.
func TestSnapshotTake_CapIdAndLinks(t *testing.T) {
	l := snapLab(t)
	for _, id := range []string{"../x", "a/b", "", ".", "a.b", strings.Repeat("a", 81)} {
		if _, err := take(t, l, id); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.Home, "workspace", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := take(t, l, "k-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(l.SnapshotsDir(), "k-1", "home", "workspace", "link")); err == nil {
		t.Error("a link was copied into the snapshot")
	}
	if err := os.RemoveAll(filepath.Join(l.SnapshotsDir(), "k-1")); err != nil { // one at a time in the home
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(l.Home, "workspace", "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(labsnap.MaxHomeBytes + 1); err != nil { // sparse
		t.Fatal(err)
	}
	_ = f.Close()
	_, err = take(t, l, "big-1")
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v, want too large", err)
	}
	if _, err := os.Stat(filepath.Join(l.SnapshotsDir(), "big-1")); err == nil {
		t.Error("a too large take left a partial snapshot")
	}
}

// The manifest records the instance the snapshot was taken from, which a restore with no
// name starts it as (215-1). The supervisor's tests fill the field in their fake gateway;
// this is the real take.
func TestSnapshotTake_RecordsTheInstance(t *testing.T) {
	t.Setenv("KQ_SANDBOX_INSTANCE", "alpha")
	l := snapLab(t)
	if _, err := take(t, l, "alpha-1"); err != nil {
		t.Fatal(err)
	}
	m, err := labsnap.Read(filepath.Join(l.SnapshotsDir(), "alpha-1"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Instance != "alpha" {
		t.Errorf("manifest instance = %q, want %q", m.Instance, "alpha")
	}
}

// The home holds one snapshot at a time (AS2-1). The supervisor moves each take to its
// store, which has the cap; the take op is also a tool of the lab, and a direct call used to
// add a copy of the home on every call, in a directory the size limit does not count.
func TestSnapshotTake_OneAtATimeInTheHome(t *testing.T) {
	l := snapLab(t)
	writeHome(t, l, "workspace/notes.txt", []byte("notes"))
	if _, err := take(t, l, "one-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one-2", "one-3"} {
		_, err := take(t, l, id)
		if err == nil || !strings.Contains(err.Error(), "one at a time") {
			t.Fatalf("take %s with one-1 still in the home: err = %v, want a refusal", id, err)
		}
	}
	if ents, _ := os.ReadDir(l.SnapshotsDir()); len(ents) != 1 || ents[0].Name() != "one-1" {
		t.Fatalf("the home holds %d snapshots, want only one-1", len(ents))
	}
	// Control: once it is moved out (what the supervisor does), the next take is accepted.
	if err := os.Rename(filepath.Join(l.SnapshotsDir(), "one-1"), filepath.Join(t.TempDir(), "one-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := take(t, l, "one-2"); err != nil {
		t.Fatalf("take after the first was moved out: %v", err)
	}
}
