package channels

import (
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// stubFactory swaps the factory registered under name for one that counts calls
// and builds a mock channel, and restores the previous factory afterwards.
func stubFactory(t *testing.T, name string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32

	factoriesMu.Lock()
	prev, had := factories[name]
	factories[name] = func(*config.Config, *bus.MessageBus) (Channel, error) {
		calls.Add(1)
		return &mockChannel{BaseChannel: *NewBaseChannel(name, nil, nil, nil)}, nil
	}
	factoriesMu.Unlock()

	t.Cleanup(func() {
		factoriesMu.Lock()
		defer factoriesMu.Unlock()
		if had {
			factories[name] = prev
		} else {
			delete(factories, name)
		}
	})
	return &calls
}

func telegramConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels.Telegram.Enabled = true
	cfg.Channels.Telegram.Token = *config.NewSecureString("123456:fake-token")
	return cfg
}

func TestInitChannel_LockedOnlySimchat(t *testing.T) {
	telegramCalls := stubFactory(t, "telegram")
	simchatCalls := stubFactory(t, "simchat")

	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)
	if err := sandbox.Lock(sandbox.LockOptions{Token: "t-0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatal(err)
	}

	m, err := NewManager(telegramConfig(), bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if telegramCalls.Load() != 0 {
		t.Fatal("the telegram factory ran under the sandbox lock")
	}
	// simchat is the one channel the lock lets through, and a locked manager builds it.
	if simchatCalls.Load() != 1 {
		t.Fatalf("simchat factory calls = %d, want 1", simchatCalls.Load())
	}
	if got := m.GetEnabledChannels(); len(got) != 1 || got[0] != "simchat" {
		t.Fatalf("channels built under the lock = %v, want [simchat]", got)
	}
}

func TestInitChannels_UnlockedNeverBuildsSimchat(t *testing.T) {
	simchatCalls := stubFactory(t, "simchat")
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	if _, err := NewManager(config.DefaultConfig(), bus.NewMessageBus(), nil); err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if simchatCalls.Load() != 0 {
		t.Fatal("an unlocked manager built the simchat channel")
	}
}

func TestInitChannel_UnlockedBuildsTelegram(t *testing.T) {
	telegramCalls := stubFactory(t, "telegram")
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	m, err := NewManager(telegramConfig(), bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if telegramCalls.Load() != 1 {
		t.Fatalf("telegram factory calls = %d, want 1 when unlocked", telegramCalls.Load())
	}
	if got := m.GetEnabledChannels(); len(got) != 1 || got[0] != "telegram" {
		t.Fatalf("enabled channels = %v, want [telegram]", got)
	}
}
