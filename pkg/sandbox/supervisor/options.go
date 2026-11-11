package supervisor

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// UpOptions is the one definition of the options of `up`: the flags of `khunquant
// sandbox up` and the arguments of sandbox_instance op=up are both generated from the
// tagged fields (see UpOptionFields), so an option added here appears in both. A field
// without a flag tag is for Go callers only.
type UpOptions struct {
	Name         string `json:"name,omitempty" flag:"name" desc:"Instance name (default: random; \"default\" in sandbox_instance)"`
	FixturesFrom string `json:"fixtures_from,omitempty" flag:"fixtures-from" desc:"Directory holding <venue>/fixtures.json (default: ./workspace/sandbox)"`

	// The clock options become the instance's KQ_SANDBOX_CLOCK_* environment (see
	// clockEnv); a restart reads the saved clock.json instead.
	ClockStart      string `json:"clock_start,omitempty" flag:"clock-start" desc:"Sim clock start, RFC 3339 (default: wall now)"`
	ClockMode       string `json:"clock_mode,omitempty" flag:"clock-mode" desc:"Sim clock mode: manual, scaled or realtime (default: realtime)"`
	ClockResolution string `json:"clock_resolution,omitempty" flag:"clock-resolution" desc:"Manual-mode clock quantum, a duration; 0 steps exactly (default: 1m)"`

	// The simulated model (SBX-16). LLM is scripted (default), replay, record or live; the
	// last two reach the real model named by LLMModel ("<protocol>/<model>"), need a
	// LLMBudget ("2USD" or "200000tok") and read the key from KQ_SANDBOX_LLM_KEY, the
	// allowlist from KQ_SANDBOX_LLM_MODELS and optional prices from KQ_SANDBOX_LLM_PRICES in
	// the environment of whoever starts the instance. Seed seeds the nonces and ids of the
	// instance: empty derives one from the instance name, "0" leaves them random.
	LLM       string `json:"llm,omitempty" flag:"llm" desc:"Simulated model: scripted (default), replay, record or live"`
	LLMModel  string `json:"llm_model,omitempty" flag:"llm-model" desc:"record/live: the real model, <protocol>/<model> (anthropic-messages, openai or openrouter); must be in KQ_SANDBOX_LLM_MODELS as written, protocol included"`
	LLMBudget string `json:"llm_budget,omitempty" flag:"llm-budget" desc:"record/live: spending cap, e.g. 2USD or 200000tok (required; it can only be lowered later)"`
	Seed      string `json:"seed,omitempty" flag:"seed" desc:"Seed of the instance's nonces and ids: an integer, 0 for none (default: derived from the instance name)"`

	// LLMAPIBase overrides the protocol's API base, for a test that points record/live at a
	// loopback fake. Go callers only: whatever host it names is the one host the instance may
	// reach and receives the key.
	LLMAPIBase string `json:"-"`

	// Env is extra child environment; only keys that AllowEnv has allowed pass.
	Env map[string]string `json:"-"`

	// ConfigPatch is merged into config.json and Workspace is written under
	// <home>/workspace before the gateway starts (the scenario runner's instance block).
	// Both are for Go callers: no flag and no JSON name, so neither `up` nor
	// sandbox_instance can carry them. CheckConfigPatch and CheckWorkspacePath apply.
	ConfigPatch map[string]any    `json:"-"`
	Workspace   map[string]string `json:"-"`

	// Accounts are the venues the profile gives a placeholder exchange account (see
	// sandbox.ProfileOptions). Like ConfigPatch it is for Go callers only: a flag or an MCP
	// argument would let a tool caller turn on a venue the instance never configured.
	Accounts []string `json:"-"`

	// restore, set only by Restore, fills each attempt's home from a snapshot.
	restore *restoreSpec `json:"-"`
}

// OptionField describes one generated option of UpOptions.
type OptionField struct {
	Index      int    // field index in UpOptions
	JSON, Flag string // MCP argument name, CLI flag name
	Desc       string
	Kind       reflect.Kind // string, bool or int
}

