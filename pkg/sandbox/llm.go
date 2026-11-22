package sandbox

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The environment of a sandbox instance's simulated model. The supervisor sets it in the
// child (and nowhere else: the key is never written to config, the journal or a log), the
// gateway reads it. Only KQ_SANDBOX_LLM and the seed are set for scripted and replay.
const (
	EnvLLM        = "KQ_SANDBOX_LLM"              // scripted | replay | record | live
	EnvLLMModel   = "KQ_SANDBOX_LLM_MODEL"        // record/live: <protocol>/<model> of the real model
	EnvLLMKey     = "KQ_SANDBOX_LLM_KEY"          // record/live: the API key; the child's env only
	EnvLLMBudget  = "KQ_SANDBOX_LLM_BUDGET"       // record/live: "2USD" or "200000tok"
	EnvLLMModels  = "KQ_SANDBOX_LLM_MODELS"       // record/live: allowed <protocol>/<model> ids, comma-separated
	EnvLLMPrices  = "KQ_SANDBOX_LLM_PRICES"       // optional: "[protocol/]model=in:out,..." USD per million tokens
	EnvLLMAPIBase = "KQ_SANDBOX_LLM_API_BASE"     // Go callers only (UpOptions.LLMAPIBase): overrides the protocol's API base
	EnvLLMTimeout = "KQ_SANDBOX_LLM_CALL_TIMEOUT" // record/live: wall limit of one model call, a Go duration (default DefaultLLMCallTimeout)
	EnvSeed       = "KQ_SANDBOX_SEED"             // non-zero int64 seeds the nonce source (pkg/randsrc)
	EnvAllowHosts = "KQ_SANDBOX_ALLOW_HOSTS"      // the one inner API host, record/live only
)

// LocalLLMKey is the fixed key a loopback openai base is sent. A local server (an MLX or
// llama.cpp server) needs none; sending a constant keeps a real key from ever reaching it.
const LocalLLMKey = "sk-sandbox-local-no-key"

// DefaultLLMCallTimeout is the wall limit of one live or record model call when none is set:
// a local 26B model answers in 10 to 60 s, so 180 s leaves room and still ends a hung server.
const DefaultLLMCallTimeout = 180 * time.Second

// LLMAPIBaseLoopback checks the API base a caller may give: an explicit http or https URL
// whose host is exactly localhost, 127.0.0.1 or ::1, with no user info. Anything else (a
// remote host, a file: URL, no host, a lookalike such as http://127.0.0.1@evil.example) is
// an error, so the key-free path can never point at a remote host.
func LLMAPIBaseLoopback(base string) error {
	base = strings.TrimSpace(base)
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("api base %q is not a URL", base)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("api base %q must be an http or https URL", base)
	}
	if u.User != nil {
		return fmt.Errorf("api base %q must not carry user info", base)
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	return fmt.Errorf("api base %q: only a loopback host (localhost, 127.0.0.1 or [::1]) is allowed", base)
}

// LLMLocalKeyless says whether a model reached through apiBase needs no real key: the
// openai protocol on a loopback base.
func LLMLocalKeyless(inner, apiBase string) bool {
	protocol, _, err := SplitInnerModel(inner)
	return err == nil && protocol == "openai" && strings.TrimSpace(apiBase) != "" && LLMAPIBaseLoopback(apiBase) == nil
}

// ParseLLMCallTimeout reads the per-call wall limit ("" is the default).
func ParseLLMCallTimeout(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return DefaultLLMCallTimeout, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("llm call timeout %q must be a duration of at least 1s, e.g. 180s", s)
	}
	return d, nil
}

// LLMDefaultBase is the API base of each protocol a live or record instance may use: the
// HTTP clients that run on http.DefaultTransport, which the egress guard replaces. Every
// other protocol (the agent CLIs, OAuth providers, local daemons) is refused. A providers
// test pins these against the model factory's own defaults.
var LLMDefaultBase = map[string]string{
	"anthropic-messages": "https://api.anthropic.com/v1",
	"openai":             "https://api.openai.com/v1",
	"openrouter":         "https://openrouter.ai/api/v1",
}

