package config

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

func TestRegisterSecretsForRedaction_CoversConfigCredentials(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Channels.Telegram.Token.Set("123456:telegram-bot-token-for-redaction")
	cfg.Gateway.ManagementToken = "mgmt-token-for-redaction-0001"
	cfg.Tools.Web.Brave.APIKeys = []string{"brave-key-for-redaction-01"}
	cfg.Tools.MCP.Servers = map[string]MCPServerConfig{
		"gh": {Env: map[string]string{"API_KEY": "mcp-env-key-for-redaction-9"}},
	}
	// Long enough to register, but not a credential field.
	cfg.Channels.Telegram.BaseURL = "https://not-a-secret.example"

	registerSecretsForRedaction(cfg)

	for _, secret := range []string{
		"123456:telegram-bot-token-for-redaction", // SecureString field
		"mgmt-token-for-redaction-0001",           // plain-string management token
		"brave-key-for-redaction-01",              // plain []string in a yaml:"-" subtree, found by name
		"mcp-env-key-for-redaction-9",             // map value under a credential-named key
	} {
		if got := logger.Redact("value=" + secret); strings.Contains(got, secret) {
			t.Errorf("%q was not registered for redaction", secret)
		}
	}
	if got := logger.Redact("https://not-a-secret.example"); got != "https://not-a-secret.example" {
		t.Errorf("a non-credential field was registered: %q", got)
	}
}

// Short values are left alone: Redact is a substring replace, so registering
// one would mangle unrelated log text.
func TestRegisterSecretsForRedaction_SkipsShortValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Channels.Telegram.Token.Set("abc")
	registerSecretsForRedaction(cfg)

	if got := logger.Redact("abcdef"); got != "abcdef" {
		t.Fatalf("a 3-character value was registered: %q", got)
	}
}
