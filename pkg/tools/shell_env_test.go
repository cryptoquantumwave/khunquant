package tools

import (
	"runtime"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestEnvNameAllowed_BaseAndPassthrough(t *testing.T) {
	tests := []struct {
		name        string
		envName     string
		passthrough []string
		want        bool
	}{
		{name: "PATH is always allowed", envName: "PATH", want: true},
		{name: "HOME is always allowed", envName: "HOME", want: true},
		{name: "locale family allowed by prefix", envName: "LC_ALL", want: true},
		{name: "API key denied by default", envName: "OPENAI_API_KEY", want: false},
		{name: "exchange secret denied by default", envName: "BINANCE_API_SECRET", want: false},
		{
			name:        "named passthrough allowed",
			envName:     "MY_TOOL_FLAG",
			passthrough: []string{"MY_TOOL_FLAG"},
			want:        true,
		},
		{
			name:        "prefix passthrough allowed",
			envName:     "CI_JOB_ID",
			passthrough: []string{"CI_*"},
			want:        true,
		},
		{
			name:        "prefix passthrough does not leak other names",
			envName:     "OPENAI_API_KEY",
			passthrough: []string{"CI_*"},
			want:        false,
		},
		{
			name:        "bare star does not restore the whole environment",
			envName:     "OPENAI_API_KEY",
			passthrough: []string{"*"},
			want:        false,
		},
		{name: "empty name rejected", envName: "", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := envNameAllowed(tc.envName, tc.passthrough); got != tc.want {
				t.Errorf("envNameAllowed(%q, %v) = %v, want %v",
					tc.envName, tc.passthrough, got, tc.want)
			}
		})
	}
}

func TestBuildChildEnv_DropsSecretsKeepsPath(t *testing.T) {
	t.Setenv("KHUNQUANT_TEST_SECRET", "super-secret-value")
	t.Setenv("PATH", "/usr/bin:/bin")

	env := buildChildEnv(nil, "")

	var sawPath bool
	for _, kv := range env {
		if strings.HasPrefix(kv, "KHUNQUANT_TEST_SECRET=") {
			t.Errorf("child env leaked the secret: %q", kv)
		}
		if strings.HasPrefix(kv, "PATH=") {
			sawPath = true
		}
	}
	if !sawPath {
		t.Error("child env has no PATH; the shell would not find any program")
	}
}

func TestBuildChildEnv_PassthroughOptsIn(t *testing.T) {
	t.Setenv("KHUNQUANT_TEST_SECRET", "super-secret-value")

	env := buildChildEnv([]string{"KHUNQUANT_TEST_SECRET"}, "")

	var found bool
	for _, kv := range env {
		if kv == "KHUNQUANT_TEST_SECRET=super-secret-value" {
			found = true
		}
	}
	if !found {
		t.Error("explicitly passed-through variable is missing from the child env")
	}
}

func TestBuildChildEnv_HomeOverrideReplacesHome(t *testing.T) {
	t.Setenv("HOME", "/home/realuser")

	env := buildChildEnv(nil, "/tmp/workspace")

	var homes []string
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, "HOME="); ok {
			homes = append(homes, after)
		}
	}
	if len(homes) != 1 {
		t.Fatalf("expected exactly one HOME in child env, got %v", homes)
	}
	if homes[0] != "/tmp/workspace" {
		t.Errorf("HOME = %q, want the workspace path", homes[0])
	}
}

// TestShellTool_ChildEnv is the end-to-end form of the check: a command the
// model could actually run must not be able to read a credential out of the
// gateway's environment.
func TestShellTool_ChildEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses printenv")
	}
	t.Setenv("KHUNQUANT_TEST_SECRET", "super-secret-value")

	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatalf("NewExecTool() error: %v", err)
	}

	result := tool.Execute(t.Context(), map[string]any{
		"command": "printenv KHUNQUANT_TEST_SECRET; printenv PATH",
	})

	if strings.Contains(result.ForLLM, "super-secret-value") {
		t.Errorf("exec leaked a host environment secret to the model: %q", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "/") {
		t.Errorf("expected PATH to survive scrubbing, got %q", result.ForLLM)
	}
}

func TestShellTool_ChildEnv_PassthroughFromConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses printenv")
	}
	t.Setenv("KHUNQUANT_TEST_SECRET", "super-secret-value")

	cfg := config.DefaultConfig()
	cfg.Tools.Exec.EnvPassthrough = []string{"KHUNQUANT_TEST_SECRET"}
	// The default config blocks exec from remote channels, and a direct
	// Execute carries no channel at all (fail-closed). This test is about the
	// environment, not that gate, so opt in explicitly.
	cfg.Tools.Exec.AllowRemote = true

	tool, err := NewExecToolWithConfig("", false, cfg)
	if err != nil {
		t.Fatalf("NewExecToolWithConfig() error: %v", err)
	}

	result := tool.Execute(t.Context(), map[string]any{
		"command": "printenv KHUNQUANT_TEST_SECRET",
	})

	if !strings.Contains(result.ForLLM, "super-secret-value") {
		t.Errorf("configured passthrough did not reach the child: %q", result.ForLLM)
	}
}
