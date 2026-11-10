package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/media"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A locked gateway's temporary directory, and so its media store, is inside the instance
// home (AS-1). The file tools may always read the media directory, whatever the workspace
// is, and it used to be <TMPDIR>/khunquant_media of the developer: the directory where a
// real gateway on the same machine keeps the attachments it downloaded from chats.
func TestLockedGateway_CannotReadTheDevelopersMediaDir(t *testing.T) {
	// The developer's temporary directory, holding a real gateway's downloaded attachment.
	devTmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, devTmp)
	}
	devMedia := filepath.Join(devTmp, media.TempDirName)
	if err := os.MkdirAll(devMedia, 0o700); err != nil {
		t.Fatal(err)
	}
	attachment := filepath.Join(devMedia, "chat-attachment.txt")
	if err := os.WriteFile(attachment, []byte("the-developers-attachment"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := newLockedFixture(t, ownerConfig)
	home := filepath.Dir(f.workspace)
	if !sandbox.PathInside(home, os.TempDir()) || !sandbox.PathInside(home, media.TempDir()) {
		t.Errorf("temp dir %q and media dir %q, want both inside the home %q", os.TempDir(), media.TempDir(), home)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"name": name, "as": "owner", "args": args})
		if err != nil {
			t.Fatal(err)
		}
		code, out := callToolVia(t, f, string(body))
		if code != http.StatusOK {
			t.Fatalf("%s: call_tool answered %d %v", name, code, out)
		}
		return out
	}

	if out := call("read_file", map[string]any{"path": attachment}); out["is_error"] != true || strings.Contains(fmt.Sprint(out["for_llm"]), "the-developers-attachment") {
		t.Errorf("read_file of the developer's attachment = %v, want a refusal", out)
	}
	if out := call("list_dir", map[string]any{"path": devMedia}); out["is_error"] != true || strings.Contains(fmt.Sprint(out["for_llm"]), "chat-attachment") {
		t.Errorf("list_dir of the developer's media directory = %v, want a refusal", out)
	}
	if b, err := os.ReadFile(attachment); err != nil || string(b) != "the-developers-attachment" {
		t.Errorf("the developer's attachment is %q, %v after the calls", b, err)
	}

	// Control: the instance's own media directory, outside the workspace, is still readable.
	own := filepath.Join(media.TempDir(), "own-attachment.txt")
	if err := os.MkdirAll(filepath.Dir(own), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(own, []byte("the-instances-own"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := call("read_file", map[string]any{"path": own}); out["is_error"] == true || !strings.Contains(fmt.Sprint(out["for_llm"]), "the-instances-own") {
		t.Errorf("read_file of the instance's own media file = %v, want its content", out)
	}
}
