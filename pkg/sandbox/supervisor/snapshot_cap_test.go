//go:build !windows

package supervisor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// The store holds at most MaxSnapshots snapshots: a take beyond that is refused with an
// error that names the delete op and leaves nothing behind, and deleting one makes room.
func TestSnapshot_StoreIsBounded(t *testing.T) {
	s, _ := snapSupervisor(t, 1)
	s.cfg.MaxSnapshots = 2
	setup(t, s, "a", UpOptions{})
	ctx := context.Background()
	var first SnapshotInfo
	for i := 0; i < 2; i++ {
		si, err := s.Snapshot(ctx, "", "")
		if err != nil {
			t.Fatalf("take %d: %v", i+1, err)
		}
		if i == 0 {
			first = si
		}
	}
	_, err := s.Snapshot(ctx, "", "")
	if !errors.Is(err, ErrTooManySnapshots) || !strings.Contains(err.Error(), "snapshot_delete") {
		t.Fatalf("a take over the cap = %v, want ErrTooManySnapshots naming snapshot_delete", err)
	}
	if list, _ := s.ListSnapshots(); len(list) != 2 {
		t.Errorf("the refused take changed the store: %d snapshots", len(list))
	}
	if err := s.DeleteSnapshot(first.Manifest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.Dir); !os.IsNotExist(err) {
		t.Errorf("the deleted snapshot is still there: %v", err)
	}
	if _, err := s.Snapshot(ctx, "", ""); err != nil {
		t.Fatalf("a take after a delete: %v", err)
	}
	if err := s.DeleteSnapshot("no-such-1"); err == nil {
		t.Error("deleting an absent snapshot succeeded")
	}
}

// The cap defaults to 20 and KQ_SANDBOX_MAX_SNAPSHOTS overrides it, clamped to [1, 200];
// garbage reads as the default.
func TestSnapshot_CapFromEnvIsClamped(t *testing.T) {
	for _, c := range []struct {
		env  string
		want int
	}{{"", DefaultMaxSnapshots}, {"5", 5}, {"0", DefaultMaxSnapshots}, {"-3", 1}, {"100000", MaxSnapshotsCeiling}, {"many", DefaultMaxSnapshots}} {
		t.Setenv("KQ_SANDBOX_MAX_SNAPSHOTS", c.env)
		s, _ := snapSupervisor(t, 1)
		if s.cfg.MaxSnapshots != c.want {
			t.Errorf("env %q: cap = %d, want %d", c.env, s.cfg.MaxSnapshots, c.want)
		}
	}
	if DefaultMaxSnapshots != 20 {
		t.Errorf("default cap = %d, want 20", DefaultMaxSnapshots)
	}
}