// SplitInnerModel splits "<protocol>/<model>" and checks the protocol is one LLMDefaultBase allows.
func SplitInnerModel(inner string) (protocol, model string, err error) {
	protocol, model, ok := strings.Cut(strings.TrimSpace(inner), "/")
	if !ok || model == "" {
		return "", "", fmt.Errorf("inner model %q must be <protocol>/<model>", inner)
	}
	if _, allowed := LLMDefaultBase[protocol]; !allowed {
		return "", "", fmt.Errorf("inner protocol %q is not allowed (only anthropic-messages, openai, openrouter)", protocol)
	}
	return protocol, model, nil
}

// LLMInnerHost is the one host a live or record instance may reach: the host of apiBase,
// or of the protocol's default. The supervisor puts it in LockOptions.AllowHosts, and the
// gateway checks the lock against it, so both call this. A non-loopback base must be https.
func LLMInnerHost(inner, apiBase string) (string, error) {
	protocol, _, err := SplitInnerModel(inner)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(apiBase)
	if base == "" {
		base = LLMDefaultBase[protocol]
	}
	host := apiBaseHost(base)
	if host == "" {
		return "", fmt.Errorf("api base %q has no host", base)
	}
	if !hostIsLoopback(host) && !strings.HasPrefix(strings.ToLower(base), "https://") {
		return "", fmt.Errorf("api base %q must be https (the key travels in it)", base)
	}
	return host, nil
}

// ParseLLMBudget reads "2USD" / "2.5usd" (dollars) or "200000tok" (tokens). Exactly one
// is returned non-zero.
func ParseLLMBudget(s string) (usd float64, tok int, err error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(t, "usd"):
		usd, err = strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(t, "usd")), 64)
		if err != nil || math.IsNaN(usd) || math.IsInf(usd, 0) || usd <= 0 {
			return 0, 0, fmt.Errorf("budget %q: the dollar amount must be a positive number", s)
		}
	case strings.HasSuffix(t, "tok"):
		tok, err = strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(t, "tok")))
		if err != nil || tok <= 0 {
			return 0, 0, fmt.Errorf("budget %q: the token count must be a positive integer", s)
		}
	default:
		return 0, 0, errors.New("budget " + strconv.Quote(s) + " must end in USD or tok, e.g. 2USD or 200000tok")
	}
	return usd, tok, nil
}

// LLMModelAllowed checks inner ("<protocol>/<model>") against the KQ_SANDBOX_LLM_MODELS
// allowlist. An entry names the protocol as well as the model: the protocol picks the API
// host the key is sent to, so an entry that named the model alone would let any allowed
// protocol's host receive the key (192-1). A bare entry matches nothing, and the error says
// how to write it. The supervisor and the gateway both call this, so they cannot drift.
func LLMModelAllowed(inner string, allowlist []string) error {
	protocol, model, err := SplitInnerModel(inner)
	if err != nil {
		return err
	}
	want := protocol + "/" + model
	bare := false
	for _, e := range allowlist {
		switch e {
		case want:
			return nil
		case model:
			bare = true
		}
	}
	if bare {
		return fmt.Errorf("%s lists %q without its protocol; list it as %q", EnvLLMModels, model, want)
	}
	return fmt.Errorf("model %q is not in %s", want, EnvLLMModels)
}

// ParseLLMModels splits the allowlist.
func ParseLLMModels(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// ParseLLMPrices reads "model=in:out,model=in:out" (USD per million tokens).
func ParseLLMPrices(s string) (map[string][2]float64, error) {
	out := map[string][2]float64{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		model, rest, ok := strings.Cut(part, "=")
		in, outp, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 || strings.TrimSpace(model) == "" {
			return nil, fmt.Errorf("price %q must be model=in:out", part)
		}
		a, errA := strconv.ParseFloat(in, 64)
		b, errB := strconv.ParseFloat(outp, 64)
		if errA != nil || errB != nil || a < 0 || b < 0 || math.IsNaN(a+b) || math.IsInf(a+b, 0) {
			return nil, fmt.Errorf("price %q must be two non-negative numbers", part)
		}
		out[strings.TrimSpace(model)] = [2]float64{a, b}
	}
	return out, nil
}
