package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// A press goes through the real control router of the running gateway (not a stub),
// whoever taps: a persona that is not the owner is told so by the control path itself.
func TestLabChatPressReachesRealControlPath(t *testing.T) {
	f := newLockedFixture(t, func(c *config.Config) {
		c.Proactive.Owner = config.OwnerConfig{Channel: "simchat", ChatID: "dm:owner", SenderID: "simchat:owner"}
	})
	chatURL := f.url("/api/sandbox/lab/chat")
	for _, chat := range []string{"group:team", "dm:owner"} {
		err := f.bus.PublishOutbound(context.Background(), bus.OutboundMessage{
			Channel: "simchat", ChatID: chat, Content: "BTC moved 5%", InterventionID: "I-9", Severity: proactive.SeverityWarning,
			Actions: []bus.Action{{ID: "mute", Label: "Mute", Payload: "I-9", TextCommand: "/mute price_move"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		waitChatOutbox(t, chatURL, chat, 1)
	}

	press := func(as, chat string) map[string]any {
		body := `{"op":"press","as":"` + as + `","on":{"to":"` + chat + `","severity":"warning","contains":["BTC"]},"action":"mute"}`
		code, resp, err := postJSON(chatURL, lockTestToken, body)
		if err != nil || code != http.StatusOK {
			t.Fatalf("POST press as %s = %d (%v) %s", as, code, err, resp)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(resp), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// alice passes simchat's allow-list (allow_from is *), so "Not authorized." is the
	// control path refusing a non-owner; the owner's tap reaches its mute handler, which
	// answers that proactive features are off in this config.
	alice := press("alice", "group:team")
	owner := press("owner", "dm:owner")
	if alice["toast"] != "Not authorized." || !strings.Contains(owner["toast"].(string), "Proactive features are off") {
		t.Fatalf("taps answered by %q and %q, want the control path's refusal and its mute handler's answer", alice["toast"], owner["toast"])
	}
	if code, body, err := postJSON(chatURL, lockTestToken, `{"op":"react","as":"owner","on":{"to":"dm:owner"},"emoji":"up"}`); err != nil ||
		code != http.StatusOK || !strings.Contains(body, `"allowed":true`) {
		t.Fatalf("react = %d (%v) %s", code, err, body)
	}
}
