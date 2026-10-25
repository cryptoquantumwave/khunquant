package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

func TestBuildServerEnv_WithholdsSecretsKeepsBase(t *testing.T) {
	t.Setenv("KHUNQUANT_TEST_MCP_SECRET", "sk-live-should-not-leak")
	t.Setenv("PATH", "/usr/bin:/bin")

	env, err := buildServerEnv("s", config.MCPServerConfig{Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envValue(env, "KHUNQUANT_TEST_MCP_SECRET"); ok {
		t.Fatal("MCP server inherited a secret from the parent environment")
	}
	// Control: the base allowlist still gets through, so the server can run.
	if v, _ := envValue(env, "PATH"); v != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want the parent's", v)
	}
}

func TestBuildServerEnv_InheritEnvOptsIn(t *testing.T) {
	t.Setenv("KHUNQUANT_TEST_MCP_TOKEN", "needed")
	t.Setenv("KHUNQUANT_TEST_MCP_GROUP_A", "a")
	t.Setenv("KHUNQUANT_TEST_MCP_OTHER", "not listed")

	env, err := buildServerEnv("s", config.MCPServerConfig{
		Command:    "x",
		InheritEnv: []string{"KHUNQUANT_TEST_MCP_TOKEN", "KHUNQUANT_TEST_MCP_GROUP_*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"KHUNQUANT_TEST_MCP_TOKEN":   "needed",
		"KHUNQUANT_TEST_MCP_GROUP_A": "a",
	} {
		if v, _ := envValue(env, name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if _, ok := envValue(env, "KHUNQUANT_TEST_MCP_OTHER"); ok {
		t.Error("a variable not named in inherit_env was passed through")
	}
}

func TestBuildServerEnv_InheritAllEnvRestoresOldBehaviour(t *testing.T) {
	t.Setenv("KHUNQUANT_TEST_MCP_SECRET", "s")

	env, err := buildServerEnv("s", config.MCPServerConfig{Command: "x", InheritAllEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envValue(env, "KHUNQUANT_TEST_MCP_SECRET"); !ok {
		t.Fatal("inherit_all_env did not pass the full environment")
	}
}

// env and env_file are the operator's explicit values; the scrub must not
// touch them, and env still overrides env_file.
func TestBuildServerEnv_ExplicitValuesAreKeptWithPrecedence(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "mcp.env")
	if err := os.WriteFile(envFile, []byte("FROM_FILE=file\nBOTH=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env, err := buildServerEnv("s", config.MCPServerConfig{
		Command: "x",
		EnvFile: envFile,
		Env:     map[string]string{"API_KEY": "explicit", "BOTH": "env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"FROM_FILE": "file", "API_KEY": "explicit", "BOTH": "env"} {
		if v, _ := envValue(env, name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
}
