package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
)

// ErrClockRunning: a snapshot needs a stopped sim clock, so sim time is the same in every file.
var ErrClockRunning = errors.New("snapshot: clock must be in manual mode")

// snapshotSettleCap bounds the settle of a take; the whole call has CallTimeout. A variable for tests.
var snapshotSettleCap = 10 * time.Second

var sqliteMagic = []byte("SQLite format 3\x00")

// SnapshotsDir is where `take` writes, and the only place it writes: <home>/lab/snapshots.
// The supervisor moves a snapshot out of the home, so a `down` does not delete it.
func (l *Lab) SnapshotsDir() string { return filepath.Join(l.Home, "lab", "snapshots") }

// SnapshotGroup is the group "snapshot" (take). Only a locked instance registers it.
func (l *Lab) SnapshotGroup() Group {
	return Group{
		Name:        "snapshot",
		Description: "Freeze a settled instance with a stopped clock into <home>/lab/snapshots/<id> (the supervisor moves it out). Not an agent step: use sandbox_instance op=snapshot.",
		Ops: []Op{{
			Name: "take", Description: "settle, save the world, back up every SQLite file, copy the rest, write the manifest with sha256 sums",
			Args:     json.RawMessage(`{"id":{"type":"string","description":"letters, digits and '-'"},"label":{"type":"string"}}`),
			Required: []string{"id"}, Run: l.snapshotTake,
			Exclusive: true, // no other lab call between the settle and the last copy; the take never calls Lab.Call
		}},
	}
}

func (l *Lab) snapshotTake(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		Op, ID, Label string
	}
	if err := strictArgs(args, &a); err != nil {
		return nil, err
	}
	if err := labsnap.ValidID(a.ID); err != nil {
		return nil, Errorf(CodeBadArgs, "%v", err)
	}
	if len(a.Label) > 64 || strings.ContainsFunc(a.Label, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return nil, Errorf(CodeBadArgs, "label must be at most 64 printable characters")
	}
	// Money is never rolled back: a restore would bring back an older budget.json.
	if m := os.Getenv(sandbox.EnvLLM); m == "live" || m == "record" {
		return nil, Errorf(CodeFailed, "snapshot is refused for a %s instance: spent budget must not be rolled back", m)
	}
	if l.Clock == nil {
		return nil, Errorf(CodeUnavailable, "this lab has no sim clock")
	}
	if l.Clock.State().Mode != simclock.ModeManual {
		return nil, Errorf(CodeFailed, "%v", ErrClockRunning)
	}
	if _, err := l.SettleWait(ctx, snapshotSettleCap); err != nil {
		return nil, Errorf(CodeFailed, "snapshot: the instance is not settled: %v", err)
	}
	m, dir, err := l.writeSnapshot(ctx, a.ID, a.Label)
	if err != nil {
		_ = os.RemoveAll(dir)
		var le *Error
		if errors.As(err, &le) {
			return nil, err
		}
		return nil, Errorf(CodeFailed, "%v", err)
	}
	return map[string]any{"id": m.ID, "sim_time": m.SimTime, "journal_seq": m.JournalSeq, "bytes": m.Size(), "files": len(m.Files)}, nil
}

// snapshotFile is one file to take.
type snapshotFile struct {
	rel, abs string
	size     int64
	sqlite   bool
}

