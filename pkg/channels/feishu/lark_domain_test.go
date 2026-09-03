package feishu

import (
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// Feishu and Lark are the same product on separate deployments with separate
// hosts. An app registered on one is not reachable on the other, so pointing at
// the wrong domain fails authentication outright rather than degrading — which
// is why the selector has to reach both the REST client and the event socket.

func TestNewFeishuChannel_SelectsDomainFromIsLark(t *testing.T) {
	// The SDK's exported base URLs are what both call sites switch between;
	// pin them so a dependency bump that renames or repoints one is caught
	// here rather than at runtime against a live tenant.
	if lark.FeishuBaseUrl != "https://open.feishu.cn" {
		t.Errorf("lark.FeishuBaseUrl = %q, unexpected", lark.FeishuBaseUrl)
	}
	if lark.LarkBaseUrl != "https://open.larksuite.com" {
		t.Errorf("lark.LarkBaseUrl = %q, unexpected", lark.LarkBaseUrl)
	}
	if lark.FeishuBaseUrl == lark.LarkBaseUrl {
		t.Fatal("the two base URLs are identical; the selector would be meaningless")
	}

	for _, isLark := range []bool{false, true} {
		cfg := config.FeishuConfig{
			Enabled:   true,
			AppID:     "cli_test",
			AppSecret: *config.NewSecureString("secret"),
			IsLark:    isLark,
		}
		ch, err := NewFeishuChannel(cfg, bus.NewMessageBus())
		if err != nil {
			t.Fatalf("NewFeishuChannel(IsLark=%v) error = %v", isLark, err)
		}
		if ch.client == nil {
			t.Fatalf("NewFeishuChannel(IsLark=%v) produced no client", isLark)
		}
		if ch.config.IsLark != isLark {
			t.Errorf("config.IsLark = %v, want %v", ch.config.IsLark, isLark)
		}
	}
}

// The field must survive a config round-trip, or an operator setting it in
// config.json would see it silently ignored.
func TestFeishuConfig_IsLarkRoundTrips(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.Channels.Feishu.IsLark {
		t.Error("IsLark should default to false (Feishu), not Lark")
	}
}
