package teamswebhook

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func channelWith(t *testing.T, targets map[string]config.TeamsWebhookTarget) *TeamsWebhookChannel {
	t.Helper()
	ch, err := NewTeamsWebhookChannel(config.TeamsWebhookConfig{
		Enabled:  true,
		Webhooks: targets,
	}, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewTeamsWebhookChannel() error = %v", err)
	}
	ch.SetRunning(true)
	return ch
}

// Two layers guard against a message going nowhere.
//
// The constructor is the primary one: it refuses a config with no "default"
// target or an empty URL, so a misconfigured channel never starts. Send carries
// a second check because a target can be absent at send time even when the
// config was valid at construction — an unknown ChatID with no default to fall
// back to. Upstream has neither: it falls through with a zero-value target,
// posts to an empty URL, and reports success.

func TestNewTeamsWebhookChannel_RequiresDefaultTarget(t *testing.T) {
	_, err := NewTeamsWebhookChannel(config.TeamsWebhookConfig{
		Enabled: true,
		Webhooks: map[string]config.TeamsWebhookTarget{
			"ops": {WebhookURL: *config.NewSecureString("https://example.invalid/hook")},
		},
	}, bus.NewMessageBus())
	if err == nil {
		t.Fatal("a config with no default target was accepted")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error = %q, want it to name the missing default", err.Error())
	}
}

func TestNewTeamsWebhookChannel_RejectsEmptyURL(t *testing.T) {
	_, err := NewTeamsWebhookChannel(config.TeamsWebhookConfig{
		Enabled:  true,
		Webhooks: map[string]config.TeamsWebhookTarget{"default": {}},
	}, bus.NewMessageBus())
	if err == nil {
		t.Fatal("a target with an empty webhook_url was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to explain the empty URL", err.Error())
	}
}

// Send's own check: the config was valid, but the requested target is not in it
// and there is nothing to fall back to.
func TestSend_RefusesUnknownTargetWhenDefaultRemoved(t *testing.T) {
	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString("https://example.invalid/hook")},
	})
	// Simulate the target disappearing after construction.
	ch.config.Webhooks = map[string]config.TeamsWebhookTarget{}

	err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "marketing", Content: "hello"})
	if err == nil {
		t.Fatal("sending with no resolvable target succeeded; the message would have gone nowhere")
	}
	if !strings.Contains(err.Error(), "no webhook configured") {
		t.Errorf("error = %q, want it to name the problem", err.Error())
	}
	if !strings.Contains(err.Error(), "marketing") {
		t.Errorf("error = %q, want it to name the requested target", err.Error())
	}
}

// A message with no ChatID uses "default", which is how a single-target setup
// is expected to work.
func TestSend_EmptyChatIDUsesDefault(t *testing.T) {
	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString("https://example.invalid/hook")},
	})

	err := ch.Send(context.Background(), bus.OutboundMessage{Content: "hello"})
	// The send itself will fail against an unreachable host — what matters is
	// that it got past target resolution rather than being refused as
	// unconfigured.
	if err != nil && strings.Contains(err.Error(), "no webhook configured") {
		t.Errorf("empty ChatID did not resolve to the default target: %v", err)
	}
}

// The webhook URL is the credential — anyone holding it can post to the channel
// — so it must not be recoverable from a marshalled config.
func TestTeamsWebhookTarget_URLIsWithheldOnMarshal(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channels.TeamsWebhook.Enabled = true
	cfg.Channels.TeamsWebhook.Webhooks = map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString("https://outlook.office.com/webhook/SECRET-TOKEN")},
	}

	path := t.TempDir() + "/config.json"
	if err := config.SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "SECRET-TOKEN") {
		t.Error("config.json contains the webhook URL; it is a credential and must be withheld")
	}
}
