package supervisor

import (
	"strings"
	"testing"
)

const (
	localModel = "openai//models/local-26b"
	localBase  = "http://127.0.0.1:8080/v1"
)

// localShell is the environment of a starter that has no key and lists the local model.
var localShell = map[string]string{"KQ_SANDBOX_LLM_MODELS": localModel}

// A loopback base with the openai protocol needs no KQ_SANDBOX_LLM_KEY: the instance gets the
// fixed placeholder, the one host opened is the loopback host, and a token budget needs no
// price.
func TestLocalModelNeedsNoKeyAndOpensOnlyLoopback(t *testing.T) {
	o := UpOptions{LLM: "live", LLMModel: localModel, LLMBudget: "20000000tok", LLMAPIBase: localBase}
	e := env(t, o, localShell)
	for k, want := range map[string]string{
		"KQ_SANDBOX_LLM": "live", "KQ_SANDBOX_LLM_MODEL": localModel, "KQ_SANDBOX_LLM_BUDGET": "20000000tok",
		"KQ_SANDBOX_LLM_KEY": "sk-sandbox-local-no-key", "KQ_SANDBOX_ALLOW_HOSTS": "127.0.0.1",
		"KQ_SANDBOX_LLM_API_BASE": localBase, "KQ_SANDBOX_LLM_CALL_TIMEOUT": "3m0s",
	} {
		if e[k] != want {
			t.Errorf("%s = %q, want %q", k, e[k], want)
		}
	}
	if _, ok := e["KQ_SANDBOX_LLM_PRICES"]; ok {
		t.Error("no price table was given and none may appear")
	}
}

// A real key in the shell never reaches a local server: the placeholder is sent whatever
// the starter holds.
func TestLocalModelNeverReceivesTheRealKey(t *testing.T) {
	sh := map[string]string{"KQ_SANDBOX_LLM_MODELS": localModel, "KQ_SANDBOX_LLM_KEY": testKey}
	e := env(t, UpOptions{LLM: "live", LLMModel: localModel, LLMBudget: "1000tok", LLMAPIBase: localBase}, sh)
	if e["KQ_SANDBOX_LLM_KEY"] == testKey || strings.Contains(strings.Join(mapValues(e), "\n"), testKey) {
		t.Errorf("the real key reached an instance that talks to a local server: %v", e)
	}
	for _, host := range []string{"http://localhost:8080/v1", "http://[::1]:8080/v1"} {
		e = env(t, UpOptions{LLM: "live", LLMModel: localModel, LLMBudget: "1000tok", LLMAPIBase: host}, localShell)
		if want := strings.Trim(strings.TrimPrefix(strings.SplitN(host, ":8080", 2)[0], "http://"), "[]"); e["KQ_SANDBOX_ALLOW_HOSTS"] != want {
			t.Errorf("%s: allow hosts = %q, want %q", host, e["KQ_SANDBOX_ALLOW_HOSTS"], want)
		}
	}
}

// Everything else is refused before anything starts: a remote host, a non-http URL, a base
// with user info, and a key-free start that is not openai-on-loopback. A remote start still
// needs its key.
func TestLocalBaseRefusals(t *testing.T) {
	base := UpOptions{LLM: "live", LLMModel: localModel, LLMBudget: "1000tok", LLMAPIBase: localBase}
	for name, c := range map[string]struct {
		mut  func(*UpOptions)
		sh   map[string]string
		want string
	}{
		"a remote https host":    {func(o *UpOptions) { o.LLMAPIBase = "https://api.openai.com/v1" }, localShell, "loopback"},
		"a remote host with key": {func(o *UpOptions) { o.LLMAPIBase = "https://llm.example.com/v1" }, map[string]string{"KQ_SANDBOX_LLM_MODELS": localModel, "KQ_SANDBOX_LLM_KEY": testKey}, "loopback"},
		"a file URL":             {func(o *UpOptions) { o.LLMAPIBase = "file:///etc/passwd" }, localShell, "http or https"},
		"an empty host":          {func(o *UpOptions) { o.LLMAPIBase = "http:///v1" }, localShell, "loopback"},
		"user info":              {func(o *UpOptions) { o.LLMAPIBase = "http://127.0.0.1@evil.example/v1" }, localShell, "user info"},
		"anthropic on loopback":  {func(o *UpOptions) { o.LLMModel = "anthropic-messages/claude-sonnet-4.6" }, map[string]string{"KQ_SANDBOX_LLM_MODELS": "anthropic-messages/claude-sonnet-4.6"}, "KQ_SANDBOX_LLM_KEY"},
		"no base, no key":        {func(o *UpOptions) { o.LLMAPIBase = "" }, localShell, "KQ_SANDBOX_LLM_KEY"},
		"a model off the list":   {func(o *UpOptions) { o.LLMModel = "openai//models/other" }, localShell, "KQ_SANDBOX_LLM_MODELS"},
		"no budget":              {func(o *UpOptions) { o.LLMBudget = "" }, localShell, "budget"},
		"a bad call timeout":     {func(o *UpOptions) { o.LLMCallTimeout = "fast" }, localShell, "timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			c.mut(&o)
			_, err := o.childEnvOf("n", shell(c.sh))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("the key is in the error: %v", err)
			}
		})
	}
	// The call timeout and the base belong to record and live only.
	for _, o := range []UpOptions{{LLMCallTimeout: "30s"}, {LLM: "replay", LLMAPIBase: localBase}} {
		if _, err := o.childEnvOf("n", shell(localShell)); err == nil {
			t.Errorf("%+v must be refused", o)
		}
	}
}

// The call timeout reaches the instance as a validated duration.
func TestCallTimeoutReachesTheInstance(t *testing.T) {
	e := env(t, UpOptions{LLM: "live", LLMModel: localModel, LLMBudget: "1000tok", LLMAPIBase: localBase, LLMCallTimeout: "45s"}, localShell)
	if e["KQ_SANDBOX_LLM_CALL_TIMEOUT"] != "45s" {
		t.Errorf("timeout = %q", e["KQ_SANDBOX_LLM_CALL_TIMEOUT"])
	}
}

// The option appears as a flag and as an MCP argument without any other file naming it.
func TestLLMAPIBaseIsAGeneratedOption(t *testing.T) {
	found := map[string]string{}
	for _, f := range UpOptionFields() {
		found[f.Flag] = f.JSON
	}
	for flag, js := range map[string]string{"llm-api-base": "llm_api_base", "llm-call-timeout": "llm_call_timeout"} {
		if found[flag] != js {
			t.Errorf("flag --%s -> %q, want MCP argument %q", flag, found[flag], js)
		}
	}
}
