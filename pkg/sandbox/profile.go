package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// ProfileOptions describes the instance a sandbox profile is written for.
type ProfileOptions struct {
	Home string // absolute path of the instance home (KHUNQUANT_HOME)
	Port int    // gateway port on 127.0.0.1
}

// Profile returns the config of a sandbox instance: the defaults (every chat channel
// off) with workspace and fixtures inside Home, the gateway on 127.0.0.1:Port, sandbox
// and dev MCP on without tokens, and exec, MCP, heartbeat and the web owner route off.
// The model is the interim "sandbox-offline" on the loopback discard port (until
// SBX-04): it can be built but never reaches an LLM. The result already satisfies the
// gateway's locked-config overlay.
func Profile(o ProfileOptions) (*config.Config, error) {
	if !filepath.IsAbs(o.Home) {
		return nil, fmt.Errorf("sandbox profile: home %q is not an absolute path", o.Home)
	}
	if o.Port < 1 || o.Port > 65535 {
		return nil, fmt.Errorf("sandbox profile: port %d is out of range", o.Port)
	}
	workspace := filepath.Join(filepath.Clean(o.Home), "workspace")

	cfg := config.DefaultConfig() // reads the building process's home; only paths set below survive
	cfg.Agents.Defaults.Workspace = workspace
	cfg.Agents.Defaults.RestrictToWorkspace = true
	cfg.Agents.Defaults.ModelName = "sandbox"
	cfg.ModelList = []config.ModelConfig{{
		ModelName: "sandbox",
		Model:     "openai/sandbox-offline",
		APIBase:   "http://127.0.0.1:9/v1",
		APIKey:    *config.NewSecureString("sandbox"),
	}}
	cfg.Gateway.Host, cfg.Gateway.Port = "127.0.0.1", o.Port
	cfg.Debug.Sandbox = config.SandboxConfig{Enabled: true, FixturesDir: filepath.Join(workspace, "sandbox")}
	cfg.Debug.DevMCP.Enabled, cfg.Debug.DevMCP.Token = true, ""
	cfg.Tools.Exec.Enabled, cfg.Tools.MCP.Enabled = false, false
	off := false
	cfg.Proactive.Owner.WebIsOwner = &off
	cfg.Heartbeat.Enabled, cfg.Devices.Enabled = false, false
	return cfg, nil
}

// WriteProfile writes the profile to <home>/config.json and returns that path.
// config.SaveConfig puts the secrets in <home>/.security.yml, so both files land in
// the instance home. It runs in the process that creates the instance (not locked).
func WriteProfile(o ProfileOptions) (string, error) {
	cfg, err := Profile(o)
	if err != nil {
		return "", err
	}
	home := filepath.Clean(o.Home)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", fmt.Errorf("sandbox profile: create home: %w", err)
	}
	path := filepath.Join(home, "config.json")
	if err := config.SaveConfig(path, cfg); err != nil {
		return "", fmt.Errorf("sandbox profile: write config: %w", err)
	}
	return path, nil
}
