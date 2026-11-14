package lab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
)

// The instance's tmp/ stays out of a snapshot (only <home>/tmp, not a workspace folder of
// that name); a file beside a -shm is a database without being opened (it is unreadable here).
func TestSnapshotTake_SkipsTmpAndNeedsNoOpenForWAL(t *testing.T) {
	l := snapLab(t)
	writeHome(t, l, "tmp/scratch.bin", []byte("x"))
	writeHome(t, l, "workspace/tmp/keep.txt", []byte("kept"))
	if _, err := take(t, l, "t-1"); err != nil {
		t.Fatal(err)
	}
	m, err := labsnap.Verify(filepath.Join(l.SnapshotsDir(), "t-1"), config.FormatVersion())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range m.Files {
		have[f.Path] = true
	}
	if have["tmp/scratch.bin"] || !have["workspace/tmp/keep.txt"] {
		t.Fatalf("files = %v, want no tmp/scratch.bin and workspace/tmp/keep.txt kept", m.Files)
	}

	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable file")
	}
	db := filepath.Join(t.TempDir(), "held.db")
	if err := os.WriteFile(db, append(append([]byte{}, sqliteMagic...), 0), 0o000); err != nil {
		t.Fatal(err)
	}
	if isSQLite(db) {
		t.Fatal("an unreadable file with no side file was called SQLite")
	}
	if err := os.WriteFile(db+"-shm", []byte("shm"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isSQLite(db) {
		t.Error("a file with a -shm beside it was not recognised without opening it")
	}
}

// A take waits for the calls in flight and holds off those that start after it, so the
// snapshot is never taken while another call is half done. The bump op writes two files
// one after the other; every snapshot must hold equal values (it fails without the lock).
func TestSnapshotTake_ExcludesOtherCalls(t *testing.T) {
	l := snapLab(t)
	ws := filepath.Join(l.Home, "workspace")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	counter := 0
	bump := func(ctx context.Context, _ json.RawMessage) (any, error) {
		counter++
		v := []byte("v" + strconv.Itoa(counter))
		if err := os.WriteFile(filepath.Join(ws, "a.txt"), v, 0o600); err != nil {
			return nil, err
		}
		time.Sleep(time.Millisecond) // a take landing here would see a ahead of b
		return nil, os.WriteFile(filepath.Join(ws, "b.txt"), v, 0o600)
	}
	if err := l.AddGroup(Group{Name: "bump", Ops: []Op{{Name: "bump", Run: bump}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Call(context.Background(), "bump", "bump", nil); err != nil {
		t.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = l.Call(context.Background(), "bump", "bump", nil)
			}
		}
	}()
	for i := 0; i < 8; i++ {
		id := "x-" + strconv.Itoa(i)
		if _, err := take(t, l, id); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(l.SnapshotsDir(), id, "home", "workspace")
		a, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
		b, _ := os.ReadFile(filepath.Join(dir, "b.txt"))
		if len(a) == 0 || string(a) != string(b) {
			t.Fatalf("snapshot %s is torn: a=%q b=%q", id, a, b)
		}
		if err := os.RemoveAll(filepath.Join(l.SnapshotsDir(), id)); err != nil { // as the supervisor moves it out: one at a time
			t.Fatal(err)
		}
	}
	close(stop)
	<-done
}

// A take blocked behind a long call ends with its own context instead of waiting for the
// call, and the lock it was waiting for is not left held: the next take works.
func TestSnapshotTake_WaitEndsWithItsContext(t *testing.T) {
	l := snapLab(t)
	hold, started := make(chan struct{}), make(chan struct{})
	slow := func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-hold
		return nil, nil
	}
	if err := l.AddGroup(Group{Name: "slow", Ops: []Op{{Name: "slow", Run: slow}}}); err != nil {
		t.Fatal(err)
	}
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		_, _ = l.Call(context.Background(), "slow", "slow", nil)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := l.Call(ctx, "snapshot", "take", json.RawMessage(`{"op":"take","id":"w-1"}`))
	if err == nil || !strings.Contains(err.Error(), "waiting for other lab calls") {
		t.Fatalf("err = %v, want the wait to end with the context", err)
	}
	close(hold)
	<-slowDone
	if _, err := take(t, l, "w-2"); err != nil {
		t.Fatalf("a take after the long call: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.SnapshotsDir(), "w-1")); err == nil {
		t.Error("the refused take left a directory")
	}
}
