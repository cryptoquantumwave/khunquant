package agent

import (
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/media"
)

// TestBuildExecAllowPatterns_UsesWriteAllowlist covers M-1.
//
// exec runs a shell, so any path it may reach it may also write. Handing it the
// read allowlist silently turned "these paths are readable" into "these paths
// are writable"; it gets the write allowlist instead.
func TestBuildExecAllowPatterns_UsesWriteAllowlist(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.AllowReadPaths = []string{`^/srv/reference(?:/|$)`}
	cfg.Tools.AllowWritePaths = []string{`^/srv/output(?:/|$)`}

	patterns := buildExecAllowPatterns(cfg)

	matches := func(path string) bool {
		for _, p := range patterns {
			if p.MatchString(path) {
				return true
			}
		}
		return false
	}

	if !matches("/srv/output/report.csv") {
		t.Error("exec cannot reach a path on the write allowlist")
	}
	if matches("/srv/reference/secrets.txt") {
		t.Error("exec can still reach a read-only allow path, so it can write there too")
	}
	if !matches(filepath.Join(media.TempDir(), "clip.mp4")) {
		t.Error("exec lost access to the media temp dir, which breaks media processing")
	}
}

func TestBuildExecAllowPatterns_NilConfigStillAllowsMediaDir(t *testing.T) {
	patterns := buildExecAllowPatterns(nil)

	var found bool
	for _, p := range patterns {
		if p.MatchString(filepath.Join(media.TempDir(), "clip.mp4")) {
			found = true
		}
	}
	if !found {
		t.Error("media temp dir is not reachable with a nil config")
	}
}
