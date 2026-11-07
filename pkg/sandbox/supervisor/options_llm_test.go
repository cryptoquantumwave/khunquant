package supervisor

import (
	"strings"
	"testing"
)

const (
	testKey   = "sk-test-SENTINEL-0123456789-not-a-real-key"
	liveModel = "anthropic-messages/claude-sonnet-4.6"
)

// shell is the environment of the starter: a test's own, never the developer's.
func shell(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var liveShell = map[string]string{
	"KQ_SANDBOX_LLM_KEY": testKey, "KQ_SANDBOX_LLM_MODELS": "claude-sonnet-4.6,other", "KQ_SANDBOX_LLM_PRICES": "other=1:2",
	// A real key in the shell must never be consulted.
	"ANTHROPIC_API_KEY": "sk-shell-MUST-NEVER-BE-USED", "OPENAI_API_KEY": "sk-shell-MUST-NEVER-BE-USED",
}

func env(t *testing.T, o UpOptions, sh map[string]string) map[string]string {
	t.Helper()
	e, err := o.childEnvOf("sbx-name", shell(sh))
	if err != nil {
		t.Fatalf("childEnvOf: %v", err)
	}
	return e
}

func TestChildEnvScriptedAndReplayOpenNothing(t *testing.T) {
	for _, mode := range []string{"", "scripted", "replay"} {
		e := env(t, UpOptions{LLM: mode}, liveShell)
		for k := range e {
			if strings.HasPrefix(k, "KQ_SANDBOX_LLM_") || k == "KQ_SANDBOX_ALLOW_HOSTS" {
				t.Errorf("mode %q: %s reached the child", mode, k)
			}
		}
		if mode != "" && e["KQ_SANDBOX_LLM"] != mode {
			t.Errorf("mode %q: KQ_SANDBOX_LLM = %q", mode, e["KQ_SANDBOX_LLM"])
		}
		if strings.Contains(strings.Join(mapValues(e), "\n"), testKey) {
			t.Errorf("mode %q: the key reached a child that has no live model", mode)
		}
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestChildEnvLiveSetsExactlyWhatItNeeds(t *testing.T) {
	e := env(t, UpOptions{LLM: "live", LLMModel: liveModel, LLMBudget: "2USD"}, liveShell)
	want := map[string]string{
		"KQ_SANDBOX_LLM": "live", "KQ_SANDBOX_LLM_MODEL": liveModel, "KQ_SANDBOX_LLM_BUDGET": "2USD",
		"KQ_SANDBOX_LLM_KEY": testKey, "KQ_SANDBOX_LLM_MODELS": "claude-sonnet-4.6,other",
		"KQ_SANDBOX_LLM_PRICES": "other=1:2", "KQ_SANDBOX_ALLOW_HOSTS": "api.anthropic.com",
	}
	for k, v := range want {
		if e[k] != v {
			t.Errorf("%s = %q, want %q", k, e[k], v)
		}
	}
	for k, v := range e {
		if strings.Contains(v, "SHELL-MUST-NEVER") {
			t.Errorf("%s carries a key from the shell", k)
		}
	}
	// The loopback fake of a test: its host is the one opened.
	e = env(t, UpOptions{LLM: "record", LLMModel: "openai/gpt-x", LLMBudget: "1000tok", LLMAPIBase: "http://127.0.0.1:5555/v1"},
		map[string]string{"KQ_SANDBOX_LLM_KEY": testKey, "KQ_SANDBOX_LLM_MODELS": "gpt-x"})
	if e["KQ_SANDBOX_ALLOW_HOSTS"] != "127.0.0.1" || e["KQ_SANDBOX_LLM_API_BASE"] != "http://127.0.0.1:5555/v1" {
		t.Errorf("record env = %v", e)
	}
}

func TestLiveStartIsRefusedBeforeAnythingStarts(t *testing.T) {
	ok := UpOptions{LLM: "live", LLMModel: liveModel, LLMBudget: "2USD"}
	for name, c := range map[string]struct {
		mut  func(*UpOptions)
		sh   map[string]string
		want string
	}{
		"no budget":             {func(o *UpOptions) { o.LLMBudget = "" }, liveShell, "budget"},
		"a zero budget":         {func(o *UpOptions) { o.LLMBudget = "0USD" }, liveShell, "budget"},
		"a budget with no unit": {func(o *UpOptions) { o.LLMBudget = "5" }, liveShell, "budget"},
		"a CLI model":           {func(o *UpOptions) { o.LLMModel = "claude-cli/sonnet" }, liveShell, "not allowed"},
		"no inner model":        {func(o *UpOptions) { o.LLMModel = "" }, liveShell, "protocol"},
		"a model off the list":  {func(o *UpOptions) { o.LLMModel = "anthropic-messages/unlisted" }, liveShell, "KQ_SANDBOX_LLM_MODELS"},
		"no key":                {func(*UpOptions) {}, map[string]string{"KQ_SANDBOX_LLM_MODELS": "claude-sonnet-4.6"}, "KQ_SANDBOX_LLM_KEY"},
		"a cleartext base":      {func(o *UpOptions) { o.LLMAPIBase = "http://api.example.com/v1" }, liveShell, "https"},
		"an unknown mode":       {func(o *UpOptions) { o.LLM = "turbo" }, liveShell, "not scripted"},
	} {
		t.Run(name, func(t *testing.T) {
			o := ok
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
	// A model and a budget do not belong to the other modes.
	for _, o := range []UpOptions{{LLMModel: liveModel}, {LLM: "replay", LLMBudget: "1USD"}, {LLM: "scripted", LLMAPIBase: "http://127.0.0.1/v1"}} {
		if _, err := o.childEnvOf("n", shell(liveShell)); err == nil {
			t.Errorf("%+v must be refused", o)
		}
	}
}

// The egress opening cannot be asked for through Env or any option: it is derived from the
// validated model, and the variables are not AllowEnv-ed.
func TestAllowHostsAndKeyCannotComeFromEnv(t *testing.T) {
	for _, k := range []string{"KQ_SANDBOX_ALLOW_HOSTS", "KQ_SANDBOX_LLM_KEY", "KQ_SANDBOX_LLM", "KQ_SANDBOX_SEED", "KQ_SANDBOX_LLM_API_BASE", "KQ_SANDBOX_LLM_BUDGET"} {
		if err := validEnv(map[string]string{k: "x"}); err == nil {
			t.Errorf("%s must not be accepted in UpOptions.Env", k)
		}
	}
}

func TestSeedOfInstance(t *testing.T) {
	a, _ := seedOf("", "sbx-one")
	b, _ := seedOf("", "sbx-one")
	c, _ := seedOf("", "sbx-two")
	if a == 0 || a != b || a == c || a < 0 {
		t.Errorf("derived seeds: %d %d %d", a, b, c)
	}
	if s, err := seedOf("77", "n"); err != nil || s != 77 {
		t.Errorf("explicit = %d, %v", s, err)
	}
	if _, err := seedOf("seven", "n"); err == nil {
		t.Error("a non-integer seed must be refused")
	}
	if e := env(t, UpOptions{Seed: "0"}, nil); e["KQ_SANDBOX_SEED"] != "" {
		t.Errorf("--seed 0 must leave the nonces random: %v", e)
	}
	if e := env(t, UpOptions{}, nil); e["KQ_SANDBOX_SEED"] == "" {
		t.Error("an instance with no --seed derives one from its name")
	}
	if e := env(t, UpOptions{Seed: "5"}, nil); e["KQ_SANDBOX_SEED"] != "5" {
		t.Errorf("--seed 5 = %v", e)
	}
}
