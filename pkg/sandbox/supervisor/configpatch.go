package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// checkOverlay refuses a config patch or workspace file set that CheckConfigPatch and
// CheckWorkspacePath refuse: the supervisor enforces them itself, so no caller (a
// scenario runner, a test) can skip the check.
func checkOverlay(patch map[string]any, workspace map[string]string) error {
	if issues := CheckConfigPatch(patch); len(issues) > 0 {
		i := issues[0]
		return fmt.Errorf("sandbox: config patch refused at %s: %s", strings.Join(i.Path, "."), i.Msg)
	}
	for p := range workspace {
		if err := CheckWorkspacePath(p); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
	}
	return nil
}

// mergePatch applies an RFC 7386 merge patch to dst: objects merge, null removes a key,
// anything else (arrays included) replaces.
func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		switch t := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			sub, _ := dst[k].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
			}
			mergePatch(sub, t)
			dst[k] = sub
		default:
			dst[k] = v
		}
	}
}

// patchConfigFile merges patch into <home>/config.json and says whether the file
// changed. It is written to a temporary file and renamed, so the gateway's config
// watcher never reads half a file; an unchanged file is left alone, because the watcher
// would not reload it either.
func patchConfigFile(home string, patch map[string]any) (bool, error) {
	path := filepath.Join(home, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var cfg map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // a port or an id stays exact
	if err := dec.Decode(&cfg); err != nil {
		return false, fmt.Errorf("sandbox: %s is not JSON: %w", path, err)
	}
	before, _ := json.Marshal(cfg)
	mergePatch(cfg, patch)
	after, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return false, err
	}
	if compact, _ := json.Marshal(cfg); bytes.Equal(before, compact) {
		return false, nil
	}
	tmp, err := os.CreateTemp(home, ".config-*.tmp")
	if err != nil {
		return false, err
	}
	_, werr := tmp.Write(append(after, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return false, werr
	}
	return true, nil
}

// writeWorkspace writes files under <home>/workspace. A path that would lead out of it
// (checked again after symlinks are resolved) is refused.
func writeWorkspace(home string, files map[string]string) error {
	root := filepath.Join(home, "workspace")
	for rel, content := range files {
		if err := CheckWorkspacePath(rel); err != nil {
			return err
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if !sandbox.PathInside(root, dst) {
			return fmt.Errorf("sandbox: workspace path %q leads out of the workspace", rel)
		}
		if err := os.WriteFile(dst, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// PatchConfig merges patch into the instance's config.json. The running gateway notices
// the change within a couple of seconds and reloads (journaling config.reload); changed
// is false when the patch altered nothing, in which case no reload will come. The patch
// is checked by CheckConfigPatch; what it may not say (a real model, a workspace outside
// the home) the gateway refuses itself on reload, exiting with code 3.
func (s *Supervisor) PatchConfig(_ context.Context, name string, patch map[string]any) (changed bool, err error) {
	if err := checkOverlay(patch, nil); err != nil {
		return false, err
	}
	in, err := s.lookup(name)
	if err != nil {
		return false, err
	}
	in.opMu.Lock()
	defer in.opMu.Unlock()
	if st := in.info().State; st == StateExited {
		return false, errors.New("sandbox: the instance has exited")
	}
	return patchConfigFile(in.home, patch)
}
