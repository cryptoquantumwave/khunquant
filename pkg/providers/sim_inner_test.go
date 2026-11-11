package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim/simtest"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/egress"
)

// A sentinel and not a key: these tests never read or set a real API key, and they blank
// the variables a provider could fall back to, so a developer's shell cannot leak in.
const (
	testKey   = "sk-test-SENTINEL-0123456789-not-a-real-key"
	shellKey  = "sk-shell-MUST-NEVER-BE-USED"
	testModel = "test-model"
	// testAllow is the allowlist: the test model under every protocol a live model may use.
	testAllow = "anthropic-messages/" + testModel + ",openai/" + testModel + ",openrouter/" + testModel
)

func liveEnv(t *testing.T, base string, over map[string]string) {
	t.Helper()
	for k, v := range map[string]string{
		sandbox.EnvLLMKey: testKey, sandbox.EnvLLMModels: testAllow, sandbox.EnvLLMBudget: "2USD",
		sandbox.EnvLLMPrices: testModel + "=3:15", sandbox.EnvLLMAPIBase: base,
		"ANTHROPIC_API_KEY": shellKey, "OPENAI_API_KEY": shellKey, "OPENROUTER_API_KEY": shellKey,
	} {
		if o, ok := over[k]; ok {
			v = o
		}
		t.Setenv(k, v)
	}
}

func attachLive(t *testing.T, mode string) *sim.Engine {
	t.Helper()
	e, err := sim.NewEngine(sim.EngineOptions{Mode: mode, BudgetUSD: 2,
		TurnInfo: func(context.Context) sim.CallInfo { return sim.CallInfo{Turn: "user", TurnID: "t"} }})
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.Attach(e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sim.Detach(e) })
	return e
}

func chatOnce(p LLMProvider) (*protocoltypes.LLMResponse, error) {
	msgs := []protocoltypes.Message{{Role: "system", Content: "S"}, {Role: "user", Content: "hello"}}
	return p.Chat(context.Background(), msgs, nil, "ignored", map[string]any{"prompt_cache_key": "main", "max_tokens": 50})
}

func TestLiveReachesTheInnerModelWithTheKeyOnly(t *testing.T) {
	for _, protocol := range []string{"anthropic-messages", "openai", "openrouter"} {
		t.Run(protocol, func(t *testing.T) {
			fake := simtest.New(t)
			liveEnv(t, fake.Base(), nil)
			lockSandboxForTest(t, "127.0.0.1")
			attachLive(t, sim.ModeLive)

			p, id, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/live:" + protocol + "/" + testModel})
			if err != nil || id != "live:"+protocol+"/"+testModel {
				t.Fatalf("CreateProviderFromConfig = (%v, %q, %v)", p, id, err)
			}
			resp, err := chatOnce(p)
			if err != nil || resp.Content != "fake reply" {
				t.Fatalf("Chat = %v, %v", resp, err)
			}
			reqs := fake.Requests()
			if len(reqs) != 1 {
				t.Fatalf("the fake saw %d requests", len(reqs))
			}
			got := reqs[0].Header.Get("X-Api-Key") + reqs[0].Header.Get("Authorization")
			if !strings.Contains(got, testKey) || strings.Contains(got, shellKey) {
				t.Errorf("credentials sent = %q: want the sandbox key, never a shell one", got)
			}
			if !strings.Contains(string(reqs[0].Body), testModel) {
				t.Errorf("the inner model id was not sent: %s", reqs[0].Body)
			}
		})
	}
}

func TestLiveRefusesCLIAndUnlistedModel(t *testing.T) {
	fake := simtest.New(t)
	for name, c := range map[string]struct {
		ref  string
		env  map[string]string
		want string
	}{
		"claude-cli":           {"sim/live:claude-cli/x", nil, "not allowed"},
		"codex-cli":            {"sim/record:codex-cli/x", nil, "not allowed"},
		"a local daemon":       {"sim/live:ollama/x", nil, "not allowed"},
		"azure":                {"sim/live:azure/x", nil, "not allowed"},
		"no protocol":          {"sim/live:" + testModel, nil, "<protocol>/<model>"},
		"no inner at all":      {"sim/live", nil, "<protocol>/<model>"},
		"a model off the list": {"sim/live:openai/other-model", nil, "KQ_SANDBOX_LLM_MODELS"},
		"an empty allowlist":   {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMModels: ""}, "KQ_SANDBOX_LLM_MODELS"},
		// The protocol picks the host the key goes to: a model allowed under one protocol is
		// not allowed under another (192-1), and a bare entry allows nothing.
		"the model under another protocol": {"sim/live:openrouter/" + testModel, map[string]string{sandbox.EnvLLMModels: "anthropic-messages/" + testModel}, "KQ_SANDBOX_LLM_MODELS"},
		"a bare allowlist entry":           {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMModels: testModel}, "without its protocol"},
		// 192-1 as it was: a bare entry (main's documented form) let every protocol through.
		"a bare entry and another protocol": {"sim/live:openrouter/" + testModel, map[string]string{sandbox.EnvLLMModels: testModel}, "without its protocol"},
		"no key":                            {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMKey: ""}, "KQ_SANDBOX_LLM_KEY"},
		"no budget":                         {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMBudget: ""}, "without a budget"},
		"a zero budget":                     {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMBudget: "0USD"}, "positive"},
		"a model with no price":             {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMPrices: ""}, "no price"},
		"an http non-loopback base":         {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMAPIBase: "http://api.example.com/v1"}, "https"},
		"a host the lock does not allow":    {"sim/live:openai/" + testModel, map[string]string{sandbox.EnvLLMAPIBase: "https://api.example.com/v1"}, "one host the sandbox lock allows"},
	} {
		t.Run(name, func(t *testing.T) {
			liveEnv(t, fake.Base(), c.env)
			lockSandboxForTest(t, "127.0.0.1")
			p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: c.ref})
			if err == nil || p != nil {
				t.Fatalf("%s must be refused: (%v, %v)", c.ref, p, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the key is in the error: %v", err)
			}
		})
	}
}

