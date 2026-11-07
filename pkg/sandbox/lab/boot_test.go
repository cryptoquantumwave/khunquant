package lab

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNextBoot_CountsAndFailsClosed(t *testing.T) {
	home := t.TempDir()
	for want := int64(1); want <= 4; want++ {
		if got, err := NextBoot(home); err != nil || got != want {
			t.Fatalf("boot %d = %d, %v", want, got, err)
		}
	}
	if info, err := os.Stat(filepath.Join(home, "lab", "boot")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("boot file: %v, %v", info, err)
	}
	// Another home counts on its own.
	if got, _ := NextBoot(t.TempDir()); got != 1 {
		t.Errorf("a new home starts at %d", got)
	}
	// A counter that is not a count stops the lab: resetting it would repeat a stream.
	if err := os.WriteFile(filepath.Join(home, "lab", "boot"), []byte("many\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := NextBoot(home); err == nil {
		t.Errorf("a corrupt counter gave boot %d", got)
	}
}

// The first boot keeps the seed (a fresh run is what it always was), every later boot of
// the same seed has a stream of its own, and the same (seed, boot) pair is the same stream.
func TestMixSeed(t *testing.T) {
	if MixSeed(42, 1) != 42 || MixSeed(42, 0) != 42 {
		t.Fatal("the first boot must keep the seed")
	}
	seen := map[int64]int64{42: 1}
	for boot := int64(2); boot <= 50; boot++ {
		m := MixSeed(42, boot)
		if prev, dup := seen[m]; dup {
			t.Fatalf("boots %d and %d share seed %d", prev, boot, m)
		}
		seen[m] = boot
		if m != MixSeed(42, boot) {
			t.Fatalf("boot %d is not deterministic", boot)
		}
	}
	if MixSeed(42, 2) == MixSeed(43, 2) {
		t.Error("two seeds collide on the same boot")
	}
}
