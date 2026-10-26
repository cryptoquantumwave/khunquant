package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	var guarded string
	errRefused := errors.New("refused by guard")
	SetSaveGuard(func(p string) error { guarded = p; return errRefused })
	t.Cleanup(func() { SetSaveGuard(nil) })

	cfg := DefaultConfig()
	if err := SaveConfig(path, cfg); !errors.Is(err, errRefused) {
		t.Fatalf("SaveConfig error = %v, want the guard's error", err)
	}
	if guarded != path {
		t.Fatalf("guard saw path %q, want %q", guarded, path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused save wrote files: %v", entries)
	}

	// A nil guard clears it.
	SetSaveGuard(nil)
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig after clearing the guard: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written after clearing the guard: %v", err)
	}
}