// UpOptionFields lists the options of UpOptions that have a CLI flag and an MCP argument.
func UpOptionFields() []OptionField {
	t := reflect.TypeFor[UpOptions]()
	var out []OptionField
	for i := range t.NumField() {
		f := t.Field(i)
		flag := f.Tag.Get("flag")
		if flag == "" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out = append(out, OptionField{Index: i, JSON: name, Flag: flag, Desc: f.Tag.Get("desc"), Kind: f.Type.Kind()})
	}
	return out
}

var (
	envMu      sync.RWMutex
	allowedEnv = map[string]bool{}
)

// fixedEnv are the variables the supervisor sets itself; no option can touch them.
var fixedEnv = []string{"HOME", "PATH", "TMPDIR", "LANG", "TZ", "KQ_SANDBOX_LOCKED", "KQ_SANDBOX_TOKEN", "KQ_SANDBOX_INSTANCE"}

func isFixedEnv(k string) bool {
	return strings.HasPrefix(k, "KHUNQUANT_") || slices.Contains(fixedEnv, k)
}

// AllowEnv lets UpOptions.Env carry keys beyond the fixed ones, for the PRs that add
// options (SBX-07's clock variables, SBX-16's LLM settings). It panics on a key the
// supervisor sets itself.
func AllowEnv(keys ...string) {
	envMu.Lock()
	defer envMu.Unlock()
	for _, k := range keys {
		if k == "" || isFixedEnv(k) {
			panic(fmt.Sprintf("supervisor: AllowEnv(%q): the supervisor owns this variable", k))
		}
		allowedEnv[k] = true
	}
}

// validEnv refuses every key of env that AllowEnv has not allowed.
func validEnv(env map[string]string) error {
	envMu.RLock()
	defer envMu.RUnlock()
	for k := range env {
		if !allowedEnv[k] || isFixedEnv(k) {
			return fmt.Errorf("sandbox up: environment variable %q is not allowed in an instance", k)
		}
	}
	return nil
}

// The instance clock's environment (lab.EnvClockStart and its two siblings; a test
// pins that the names agree: the supervisor does not import the lab).
const (
	envClockStart      = "KQ_SANDBOX_CLOCK_START"
	envClockMode       = "KQ_SANDBOX_CLOCK_MODE"
	envClockResolution = "KQ_SANDBOX_CLOCK_RESOLUTION"
)

// The LLM variables the supervisor sets itself (sandbox.Env*): they are not AllowEnv-ed,
// so UpOptions.Env cannot carry them. Only llmEnv sets them, from the validated options.
const (
	envLLM, envLLMModel, envLLMBudget = sandbox.EnvLLM, sandbox.EnvLLMModel, sandbox.EnvLLMBudget
	envSeed, envAllowHosts            = sandbox.EnvSeed, sandbox.EnvAllowHosts
)

func init() { AllowEnv(envClockStart, envClockMode, envClockResolution) }

// llmEnv adds the simulated model's settings. Everything is checked here, before a
// directory exists or a process starts: a live instance without a budget, with a model
// outside the allowlist or without a key never reaches the gateway. The egress opening
// (KQ_SANDBOX_ALLOW_HOSTS) is derived from the validated model alone, never taken from
// o.Env, and only for record and live.
func (o UpOptions) llmEnv(env map[string]string, name string, getenv func(string) string) error {
	mode := o.LLM
	switch mode {
	case "", "scripted", "replay":
		if o.LLMModel != "" || o.LLMBudget != "" || o.LLMAPIBase != "" {
			return fmt.Errorf("sandbox up: llm_model, llm_budget and the API base belong to --llm record or live, not %q", cmp.Or(mode, "scripted"))
		}
	case "record", "live":
		if err := o.checkLive(env, getenv); err != nil {
			return err
		}
	default:
		return fmt.Errorf("sandbox up: llm %q is not scripted, replay, record or live", mode)
	}
	if mode != "" {
		env[envLLM] = mode
	}
	seed, err := seedOf(o.Seed, name)
	if err != nil {
		return err
	}
	if seed != 0 {
		env[envSeed] = strconv.FormatInt(seed, 10)
	}
	return nil
}

