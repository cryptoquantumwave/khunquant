package providers

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func lockSandboxForTest(t *testing.T, allow ...string) {
	t.Helper()
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: "t-0123456789abcdef0123456789abcdef", AllowHosts: allow}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateProviderFromConfig_LockedRefusesCLI(t *testing.T) {
	lockSandboxForTest(t)

	for _, model := range []string{
		"claude-cli/x", "claudecli/x", "codex-cli/x", "codexcli/x",
		"github-copilot/x", "copilot/x", "antigravity/x",
		"gemini-code-assist/x", "google-gemini/x",
	} {
		p, id, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "m", Model: model, APIBase: "http://127.0.0.1:9/v1"})
		if err == nil || p != nil || id != "" {
			t.Errorf("%s: got (%v, %q, %v), want an error and no provider", model, p, id, err)
		}
	}
}

func TestCreateProviderFromConfig_LockedHTTPRules(t *testing.T) {
	lockSandboxForTest(t, "llm.allowed.example")

	build := func(mc config.ModelConfig) error {
		mc.ModelName = "m"
		_, _, err := CreateProviderFromConfig(&mc)
		return err
	}

	if err := build(config.ModelConfig{Model: "openai/m", APIBase: "http://127.0.0.1:9/v1", APIKey: *config.NewSecureString("k")}); err != nil {
		t.Errorf("loopback openai must build: %v", err)
	}
	if err := build(config.ModelConfig{Model: "openai/m", APIKey: *config.NewSecureString("k")}); err == nil {
		t.Error("openai with the default (real) api_base must be refused")
	}
	if err := build(config.ModelConfig{Model: "openai/m", APIBase: "https://api.openai.com/v1", APIKey: *config.NewSecureString("k")}); err == nil {
		t.Error("openai with a real api_base must be refused")
	}
	if err := build(config.ModelConfig{Model: "openai/m", APIBase: "https://llm.allowed.example/v1", APIKey: *config.NewSecureString("k")}); err != nil {
		t.Errorf("an allowed host must build: %v", err)
	}
	// OAuth/token auth ignores api_base, so a loopback base must not let it through.
	if err := build(config.ModelConfig{Model: "openai/m", APIBase: "http://127.0.0.1:9/v1", AuthMethod: "oauth"}); err == nil {
		t.Error("auth_method oauth must be refused under the lock")
	}
	if err := build(config.ModelConfig{Model: "anthropic/m", APIBase: "http://127.0.0.1:9/v1", AuthMethod: "token"}); err == nil {
		t.Error("auth_method token must be refused under the lock")
	}
}

func TestCreateProviderFromConfig_UnlockedUnchanged(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	// Not locked: the real default base is fine (nothing is contacted by building).
	p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "m", Model: "openai/m", APIKey: *config.NewSecureString("k")})
	if err != nil || p == nil {
		t.Fatalf("unlocked openai must build: %v", err)
	}
}
