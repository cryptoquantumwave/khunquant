package sandbox

import (
	"strings"
	"testing"
	"time"
)

// The API base of a key-free run is loopback only: it is the one host the instance may reach,
// and with the openai protocol it is sent a placeholder, so a remote host must never be
// nameable there.
func TestLLMAPIBaseLoopback(t *testing.T) {
	for _, ok := range []string{
		"http://localhost:8080/v1", "http://127.0.0.1:8080/v1", "http://[::1]:8080/v1",
		"https://localhost/v1", "HTTP://LOCALHOST:1/v1", " http://127.0.0.1:8080/v1 ",
	} {
		if err := LLMAPIBaseLoopback(ok); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
	for base, want := range map[string]string{
		"http://api.openai.com/v1":         "loopback",
		"https://api.example.com/v1":       "loopback",
		"http://127.0.0.2:8080/v1":         "loopback", // only the three names, not all of 127/8
		"http://localhost.evil.example/v1": "loopback",
		"http://127.0.0.1@evil.example/v1": "user info",
		"http://user:pw@localhost:8080/v1": "user info",
		"file:///etc/passwd":               "http or https",
		"file://localhost/etc/passwd":      "http or https",
		"":                                 "http or https",
		"localhost:8080/v1":                "http or https", // no scheme: not guessed
		"//localhost:8080/v1":              "http or https",
		"http://":                          "loopback",
		"ftp://127.0.0.1/v1":               "http or https",
		"http://0.0.0.0:8080/v1":           "loopback",
		"http://[::ffff:8.8.8.8]:8080/v1":  "loopback",
	} {
		err := LLMAPIBaseLoopback(base)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want a refusal saying %q", base, err, want)
		}
	}
}

// Only the openai protocol on a loopback base runs without a key.
func TestLLMLocalKeyless(t *testing.T) {
	for _, c := range []struct {
		inner, base string
		want        bool
	}{
		{"openai//abs/path/model", "http://127.0.0.1:8080/v1", true},
		{"openai/m", "http://localhost:8080/v1", true},
		{"openai/m", "http://[::1]:8080/v1", true},
		{"openai/m", "", false}, // no base: the hosted openai needs a key
		{"openai/m", "https://api.openai.com/v1", false},
		{"openai/m", "http://evil.example/v1", false},
		{"anthropic-messages/m", "http://127.0.0.1:8080/v1", false},
		{"openrouter/m", "http://127.0.0.1:8080/v1", false},
		{"claude-cli/m", "http://127.0.0.1:8080/v1", false},
		{"", "http://127.0.0.1:8080/v1", false},
	} {
		if got := LLMLocalKeyless(c.inner, c.base); got != c.want {
			t.Errorf("LLMLocalKeyless(%q, %q) = %v, want %v", c.inner, c.base, got, c.want)
		}
	}
}

// A model id that is an absolute path (a local model directory) keeps its leading slash.
func TestSplitInnerModelKeepsAnAbsolutePathID(t *testing.T) {
	protocol, model, err := SplitInnerModel("openai//models/gemma_26b")
	if err != nil || protocol != "openai" || model != "/models/gemma_26b" {
		t.Fatalf("SplitInnerModel = %q, %q, %v", protocol, model, err)
	}
	if err := LLMModelAllowed("openai//models/gemma_26b", ParseLLMModels("openai//models/gemma_26b")); err != nil {
		t.Errorf("the allowlist entry as written must match: %v", err)
	}
}

func TestParseLLMCallTimeout(t *testing.T) {
	if d, err := ParseLLMCallTimeout(""); err != nil || d != 180*time.Second {
		t.Errorf("default = %v, %v; want 180s", d, err)
	}
	if d, err := ParseLLMCallTimeout("90s"); err != nil || d != 90*time.Second {
		t.Errorf("90s = %v, %v", d, err)
	}
	for _, bad := range []string{"0", "0s", "-5s", "500ms", "soon", "180"} {
		if _, err := ParseLLMCallTimeout(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

// A token budget parses with no price table anywhere.
func TestParseLLMBudgetTokensNeedNoPrice(t *testing.T) {
	usd, tok, err := ParseLLMBudget("20000000tok")
	if err != nil || usd != 0 || tok != 20000000 {
		t.Fatalf("ParseLLMBudget = %v, %d, %v", usd, tok, err)
	}
}