// checkLive validates a record or live start and sets its variables. The key is read from
// the starter's environment, never from an option.
func (o UpOptions) checkLive(env map[string]string, getenv func(string) string) error {
	if _, _, err := sandbox.SplitInnerModel(o.LLMModel); err != nil {
		return fmt.Errorf("sandbox up: --llm %s: %w", o.LLM, err)
	}
	if _, _, err := sandbox.ParseLLMBudget(o.LLMBudget); err != nil {
		return fmt.Errorf("sandbox up: --llm %s needs --llm-budget: %w", o.LLM, err)
	}
	key := getenv(sandbox.EnvLLMKey)
	if key == "" {
		return fmt.Errorf("sandbox up: --llm %s needs %s in the environment", o.LLM, sandbox.EnvLLMKey)
	}
	models := getenv(sandbox.EnvLLMModels)
	if err := sandbox.LLMModelAllowed(o.LLMModel, sandbox.ParseLLMModels(models)); err != nil {
		return fmt.Errorf("sandbox up: %w", err)
	}
	host, err := sandbox.LLMInnerHost(o.LLMModel, o.LLMAPIBase)
	if err != nil {
		return fmt.Errorf("sandbox up: --llm %s: %w", o.LLM, err)
	}
	env[envLLMModel], env[envLLMBudget] = o.LLMModel, o.LLMBudget
	env[sandbox.EnvLLMKey], env[sandbox.EnvLLMModels], env[envAllowHosts] = key, models, host
	if p := getenv(sandbox.EnvLLMPrices); p != "" {
		env[sandbox.EnvLLMPrices] = p
	}
	if o.LLMAPIBase != "" {
		env[sandbox.EnvLLMAPIBase] = o.LLMAPIBase
	}
	return nil
}

// seedOf is the instance's seed: the option when given ("0": none), else one derived from
// the name, so two runs of a named instance mint the same codes and ids.
func seedOf(opt, name string) (int64, error) {
	if opt == "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(name))
		return int64(h.Sum64() >> 1), nil // non-negative; zero is vanishingly unlikely and means "none"
	}
	n, err := strconv.ParseInt(strings.TrimSpace(opt), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sandbox up: seed %q is not an integer", opt)
	}
	return n, nil
}

// childEnvOf is o.Env plus the clock options as their variables, checked here so a bad
// value refuses the `up` rather than a gateway that then fails to start.
func (o UpOptions) childEnvOf(name string, getenv func(string) string) (map[string]string, error) {
	env := maps.Clone(o.Env)
	if env == nil {
		env = map[string]string{}
	}
	if err := o.llmEnv(env, name, getenv); err != nil {
		return nil, err
	}
	if o.ClockStart != "" {
		if _, err := time.Parse(time.RFC3339, o.ClockStart); err != nil {
			return nil, fmt.Errorf("sandbox up: clock_start %q is not RFC 3339", o.ClockStart)
		}
		env[envClockStart] = o.ClockStart
	}
	if o.ClockMode != "" {
		if !slices.Contains([]string{"manual", "scaled", "realtime"}, o.ClockMode) {
			return nil, fmt.Errorf("sandbox up: clock_mode %q is not manual, scaled or realtime", o.ClockMode)
		}
		env[envClockMode] = o.ClockMode
	}
	if o.ClockResolution != "" {
		if d, err := time.ParseDuration(o.ClockResolution); err != nil || d < 0 {
			return nil, fmt.Errorf("sandbox up: clock_resolution %q is not a duration >= 0", o.ClockResolution)
		}
		env[envClockResolution] = o.ClockResolution
	}
	return env, nil
}