func (l *Lab) writeSnapshot(ctx context.Context, id, label string) (labsnap.Manifest, string, error) {
	dir := filepath.Join(l.SnapshotsDir(), id)
	// The instance's name is the one the lock was given (sandbox_lock.go reads the same
	// variable): a restore with no name starts the snapshot under it.
	m := labsnap.Manifest{ID: id, Label: label, Instance: os.Getenv("KQ_SANDBOX_INSTANCE"), Home: l.Home, BinaryVersion: config.FormatVersion(),
		LLM: os.Getenv(sandbox.EnvLLM), Seed: os.Getenv(sandbox.EnvSeed), Parts: []string{}}
	if _, err := os.Lstat(dir); err == nil {
		return m, "", fmt.Errorf("snapshot %q already exists", id) // not ours to remove
	}
	// The world files first, so the copies below hold the state of this instant.
	l.pmu.Lock()
	for _, e := range l.world {
		m.Parts = append(m.Parts, e.p.StateKey())
	}
	l.pmu.Unlock()
	for _, k := range m.Parts {
		if err := l.Persist(k); err != nil {
			return m, dir, err
		}
	}
	saveClock(l.ClockPath(), l.Clock) // clock.json is otherwise written on manual steps only
	m.SimTime, m.CreatedWall = l.Clock.State().Now, time.Now()
	if l.Journal != nil {
		if err := l.Journal.Flush(ctx); err != nil {
			return m, dir, err
		}
	}
	files, total, err := l.snapshotFiles()
	if err != nil {
		return m, "", err
	}
	if total > labsnap.MaxHomeBytes { // before anything is written
		return m, "", Errorf(CodeFailed, "too large: the home holds %d MiB, the limit is %d MiB", total>>20, labsnap.MaxHomeBytes>>20)
	}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return m, dir, err
		}
		dst := filepath.Join(dir, labsnap.HomeDir, filepath.FromSlash(f.rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return m, dir, err
		}
		if err := takeFile(ctx, f, dst); err != nil {
			return m, dir, fmt.Errorf("snapshot of %s: %w", f.rel, err)
		}
		sum, n, err := labsnap.SumFile(dst)
		if err != nil {
			return m, dir, err
		}
		m.Files = append(m.Files, labsnap.FileEntry{Path: f.rel, SHA256: sum, Bytes: n})
	}
	// The journal's last seq is read from the copy, so it is what a restore will find even if
	// an entry was appended while the other files were copied.
	m.JournalSeq = copiedJournalSeq(filepath.Join(dir, labsnap.HomeDir, "lab", "journal.db"))
	b, _ := json.MarshalIndent(m, "", "  ")
	return m, dir, fileutil.WriteFileAtomic(filepath.Join(dir, labsnap.ManifestName), b, 0o600)
}

// takeFile backs up a SQLite file through a fresh read-only connection (no component handle
// is needed, and a store's journal mode is not changed), or copies any other file.
func takeFile(ctx context.Context, f snapshotFile, dst string) error {
	if f.sqlite {
		db, err := sqlitex.Open(f.abs, sqlitex.ReadOnly())
		if err != nil {
			return err
		}
		defer db.Close()
		return sqlitex.Backup(ctx, db, dst)
	}
	in, err := os.Open(f.abs)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copiedJournalSeq is the last seq of the journal copy at p (0 when there is none).
func copiedJournalSeq(p string) int64 {
	if _, err := os.Stat(p); err != nil {
		return 0
	}
	db, err := sqlitex.Open(p, sqlitex.ReadOnly())
	if err != nil {
		return 0
	}
	var seq int64
	_ = db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM lab_journal`).Scan(&seq)
	_ = db.Close()
	for _, side := range []string{"-wal", "-shm"} { // a read-only open of a WAL-flagged copy may create these
		_ = os.Remove(p + side)
	}
	return seq
}

// snapshotFiles lists what a snapshot takes: every regular file in the home, found by walk
// (links and special files are skipped, so a link planted in the workspace cannot pull in
// anything outside the home), without what skipSnapshot names.
func (l *Lab) snapshotFiles() (files []snapshotFile, total int64, err error) {
	err = filepath.WalkDir(l.Home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(l.Home, p)
		rel = filepath.ToSlash(rel)
		if skipSnapshot(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, snapshotFile{rel: rel, abs: p, size: info.Size(), sqlite: isSQLite(p)})
		total += info.Size()
		return nil
	})
	return files, total, err
}

// isSQLite says whether p is a SQLite database. A database the gateway holds open in WAL
// mode has a -wal or -shm file beside it, and is recognised by that alone: opening and
// closing any descriptor of it would drop this process's POSIX locks on the file, which
// SQLite warns about. The take is settled and holds off every other lab call, so any
// other file is not in a transaction and a header read is harmless.
func isSQLite(p string) bool {
	for _, side := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(p + side); err == nil {
			return true
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(sqliteMagic))
	n, _ := io.ReadFull(f, head)
	return n == len(head) && bytes.Equal(head, sqliteMagic)
}

// skipSnapshot says whether the home-relative path rel stays out of a snapshot: the log, the
// pid file and the instance marker (a restore writes its own), the instance's tmp/ (the
// TMPDIR of a locked instance), earlier snapshots, the crash
// marker and its reports, a restore's working directory, and SQLite's side files (the
// backup is self-contained) and temporary files.
func skipSnapshot(rel string) bool {
	base := path.Base(rel)
	switch {
	case rel == ".":
		return false
	case rel == "gateway.log", rel == ".khunquant.pid", rel == ".kq-sandbox":
		return true
	case rel == "tmp", rel == "lab/snapshots", rel == "lab/crash.json", strings.HasPrefix(rel, "lab/restore"):
		return true
	case path.Dir(rel) == "lab" && strings.HasPrefix(base, "crash-") && strings.HasSuffix(base, ".json"):
		return true
	}
	for _, s := range []string{"-wal", "-shm", "-journal", ".tmp", ".part"} {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}
