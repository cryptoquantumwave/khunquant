package supervisor

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/labsnap"
)

var (
	// ErrClockRunning: a snapshot needs the sim clock stopped (manual mode).
	ErrClockRunning = errors.New("snapshot: clock must be in manual mode")
	// ErrVersionMismatch: the snapshot was taken by another build of the binary.
	ErrVersionMismatch = errors.New("snapshot: taken by a different binary")
	// ErrMoneyAtRisk: a live or record instance is never snapshotted or restored, because a
	// restored older budget would erase spend already made.
	ErrMoneyAtRisk = errors.New("snapshot: refused for a live or record instance: spent budget must not be rolled back")
	// ErrTooManySnapshots: the store holds as many snapshots as the cap allows.
	ErrTooManySnapshots = errors.New("snapshot: the store is full")
)

// SnapshotInfo is a snapshot in the store.
type SnapshotInfo struct {
	Manifest labsnap.Manifest `json:"manifest"`
	Dir      string           `json:"dir"`
	Bytes    int64            `json:"bytes"`
}

// snapshotStoreName is the store's directory under the temp dir. It shares the prefix of an
// instance home, so no instance may be called "snapshots" (instanceHome): its home would be
// the store, and its down would delete every snapshot.
const snapshotStoreName = homePrefix + "snapshots"

// snapshotRoot is the store, <TmpDir>/kq-sbx-snapshots: outside every instance home, so a
// `down` does not delete a snapshot.
func (s *Supervisor) snapshotRoot() (string, error) {
	base, err := filepath.Abs(s.cfg.TmpDir)
	return filepath.Join(base, snapshotStoreName), err
}

func moneyAtRisk(llm string) bool { return llm == "live" || llm == "record" }

func labCall(ctx context.Context, in *instance, group, body string, out any) error {
	in.mu.Lock()
	c := in.client
	in.mu.Unlock()
	if c == nil {
		return fmt.Errorf("sandbox: instance %q has no client yet", in.name)
	}
	raw, err := c.Call(ctx, group, json.RawMessage(body))
	if err == nil && out != nil {
		err = json.Unmarshal(raw, out)
	}
	return err
}

// Snapshot freezes the instance into the store: the clock must be manual (ErrClockRunning),
// and the instance settles and writes the snapshot under its own <home>/lab/snapshots, which
// is moved to <TmpDir>/kq-sbx-snapshots/<id> and verified. An empty name is the only instance.
func (s *Supervisor) Snapshot(ctx context.Context, name, label string) (SnapshotInfo, error) {
	in, err := s.lookup(name)
	if err != nil {
		return SnapshotInfo{}, err
	}
	in.opMu.Lock()
	defer in.opMu.Unlock()
	if moneyAtRisk(in.env[envLLM]) {
		return SnapshotInfo{}, ErrMoneyAtRisk
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if list, err := s.ListSnapshots(); err != nil {
		return SnapshotInfo{}, err
	} else if len(list) >= s.cfg.MaxSnapshots {
		return SnapshotInfo{}, fmt.Errorf("%w (%d); remove one with sandbox_instance op=snapshot_delete {snapshot}", ErrTooManySnapshots, s.cfg.MaxSnapshots)
	}
	if _, err := s.Client(in.name); err != nil {
		return SnapshotInfo{}, err
	}
	var clock struct {
		Now  time.Time `json:"now"`
		Mode string    `json:"mode"`
	}
	if err := labCall(ctx, in, "clock", `{"op":"get"}`, &clock); err != nil {
		return SnapshotInfo{}, fmt.Errorf("snapshot: clock get: %w", err)
	}
	if clock.Mode != "manual" {
		return SnapshotInfo{}, fmt.Errorf("%w (it is %s)", ErrClockRunning, clock.Mode)
	}
	root, err := s.snapshotRoot()
	if err != nil {
		return SnapshotInfo{}, err
	}
	id := nextSnapshotID(root, in.name, clock.Now)
	body, _ := json.Marshal(map[string]string{"op": "take", "id": id, "label": label})
	if err := labCall(ctx, in, "snapshot", string(body), nil); err != nil {
		return SnapshotInfo{}, err
	}
	src, dst := filepath.Join(in.home, "lab", "snapshots", id), filepath.Join(root, id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return SnapshotInfo{}, err
	}
	if err := os.Rename(src, dst); err != nil {
		return SnapshotInfo{}, fmt.Errorf("snapshot: move to the store: %w", err)
	}
	m, err := labsnap.Read(dst)
	if err == nil {
		m, err = labsnap.Verify(dst, m.BinaryVersion) // sums only: the instance is its own binary
	}
	if err != nil {
		_ = os.RemoveAll(dst)
		return SnapshotInfo{}, err
	}
	return SnapshotInfo{Manifest: m, Dir: dst, Bytes: m.Size()}, nil
}

// nextSnapshotID is <name>-<sim time as yyyymmddThhmm>-<n>, with the first n not in use.
func nextSnapshotID(root, name string, simNow time.Time) string {
	prefix := name + "-" + simNow.Format("20060102T1504") + "-"
	for n := 1; ; n++ {
		if _, err := os.Lstat(filepath.Join(root, fmt.Sprint(prefix, n))); err != nil {
			return fmt.Sprint(prefix, n)
		}
	}
}

// ListSnapshots lists the store, oldest first. A directory that is not a snapshot is skipped.
func (s *Supervisor) ListSnapshots() ([]SnapshotInfo, error) {
	root, err := s.snapshotRoot()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []SnapshotInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []SnapshotInfo{}
	for _, e := range ents {
		if m, err := labsnap.Read(filepath.Join(root, e.Name())); err == nil && e.IsDir() {
			out = append(out, SnapshotInfo{Manifest: m, Dir: filepath.Join(root, e.Name()), Bytes: m.Size()})
		}
	}
	slices.SortFunc(out, func(a, b SnapshotInfo) int { return a.Manifest.CreatedWall.Compare(b.Manifest.CreatedWall) })
	return out, nil
}

// DeleteSnapshot removes one snapshot from the store.
func (s *Supervisor) DeleteSnapshot(id string) error {
	if err := labsnap.ValidID(id); err != nil {
		return err
	}
	root, err := s.snapshotRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, id)
	if _, err := labsnap.Read(dir); err != nil {
		return fmt.Errorf("snapshot: no snapshot %q in the store", id)
	}
	s.snapMu.Lock() // not while a Snapshot is counting or moving one in
	defer s.snapMu.Unlock()
	return os.RemoveAll(dir)
}