// The lock opens exactly one host. A second allowed host, or none, refuses the provider.
func TestLiveNeedsExactlyTheInnerHost(t *testing.T) {
	fake := simtest.New(t)
	liveEnv(t, fake.Base(), nil)
	for _, allow := range [][]string{nil, {"127.0.0.1", "other.example"}, {"other.example"}} {
		lockSandboxForTest(t, allow...)
		if _, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/live:openai/" + testModel}); err == nil {
			t.Errorf("AllowHosts %v must refuse the provider", allow)
		}
	}
}

type countingTransport struct {
	next  http.RoundTripper
	count atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.next.RoundTrip(r)
}

// TestLiveInnerUsesGuardedTransport: whatever sits on http.DefaultTransport (in a locked
// process, the egress guard) sees the inner provider's requests; none of the three clients
// carries a transport of its own.
func TestLiveInnerUsesGuardedTransport(t *testing.T) {
	for _, protocol := range []string{"anthropic-messages", "openai", "openrouter"} {
		t.Run(protocol, func(t *testing.T) {
			fake := simtest.New(t)
			liveEnv(t, fake.Base(), nil)
			lockSandboxForTest(t, "127.0.0.1")
			attachLive(t, sim.ModeLive)
			stub := &countingTransport{next: http.DefaultTransport}
			orig := http.DefaultTransport
			http.DefaultTransport = stub
			t.Cleanup(func() { http.DefaultTransport = orig })

			p, _, err := CreateProviderFromConfig(&config.ModelConfig{ModelName: "sim", Model: "sim/live:" + protocol + "/" + testModel})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := chatOnce(p); err != nil {
				t.Fatal(err)
			}
			if stub.count.Load() != 1 {
				t.Errorf("the guard on http.DefaultTransport saw %d requests, want 1", stub.count.Load())
			}
		})
	}
}

// In a live instance the egress guard opens the inner host and nothing else; every other
// host fails with ErrBlocked before any lookup. (The guard's own tests cover the allowed side.)
func TestLiveInstanceStillBlocksEveryOtherHost(t *testing.T) {
	fake := simtest.New(t)
	liveEnv(t, fake.Base(), nil)
	lockSandboxForTest(t, "127.0.0.1")
	egress.ResetForTest()
	t.Cleanup(egress.ResetForTest)
	lk, _ := sandbox.LockInfo()
	if err := egress.Install(egress.Policy{AllowHosts: lk.AllowHosts}); err != nil {
		t.Fatal(err)
	}
	if got := egress.Current().AllowHosts; len(got) != 1 || got[0] != "127.0.0.1" {
		t.Fatalf("policy = %v", got)
	}
	for _, target := range []string{"http://api.anthropic.com/", "http://api.openai.com/", "http://evil.example/", "http://203.0.113.7/", "http://[2001:db8::1]/"} {
		_, err := http.Get(target) //nolint:noctx // the guard refuses before any connection
		if !errors.Is(err, egress.ErrBlocked) {
			t.Errorf("GET %s = %v, want egress.ErrBlocked", target, err)
		}
	}
}

// Every protocol a live instance may use has the default API base the factory itself uses.
func TestLLMDefaultBasesMatchTheFactory(t *testing.T) {
	for protocol, base := range sandbox.LLMDefaultBase {
		want := getDefaultAPIBase(protocol)
		if protocol == "anthropic-messages" {
			want = "https://api.anthropic.com/v1" // a literal in CreateProviderFromConfig
		}
		if base != want {
			t.Errorf("%s: sandbox.LLMDefaultBase %q, factory %q", protocol, base, want)
		}
	}
}
