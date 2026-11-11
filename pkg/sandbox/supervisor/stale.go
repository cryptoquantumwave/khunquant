package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/pid"
)

// staleAfter is how old a home must be before it can be stale: a supervisor of another
// process may be starting an instance in it, and its pid file is not written yet.
const staleAfter = 2 * time.Minute

// scanStale lists the kq-sbx-* homes under tmpDir that carry the instance marker, are
// not in skip (the instances this supervisor runs) and have no live process. They are
// listed, never adopted: nothing knows their token. A home whose process is alive, or
// whose marker names a supervisor that is alive, belongs to another supervisor and is
// left out. (A home of this process that is no longer in the table counts as alive.)
func scanStale(tmpDir string, skip map[string]bool) ([]Info, error) {
	if tmpDir == "" {
		tmpDir = os.TempDir()
	}
	dirs, err := filepath.Glob(filepath.Join(tmpDir, homePrefix+"*"))
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, dir := range dirs {
		name := strings.TrimPrefix(filepath.Base(dir), homePrefix)
		if skip[name] || !nameRE.MatchString(name) {
			continue
		}
		if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
			continue
		}
		mk, err := os.Lstat(filepath.Join(dir, instanceMarker))
		if err != nil || !mk.Mode().IsRegular() || time.Since(mk.ModTime()) < staleAfter {
			continue
		}
		// A marker that names its supervisor is stale only when that supervisor is gone
		// too: it may be keeping a crashed instance to read its log or to restart it.
		// Checked first so a live owner's pid file is not touched (the check below
		// deletes a dead pid file). A marker with no supervisor keeps the pid rule.
		if sp := markerSupervisor(dir); sp > 0 && processAlive(sp) {
			continue
		}
		if pf := pid.ReadPidFileWithCheck(dir); pf != nil {
			continue // alive: another supervisor's
		}
		out = append(out, Info{Name: name, Home: dir, LogPath: filepath.Join(dir, logName), State: StateExited, Stale: true})
	}
	slices.SortFunc(out, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
