package sandbox

import (
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ResolveFixturesDir resolves the fixtures directory from the sandbox config.
// If config.Debug.Sandbox.FixturesDir is empty, it defaults to <workspace>/sandbox.
// This function is used by both the server and external tools (e.g., T6 recorder)
// to ensure they use the same fixtures location.
func ResolveFixturesDir(cfg *config.Config) string {
	if cfg.Debug.Sandbox.FixturesDir != "" {
		return cfg.Debug.Sandbox.FixturesDir
	}

	// Default to workspace/sandbox.
	return filepath.Join(cfg.Agents.Defaults.Workspace, "sandbox")
}
