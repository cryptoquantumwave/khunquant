package gateway

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestLockAllowHostsAreExact: the egress opening is the inner model's API host, for a
// record or live instance and for nothing else.
func TestLockAllowHostsAreExact(t *testing.T) {
	const model = "anthropic-messages/claude-sonnet-4.6"
	live := map[string]string{sandbox.EnvLLM: "live", sandbox.EnvLLMModel: model, sandbox.EnvAllowHosts: "api.anthropic.com"}
	got, err := lockAllowHosts(envOf(live))
	if err != nil || len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Fatalf("live = %v, %v", got, err)
	}
	rec := map[string]string{sandbox.EnvLLM: "record", sandbox.EnvLLMModel: "openai/gpt-x", sandbox.EnvAllowHosts: " API.OPENAI.COM "}
	if got, err = lockAllowHosts(envOf(rec)); err != nil || len(got) != 1 {
		t.Fatalf("record = %v, %v", got, err)
	}

	// scripted and replay open nothing, and refuse an opening someone asked for.
	for _, mode := range []string{"", "scripted", "replay", "bogus"} {
		if got, err := lockAllowHosts(envOf(map[string]string{sandbox.EnvLLM: mode})); err != nil || len(got) != 0 {
			t.Errorf("mode %q without hosts = %v, %v", mode, got, err)
		}
		in := map[string]string{sandbox.EnvLLM: mode, sandbox.EnvLLMModel: model, sandbox.EnvAllowHosts: "api.anthropic.com"}
		if _, err := lockAllowHosts(envOf(in)); err == nil || !strings.Contains(err.Error(), "record or live") {
			t.Errorf("mode %q with an allowed host must be refused: %v", mode, err)
		}
	}
	// A live instance gets exactly the model's host: not another, not two, not none.
	for name, allow := range map[string]string{
		"another host": "evil.example", "the host and one more": "api.anthropic.com,evil.example", "none": "", "a prefix": "api.anthropic.com.evil.example",
	} {
		in := map[string]string{sandbox.EnvLLM: "live", sandbox.EnvLLMModel: model, sandbox.EnvAllowHosts: allow}
		if got, err := lockAllowHosts(envOf(in)); err == nil {
			t.Errorf("%s: %v must be refused", name, got)
		}
	}
	// A CLI or unknown protocol has no host to allow.
	for _, m := range []string{"claude-cli/x", "ollama/x", "", "x"} {
		in := map[string]string{sandbox.EnvLLM: "live", sandbox.EnvLLMModel: m, sandbox.EnvAllowHosts: "localhost"}
		if _, err := lockAllowHosts(envOf(in)); err == nil {
			t.Errorf("inner model %q must be refused", m)
		}
	}
	// The loopback base the tests use is the loopback host, and an http non-loopback one is refused.
	lo := map[string]string{sandbox.EnvLLM: "live", sandbox.EnvLLMModel: model, sandbox.EnvLLMAPIBase: "http://127.0.0.1:4321/v1", sandbox.EnvAllowHosts: "127.0.0.1"}
	if got, err := lockAllowHosts(envOf(lo)); err != nil || got[0] != "127.0.0.1" {
		t.Errorf("loopback base = %v, %v", got, err)
	}
	lo[sandbox.EnvLLMAPIBase] = "http://api.example.com/v1"
	lo[sandbox.EnvAllowHosts] = "api.example.com"
	if _, err := lockAllowHosts(envOf(lo)); err == nil {
		t.Error("a cleartext non-loopback base must be refused")
	}
}

// TestReplayAndLiveInstancesEgress locks a process the way a gateway does, then checks
// what its egress guard allows: a replay (or scripted) instance allows no host beyond
// loopback at all, a live one only the inner model's host, and both block the rest.
func TestReplayAndLiveInstancesEgress(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		want []string
	}{
		"scripted": {map[string]string{}, nil},
		"replay":   {map[string]string{sandbox.EnvLLM: "replay"}, nil},
		"live": {map[string]string{sandbox.EnvLLM: "live", sandbox.EnvLLMModel: "openai/gpt-x",
			sandbox.EnvLLMAPIBase: "https://llm.example.test/v1", sandbox.EnvAllowHosts: "llm.example.test"}, []string{"llm.example.test"}},
	} {
		t.Run(name, func(t *testing.T) {
			home := newInstanceHome(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			lk := lockProcessForTest(t, home)
			if len(lk.AllowHosts) != len(c.want) || len(egress.Current().AllowHosts) != len(c.want) {
				t.Fatalf("lock allows %v, guard allows %v, want %v", lk.AllowHosts, egress.Current().AllowHosts, c.want)
			}
			for _, h := range c.want {
				if !slices.Contains(egress.Current().AllowHosts, h) {
					t.Errorf("guard does not allow %s", h)
				}
			}
			for _, target := range []string{"http://api.anthropic.com/", "http://api.openai.com/", "http://evil.example/", "http://203.0.113.7/"} {
				if _, err := http.Get(target); !errors.Is(err, egress.ErrBlocked) { //nolint:noctx // refused before connecting
					t.Errorf("GET %s = %v, want egress.ErrBlocked", target, err)
				}
			}
		})
	}
}
