package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/heartbeat"
)

// A fresh install gets the v2 HEARTBEAT.md, the template that works in every
// heartbeat mode (the model reports with respond_proactive when it has that tool
// and with one `message` when it does not).
func TestOnboard_FreshInstallGetsTheV2HeartbeatTemplate(t *testing.T) {
	target := t.TempDir()
	if err := copyEmbeddedToTarget(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "HEARTBEAT.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !heartbeat.IsV2Template(text) || heartbeat.IsV1Template(text) {
		t.Fatalf("onboarding wrote a template that is not v2 (v1=%v v2=%v)",
			heartbeat.IsV1Template(text), heartbeat.IsV2Template(text))
	}
	for _, want := range []string{"`respond_proactive`", "`message`", "Add your heartbeat tasks below this line:"} {
		if !strings.Contains(text, want) {
			t.Errorf("the v2 template lacks %q", want)
		}
	}
	// It is the one v2 text, byte for byte.
	if text != heartbeat.TemplateV2() {
		t.Fatal("the embedded onboarding copy differs from pkg/heartbeat/template_v2.md; " +
			"copy the template to workspace/HEARTBEAT.md and run `go generate ./...`")
	}
}
