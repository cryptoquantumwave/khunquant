package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// replayCaseFiles are the files of a replay case: the only ones copied out of the home.
var replayCaseFiles = []string{"events.jsonl", "policy.json", "labels.json", "golden.json"}

var replayCaseRE = regexp.MustCompile(`^[a-z0-9_]{1,48}$`)

// exportReplay has the passing instance write its journal as a replay case
// (inspect export_replay) and copies <home>/lab/export/<case>/ to <ExportDir>/<case>/.
// The case is named after the scenario, with - as _. It exports the first passing run of
// a scenario only (a matrix would write the same case twice).
func (s *runState) exportReplay(ctx context.Context) error {
	r := s.r
	name := strings.ReplaceAll(strings.ToLower(s.f.Scenario), "-", "_")
	if !replayCaseRE.MatchString(name) {
		return fmt.Errorf("the scenario id %q makes no replay case name (^[a-z0-9_]{1,48}$ after - becomes _)", s.f.Scenario)
	}
	labels, skipped := ReplayLabels(s.f, s.res)
	s.res.ExportSkipped = skipped
	if len(labels) == 0 {
		return fmt.Errorf("no replay label can be made from the message expectations (%s)", strings.Join(skipped, "; "))
	}
	raw, err := call(ctx, s.c, "inspect", "export_replay", map[string]any{"case": name, "labels": labels})
	if err != nil {
		return err
	}
	var out struct {
		Dir     string   `json:"dir"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	s.res.ExportSkipped = append(s.res.ExportSkipped, out.Skipped...)
	dest, err := CopyReplayCase(filepath.Join(s.home, filepath.FromSlash(out.Dir)), s.home, r.ExportDir, name, r.ExportForce)
	s.res.Exported = dest
	return err
}

// CopyReplayCase copies the four files of the case in src (which must lie inside home)
// to <dir>/<name>/. The destination is always below dir, an existing case is refused
// unless force, and only regular files are read.
func CopyReplayCase(src, home, dir, name string, force bool) (string, error) {
	if !replayCaseRE.MatchString(name) || !sandbox.PathInside(home, src) {
		return "", errors.New("export: the case is not a plain name inside the instance home")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	if !sandbox.PathInside(dir, dest) || filepath.Dir(dest) != filepath.Clean(dir) {
		return "", fmt.Errorf("export: %s is outside %s", dest, dir)
	}
	if _, err := os.Lstat(dest); err == nil && !force {
		return "", fmt.Errorf("export: %s exists (--force replaces it)", dest)
	}
	stage, err := os.MkdirTemp(dir, ".export-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for _, f := range replayCaseFiles {
		if err := copyRegular(filepath.Join(src, f), filepath.Join(stage, f)); err != nil {
			return "", err
		}
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return "", err
	}
	return dest, os.Rename(stage, dest)
}

func copyRegular(from, to string) error {
	if st, err := os.Lstat(from); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("export: %s is not a regular file (%v)", from, err)
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
