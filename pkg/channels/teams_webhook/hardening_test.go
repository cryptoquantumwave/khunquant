package teamswebhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	goteamsnotify "github.com/atc0005/go-teams-notify/v2"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// --- T4-1: the webhook URL (the bearer secret) must never appear in a ---
// --- returned error string.                                           ---

// TestSanitizeWebhookError_RedactsKnownURL covers the primary path: the
// go-teams-notify client's own URL-pattern validation embeds the full
// webhook URL verbatim in its error (see the vendored client's
// validateWebhook), and sanitizeWebhookError is told about that exact URL.
func TestSanitizeWebhookError_RedactsKnownURL(t *testing.T) {
	secretURL := "https://example.invalid/services/SECRET-PATH/here?key=SECRET-QUERY"
	err := errors.New("failed to validate webhook URL: webhook URL does not match one of expected patterns; got: " +
		"\"" + secretURL + "\", patterns: some-pattern")

	got := sanitizeWebhookError(err, secretURL)
	if got == nil {
		t.Fatal("sanitizeWebhookError returned nil for a non-nil error")
	}
	if strings.Contains(got.Error(), secretURL) {
		t.Errorf("sanitized error still contains the webhook URL: %v", got)
	}
	if strings.Contains(got.Error(), "SECRET-PATH") || strings.Contains(got.Error(), "SECRET-QUERY") {
		t.Errorf("sanitized error still contains credential fragments: %v", got)
	}
	if !strings.Contains(got.Error(), redactedWebhookURL) {
		t.Errorf("sanitized error = %q, want it to contain %q", got.Error(), redactedWebhookURL)
	}
}

// TestSanitizeWebhookError_DefensiveRegexCatchesUnknownURL covers a URL that
// isn't one of the exact configured targets (e.g. surfaced by a redirect)
// but still looks like a Teams/Power-Automate webhook host.
func TestSanitizeWebhookError_DefensiveRegexCatchesUnknownURL(t *testing.T) {
	surprising := "https://contoso.webhook.office.com/webhookb2/SECRET-GUID/IncomingWebhook/SECRET2"
	err := errors.New("redirected to " + surprising)

	got := sanitizeWebhookError(err) // no known URLs supplied
	if strings.Contains(got.Error(), "SECRET-GUID") || strings.Contains(got.Error(), "SECRET2") {
		t.Errorf("defensive regex failed to redact an unlisted webhook-looking URL: %v", got)
	}
}

// TestSanitizeWebhookError_PreservesStatusText asserts classifyTeamsError
// still works after sanitizing: the HTTP status text it keys off of must
// survive redaction.
func TestSanitizeWebhookError_PreservesStatusText(t *testing.T) {
	secretURL := "https://example.invalid/hook?key=SECRET"
	err := errors.New("error on notification: 401 Unauthorized, forbidden, url=" + secretURL)

	sanitized := sanitizeWebhookError(err, secretURL)
	classified := classifyTeamsError(sanitized)
	if !errors.Is(classified, channels.ErrSendFailed) {
		t.Errorf("classifyTeamsError(sanitized) = %v, want a permanent send-failed classification for 401", classified)
	}
	if strings.Contains(classified.Error(), "SECRET") {
		t.Errorf("classified error leaked the secret: %v", classified)
	}
}

// TestNewTeamsWebhookChannel_ConstructorErrorSanitized: url.Parse errors
// embed the raw string they failed on. A URL containing a control character
// fails url.Parse, and the resulting construction error must not repeat the
// URL (including its query, where the secret lives) verbatim.
func TestNewTeamsWebhookChannel_ConstructorErrorSanitized(t *testing.T) {
	badURL := "https://example.invalid/hook?key=SECRET-TOKEN\x7f"
	_, err := NewTeamsWebhookChannel(config.TeamsWebhookConfig{
		Enabled: true,
		Webhooks: map[string]config.TeamsWebhookTarget{
			"default": {WebhookURL: *config.NewSecureString(badURL)},
		},
	}, bus.NewMessageBus())
	if err == nil {
		t.Fatal("expected a URL containing a control character to fail construction")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("constructor error leaked the webhook URL: %v", err)
	}
}

