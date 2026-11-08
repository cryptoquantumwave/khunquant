package gateway

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/randsrc"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

func TestApplyLLMMode(t *testing.T) {
	mk := func() *config.Config {
		return &config.Config{ModelList: []config.ModelConfig{{ModelName: "sim", Model: "sim/scripted"}}}
	}
	cfg := mk()
	if err := applyLLMMode(cfg, ""); err != nil || cfg.ModelList[0].Model != "sim/scripted" {
		t.Fatalf("unset: %v, %q", err, cfg.ModelList[0].Model)
	}
	if err := applyLLMMode(cfg, "replay"); err != nil || cfg.ModelList[0].Model != "sim/replay" {
		t.Fatalf("replay: %v, %q", err, cfg.ModelList[0].Model)
	}
	for _, bad := range []string{"live", "record", "REPLAY", "sim/replay"} {
		if err := applyLLMMode(mk(), bad); err == nil {
			t.Errorf("mode %q must be refused by this build", bad)
		}
	}
	if got := simModeOf(cfg); got != sim.ModeReplay {
		t.Errorf("simModeOf = %q", got)
	}
	if got := simModeOf(&config.Config{}); got != sim.ModeScripted {
		t.Errorf("simModeOf of no sim model = %q", got)
	}
}

func TestSeedFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == envSeed {
				return v
			}
			return ""
		}
	}
	sandbox.ResetLockForTest()
	t.Cleanup(sandbox.ResetLockForTest)

	// Unlocked: a seed is refused, so an unlocked process can never get a seeded source.
	if err := seedFromEnv(env("5")); err == nil || !strings.Contains(err.Error(), "sandbox-locked") {
		t.Errorf("unlocked seed = %v", err)
	}
	for _, v := range []string{"", "0"} {
		if err := seedFromEnv(env(v)); err != nil {
			t.Errorf("seed %q: %v", v, err)
		}
	}
	if err := seedFromEnv(env("abc")); err == nil {
		t.Error("a non-integer seed must be refused")
	}

	if err := sandbox.Lock(sandbox.LockOptions{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := seedFromEnv(env("12345")); err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = randsrc.Read(a)
	sandbox.ResetLockForTest()
	if err := sandbox.Lock(sandbox.LockOptions{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	if err := seedFromEnv(env("12345")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 16)
	_, _ = randsrc.Read(b)
	if string(a) != string(b) {
		t.Error("the same seed under the lock must give the same stream")
	}
}
