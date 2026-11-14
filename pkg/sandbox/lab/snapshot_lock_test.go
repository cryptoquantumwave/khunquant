package lab

import (
	"context"
	"encoding/json"
	"errors"
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

// Shared is the section a writer outside the groups takes (the dev-MCP fixture and
// simulator tools). It and an exclusive call keep each other out, two shared sections
// overlap, and a wait that ends with its context never runs the section (PR6-2).
func TestShared_ExcludesAnExclusiveCall(t *testing.T) {
	l := &Lab{}
	started, release := make(chan struct{}, 1), make(chan struct{})
	hold := func(ctx context.Context, _ json.RawMessage) (any, error) {
		started <- struct{}{}
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := l.AddGroup(Group{Name: "hold", Ops: []Op{{Name: "hold", Run: hold, Exclusive: true}}}); err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	exclusive := make(chan error, 1)
	callHold := func() {
		_, err := l.Call(bg, "hold", "hold", nil)
		exclusive <- err
	}

	// An exclusive call in flight: a shared section waits, and gives up with its context.
	go callHold()
	<-started
	short, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	ran := false
	err := l.Shared(short, func() error { ran = true; return nil })
	cancel()
	if err == nil || ran || !strings.Contains(err.Error(), "waiting for other lab calls") {
		t.Fatalf("Shared during an exclusive call: err = %v, ran = %v; want it to wait and not run", err, ran)
	}
	waited := make(chan error, 1)
	go func() { waited <- l.Shared(bg, func() error { return nil }) }()
	select {
	case err := <-waited:
		t.Fatalf("Shared returned (%v) while an exclusive call held the lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	if err := <-exclusive; err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("Shared after the exclusive call: %v", err)
	}

	// A shared section in flight: another one runs beside it, an exclusive call waits.
	in, out := make(chan struct{}), make(chan struct{})
	section := make(chan error, 1)
	go func() { section <- l.Shared(bg, func() error { close(in); <-out; return nil }) }()
	<-in
	if err := l.Shared(bg, func() error { return nil }); err != nil {
		t.Fatalf("a second shared section: %v", err)
	}
	short, cancel = context.WithTimeout(bg, 50*time.Millisecond)
	_, err = l.Call(short, "hold", "hold", nil)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "waiting for other lab calls") {
		t.Fatalf("an exclusive call during a shared section: err = %v, want it to wait", err)
	}
	close(out)
	if err := <-section; err != nil {
		t.Fatal(err)
	}

	// Neither refused wait left the lock held, and Shared returns what f returns.
	go callHold()
	<-started
	release <- struct{}{}
	if err := <-exclusive; err != nil {
		t.Fatalf("an exclusive call after the shared section: %v", err)
	}
	failed := errors.New("f failed")
	if err := l.Shared(bg, func() error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("Shared = %v, want the error of f", err)
	}
}
