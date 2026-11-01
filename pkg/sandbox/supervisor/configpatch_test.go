//go:build !windows

package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func TestMergePatch(t *testing.T) {
	dst := map[string]any{"a": map[string]any{"x": 1.0, "y": 2.0}, "list": []any{1.0, 2.0}, "gone": "v", "keep": true}
	mergePatch(dst, map[string]any{"a": map[string]any{"y": 3.0, "z": map[string]any{"deep": "d"}}, "list": []any{9.0}, "gone": nil})
	want := map[string]any{"a": map[string]any{"x": 1.0, "y": 3.0, "z": map[string]any{"deep": "d"}}, "list": []any{9.0}, "keep": true}
	if !reflect.DeepEqual(dst, want) {
		t.Fatalf("merged = %v, want %v", dst, want)
	}
}

// Up applies the instance block before the gateway starts; PatchConfig changes the
// file of a running instance atomically and says when nothing changed.
func TestUpWithConfigPatchAndWorkspace(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	info, err := sup.Up(context.Background(), UpOptions{
		Name: "patched", FixturesFrom: repoFixtures(t),
		ConfigPatch: map[string]any{"heartbeat": map[string]any{"enabled": true, "interval": 30}},
		Workspace:   map[string]string{"HEARTBEAT.md": "check BTC", "memory/notes/a.md": "x"},
	})
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(func() { _ = sup.Down(context.Background(), "patched", false) })

	cfg := readJSONFile(t, filepath.Join(info.Home, "config.json"))
	if hb, _ := cfg["heartbeat"].(map[string]any); hb["enabled"] != true || hb["interval"] != 30.0 {
		t.Errorf("heartbeat = %v, want the patch merged", cfg["heartbeat"])
	}
	if gw, _ := cfg["gateway"].(map[string]any); gw["host"] != "127.0.0.1" {
		t.Errorf("the profile's gateway block must survive the merge: %v", cfg["gateway"])
	}
	for rel, want := range map[string]string{"HEARTBEAT.md": "check BTC", "memory/notes/a.md": "x"} {
		got, err := os.ReadFile(filepath.Join(info.Home, "workspace", filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("workspace/%s = %q (%v), want %q", rel, got, err, want)
		}
	}

	changed, err := sup.PatchConfig(context.Background(), "patched", map[string]any{"heartbeat": map[string]any{"interval": 30}})
	if err != nil || changed {
		t.Errorf("a patch that changes nothing: changed=%v, %v", changed, err)
	}
	past := time.Now().Add(-time.Hour) // not a sleep: the file is aged so "newer" is certain
	if err := os.Chtimes(filepath.Join(info.Home, "config.json"), past, past); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(filepath.Join(info.Home, "config.json"))
	changed, err = sup.PatchConfig(context.Background(), "patched", map[string]any{"heartbeat": map[string]any{"interval": 45}})
	if err != nil || !changed {
		t.Fatalf("PatchConfig: changed=%v, %v", changed, err)
	}
	after, _ := os.Stat(filepath.Join(info.Home, "config.json"))
	if !after.ModTime().After(before.ModTime()) || after.Mode().Perm() != 0o600 {
		t.Errorf("config.json mtime %v -> %v, mode %v: the watcher needs a newer file, and it is private", before.ModTime(), after.ModTime(), after.Mode())
	}
	if hb, _ := readJSONFile(t, filepath.Join(info.Home, "config.json"))["heartbeat"].(map[string]any); hb["interval"] != 45.0 || hb["enabled"] != true {
		t.Errorf("heartbeat = %v", hb)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(info.Home, ".config-*.tmp")); len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// What a scenario may not do is refused by the supervisor itself, whoever calls it: an
// escaping workspace path or a patch of the lock's keys starts nothing and writes nothing.
func TestUpAndPatchRefuseWhatTheLockOwns(t *testing.T) {
	base := sandboxEnv(t)
	sup := newFake(t, base)
	for name, o := range map[string]UpOptions{
		"workspace dotdot": {Workspace: map[string]string{"../escape": "x"}},
		"workspace abs":    {Workspace: map[string]string{"/tmp/escape": "x"}},
		"lock off":         {ConfigPatch: map[string]any{"debug": map[string]any{"sandbox": map[string]any{"enabled": false}}}},
		"gateway port":     {ConfigPatch: map[string]any{"gateway": map[string]any{"port": 1}}},
		"workspace dir":    {ConfigPatch: map[string]any{"agents": map[string]any{"defaults": map[string]any{"workspace": "/"}}}},
		"credential":       {ConfigPatch: map[string]any{"exchanges": map[string]any{"okx": map[string]any{"api_key": "k"}}}},
		"real channel":     {ConfigPatch: map[string]any{"channels": map[string]any{"telegram": map[string]any{"enabled": true}}}},
		"real url":         {ConfigPatch: map[string]any{"x": "https://api.binance.com"}},
	} {
		o.Name, o.FixturesFrom = "refused", repoFixtures(t)
		if _, err := sup.Up(context.Background(), o); err == nil {
			t.Errorf("%s: Up must refuse", name)
			_ = sup.Down(context.Background(), "refused", false)
		}
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("a refused Up must leave nothing behind, found %v", entries)
	}

	if _, err := sup.Up(context.Background(), UpOptions{Name: "ok", FixturesFrom: repoFixtures(t)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Down(context.Background(), "ok", false) })
	_, err := sup.PatchConfig(context.Background(), "ok", map[string]any{"debug": map[string]any{"sandbox": map[string]any{"enabled": false}}})
	if err == nil || !strings.Contains(err.Error(), "debug.sandbox") {
		t.Errorf("PatchConfig of the lock: %v", err)
	}
	// A real model is not statically refused: the gateway refuses it on reload and exits 3
	// (TestLockedGateway_RefusedReloadIsJournaled), which the runner reports.
	if _, err := sup.PatchConfig(context.Background(), "ok", map[string]any{"model_list": []any{map[string]any{"model_name": "x", "model": "openai/gpt-5"}}}); err != nil {
		t.Errorf("a model patch is the gateway's to refuse: %v", err)
	}
}