// restoreSpec is what attempt copies into a fresh home instead of leaving the profile.
type restoreSpec struct {
	dir string
	m   labsnap.Manifest
}

// Restore starts the snapshot as a locked instance called name (empty: the instance it was
// taken from), through Up. The sums, the version and the money rule are checked before any
// home exists; a running instance of that name is taken down first. The clock is not set:
// the snapshot's clock.json wins. Restoring into another name while the first runs is fine.
func (s *Supervisor) Restore(ctx context.Context, snapshotID, name string) (Info, error) {
	root, err := s.snapshotRoot()
	if err != nil {
		return Info{}, err
	}
	if err := labsnap.ValidID(snapshotID); err != nil {
		return Info{}, err
	}
	dir := filepath.Join(root, snapshotID)
	m, err := labsnap.Read(dir)
	if err != nil {
		return Info{}, fmt.Errorf("snapshot: no usable snapshot %q in the store: %w", snapshotID, err)
	}
	if m.BinaryVersion != config.FormatVersion() {
		return Info{}, fmt.Errorf("%w: it was taken by %q, this binary is %q", ErrVersionMismatch, m.BinaryVersion, config.FormatVersion())
	}
	if _, err := labsnap.Verify(dir, config.FormatVersion()); err != nil {
		return Info{}, err
	}
	if moneyAtRisk(m.LLM) || slices.ContainsFunc(m.Files, func(f labsnap.FileEntry) bool { return f.Path == "lab/budget.json" }) {
		return Info{}, ErrMoneyAtRisk
	}
	// Never an empty name: lookup("") is "the only instance", so an unnamed restore of a
	// snapshot that records no instance would take down whatever happens to run.
	if name = cmp.Or(name, m.Instance); name == "" {
		return Info{}, errors.New("snapshot: the snapshot does not record the instance it was taken from; give the name to restore it as")
	}
	accounts, err := readAccounts(filepath.Join(dir, labsnap.HomeDir))
	if err != nil {
		return Info{}, fmt.Errorf("snapshot: %w", err)
	}
	if _, err := s.lookup(name); err == nil {
		if err := s.Down(ctx, name, false); err != nil {
			return Info{}, err
		}
	}
	info, err := s.Up(ctx, UpOptions{
		Name: name, FixturesFrom: filepath.Join(dir, labsnap.HomeDir, "workspace", "sandbox"),
		LLM: m.LLM, Seed: cmp.Or(m.Seed, "0"), Accounts: accounts, restore: &restoreSpec{dir: dir, m: m},
	})
	if err != nil {
		return Info{}, err
	}
	in, err := s.lookup(info.Name)
	if err != nil {
		return Info{}, fmt.Errorf("snapshot: the restored instance is gone: %w", err)
	}
	var clock struct {
		Now time.Time `json:"now"`
	}
	if err := labCall(ctx, in, "clock", `{"op":"get"}`, &clock); err != nil || !clock.Now.Equal(m.SimTime) {
		_ = s.Down(ctx, info.Name, false)
		return Info{}, fmt.Errorf("snapshot: the restored clock reads %v (%v), want %v", clock.Now, err, m.SimTime)
	}
	return info, nil
}

// apply fills the fresh home of an attempt from the snapshot: every file at its path, with
// the old home's path moved to the new one in the text files (config, cron, sessions; a
// SQLite file is never rewritten), then the attempt's port. The marker, the profile's
// secrets file and the placeholder accounts list were written by prepareHome and are
// replaced only if the snapshot holds them.
func (r *restoreSpec) apply(home string, port int) error {
	olds := []string{r.m.Home}
	if res, err := filepath.EvalSymlinks(filepath.Dir(r.m.Home)); err == nil { // /var vs /private/var
		olds = append(olds, filepath.Join(res, filepath.Base(r.m.Home)))
	}
	for _, f := range r.m.Files {
		src := filepath.Join(r.dir, labsnap.HomeDir, filepath.FromSlash(f.Path))
		dst := filepath.Join(home, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := restoreFile(src, dst, olds, home); err != nil {
			return fmt.Errorf("sandbox restore: %s: %w", f.Path, err)
		}
	}
	if _, err := patchConfigFile(home, map[string]any{"gateway": map[string]any{"port": port}}); err != nil {
		return fmt.Errorf("sandbox restore: port: %w", err)
	}
	return nil
}

func restoreFile(src, dst string, olds []string, home string) error {
	switch strings.ToLower(filepath.Ext(src)) {
	case ".json", ".yml", ".yaml", ".jsonl":
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		for _, old := range olds {
			b = labsnap.ReplaceHome(b, old, home)
		}
		return os.WriteFile(dst, b, 0o600)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