// TestSend_ValidationErrorNeverContainsWebhookURL drives a real send (no
// mock) against a URL that does not match go-teams-notify's expected Teams
// webhook patterns. The library rejects it in ValidateWebhook before any
// network call, and its error embeds the URL verbatim - exactly the leak
// path manager.go's err.Error() logging would otherwise expose.
func TestSend_ValidationErrorNeverContainsWebhookURL(t *testing.T) {
	secretURL := "https://webhook.example.invalid/services/AAA-SECRET-ID/BBB?sig=SUPER-SECRET-SIGNATURE"
	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString(secretURL)},
	})
	ch.client = goteamsnotify.NewTeamsClient() // real client; validation needs no network

	err := ch.Send(context.Background(), bus.OutboundMessage{Content: "hello"})
	if err == nil {
		t.Fatal("expected send to fail: the URL does not match a known Teams webhook pattern")
	}
	if strings.Contains(err.Error(), "AAA-SECRET-ID") || strings.Contains(err.Error(), "SUPER-SECRET-SIGNATURE") {
		t.Errorf("Send error leaked the webhook URL: %v", err)
	}
	if strings.Contains(err.Error(), secretURL) {
		t.Errorf("Send error contains the full webhook URL: %v", err)
	}
}

// TestSend_NetworkErrorNeverContainsWebhookURL covers the other embedding
// path named in the brief: net/http errors (here, a DNS lookup failure)
// include the full request URL verbatim.
func TestSend_NetworkErrorNeverContainsWebhookURL(t *testing.T) {
	secretURL := "https://example.invalid/services/SECRET-PATH?key=SECRET-QUERY"
	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString(secretURL)},
	})
	realClient := goteamsnotify.NewTeamsClient()
	realClient.SkipWebhookURLValidationOnSend(true)
	ch.client = realClient
	ch.sendTimeout = 5 * time.Second

	err := ch.Send(context.Background(), bus.OutboundMessage{Content: "hello"})
	if err == nil {
		t.Fatal("expected send to fail against an unresolvable host")
	}
	if strings.Contains(err.Error(), "SECRET-PATH") || strings.Contains(err.Error(), "SECRET-QUERY") {
		t.Errorf("Send error leaked the webhook URL: %v", err)
	}
}

// --- T4-6: every send is bounded by a per-send timeout. ---

// TestSend_RespectsTimeout points a real client at an httptest server that
// sleeps well past a short, test-configured sendTimeout, and asserts Send
// returns close to that timeout rather than waiting for the server.
func TestSend_RespectsTimeout(t *testing.T) {
	const testTimeout = 50 * time.Millisecond
	const serverDelay = 2 * time.Second

	// TLS server: the constructor requires an HTTPS webhook URL.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString(srv.URL)},
	})
	realClient := goteamsnotify.NewTeamsClient()
	realClient.SkipWebhookURLValidationOnSend(true)
	realClient.SetHTTPClient(srv.Client())
	ch.client = realClient
	ch.sendTimeout = testTimeout

	start := time.Now()
	err := ch.Send(context.Background(), bus.OutboundMessage{Content: "hello"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected Send to fail: the server sleeps past the configured timeout")
	}
	if elapsed >= serverDelay {
		t.Errorf("Send took %v, want it bounded well under the %v server delay by a %v timeout", elapsed, serverDelay, testTimeout)
	}
}

// TestEffectiveSendTimeout_DefaultsWhenUnset asserts the zero-value channel
// (as produced by the constructor, which never sets sendTimeout) falls back
// to defaultSendTimeout rather than an unbounded send.
func TestEffectiveSendTimeout_DefaultsWhenUnset(t *testing.T) {
	ch := channelWith(t, map[string]config.TeamsWebhookTarget{
		"default": {WebhookURL: *config.NewSecureString("https://example.invalid/hook")},
	})
	if got := ch.effectiveSendTimeout(); got != defaultSendTimeout {
		t.Errorf("effectiveSendTimeout() = %v, want default %v", got, defaultSendTimeout)
	}
	ch.sendTimeout = 5 * time.Second
	if got := ch.effectiveSendTimeout(); got != 5*time.Second {
		t.Errorf("effectiveSendTimeout() = %v, want overridden 5s", got)
	}
}
