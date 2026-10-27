package supervisor

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// UpOptions is the one definition of the options of `up`: the flags of `khunquant
// sandbox up` and the arguments of sandbox_instance op=up are both generated from the
// tagged fields (see UpOptionFields), so an option added here appears in both. A field
// without a flag tag is for Go callers only.
type UpOptions struct {
	Name         string `json:"name,omitempty" flag:"name" desc:"Instance name (default: random; \"default\" in sandbox_instance)"`
	FixturesFrom string `json:"fixtures_from,omitempty" flag:"fixtures-from" desc:"Directory holding <venue>/fixtures.json (default: ./workspace/sandbox)"`

	// Env is extra child environment; only keys that AllowEnv has allowed pass.
	Env map[string]string `json:"-"`
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
