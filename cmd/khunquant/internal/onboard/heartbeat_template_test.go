package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
)

// A fresh install gets the v1 HEARTBEAT.md. The default heartbeat mode is legacy,
// and a legacy turn with the v2 file would scan on every tick and deliver nothing;
// v2 is opt-in through `khunquant heartbeat upgrade-template`.
func TestOnboard_FreshInstallGetsTheV1HeartbeatTemplate(t *testing.T) {
	target := t.TempDir()
	if err := copyEmbeddedToTarget(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !heartbeat.IsV1Template(text) || heartbeat.IsV2Template(text) {
		t.Fatalf("onboarding wrote a template that is not v1 (v1=%v v2=%v)",
			heartbeat.IsV1Template(text), heartbeat.IsV2Template(text))
	}
	if !strings.Contains(text, "`message`") {
		t.Error("the v1 template no longer tells the model to call `message`")
	}

	// And it is the repository's workspace/HEARTBEAT.md, which this change leaves
	// as it was.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
	want, err := os.ReadFile(filepath.Join(dir, "workspace", "HEARTBEAT.md"))
	if err != nil || len(want) == 0 {
		t.Fatalf("reading workspace/HEARTBEAT.md: %v (%d bytes)", err, len(want))
	}
	if string(want) != text {
		t.Fatal("the embedded onboarding copy differs from workspace/HEARTBEAT.md; run `go generate ./...`")
	}
}
