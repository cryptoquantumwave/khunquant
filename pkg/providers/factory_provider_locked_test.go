package providers

import (
	"strings"
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

// Under the lock every real provider is refused, whatever its api_base, host or auth.
func TestLockedRefusesRealProviders(t *testing.T) {
	lockSandboxForTest(t, "llm.allowed.example")

	for _, model := range []string{
		"openai/m", "anthropic/m", "azure/m", "ollama/m", "litellm/m", "m",
		"claude-cli/x", "claudecli/x", "codex-cli/x", "codexcli/x",
		"github-copilot/x", "copilot/x", "antigravity/x",
		"gemini-code-assist/x", "google-gemini/x",
	} {
		for _, base := range []string{"", "http://127.0.0.1:9/v1", "https://llm.allowed.example/v1"} {
			p, id, err := CreateProviderFromConfig(&config.ModelConfig{
				ModelName: "m", Model: model, APIBase: base, APIKey: *config.NewSecureString("k"),
			})
			if err == nil || p != nil || id != "" {
				t.Errorf("%s (base %q): got (%v, %q, %v), want an error and no provider", model, base, p, id, err)
			}
		}
	}
	for _, am := range []string{"oauth", "token"} {
		if _, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "m", Model: "sim/scripted", AuthMethod: am}); err == nil {
			t.Errorf("auth_method %q must be refused under the lock", am)
		}
	}
	if _, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "m", Model: "codex-cli/x"}); err == nil ||
		!strings.Contains(err.Error(), "codex-cli") {
		t.Errorf("the refusal must name the protocol, got %v", err)
	}

	p, id, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/scripted"})
	if err != nil || p == nil || id != "scripted" {
		t.Fatalf("sim/scripted must build under the lock: (%v, %q, %v)", p, id, err)
	}
	if p, id, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/replay"}); err != nil || p == nil || id != "replay" {
		t.Errorf("sim/replay must build under the lock: (%v, %q, %v)", p, id, err)
	}
	for _, ref := range []string{"sim/live", "sim/record", "sim/replay:openai/gpt-x", "sim/scripted:openai/gpt-x", "sim/nonsense"} {
		if _, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: ref}); err == nil {
			t.Errorf("%s must be refused by this build", ref)
		}
	}
}

// The sim model cannot be built outside a locked process.
func TestSimRefusedWhenUnlocked(t *testing.T) {
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/scripted"})
	if err == nil || p != nil {
		t.Fatalf("sim/scripted on an unlocked process = (%v, %v), want an error", p, err)
	}
	if !strings.Contains(err.Error(), "sandbox-locked") {
		t.Errorf("error %q does not say why", err)
	}
	// Replay, record and live are refused the same way, whatever the environment holds: an
	// unlocked process never builds a real inner model through sim, and never reads the key.
	t.Setenv(sandbox.EnvLLMKey, testKey)
	t.Setenv(sandbox.EnvLLMModels, testAllow)
	t.Setenv(sandbox.EnvLLMBudget, "2USD")
	t.Setenv(sandbox.EnvLLMPrices, testModel+"=3:15")
	for _, ref := range []string{"sim/replay", "sim/record:openai/" + testModel, "sim/live:anthropic-messages/" + testModel} {
		p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: ref})
		if err == nil || p != nil || !strings.Contains(err.Error(), "sandbox-locked") {
			t.Errorf("%s on an unlocked process = (%v, %v), want the lock refusal", ref, p, err)
		}
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
