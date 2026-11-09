// Package scenario is the Sandbox Lab scenario format (schema v1): a YAML file that
// says how to set an instance up, what happens over sim time, and what the lab journal
// must show afterwards. Parse reads one, Validate checks it against what the build and
// the running instance offer, and Expand turns a matrix into plain files. The runner
// (SBX-11b) walks the result.
package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultDir is where scenario files live; `khunquant sandbox run` globs it by default.
const DefaultDir = "testdata/scenarios"

// maxFile bounds a scenario file: it is data from an agent or a repository.
const maxFile = 1 << 20

// Status says whether a scenario gates (active) or documents an unfixed defect.
type Status string

const (
	StatusActive   Status = "active"
	StatusKnownBug Status = "known_bug"
)

// File is one scenario. Verbatim blocks (Profile, Faults, Web, LLMScript) are handed to
// the tool or lab op that owns them, unchanged.
type File struct {
	Version   int                    `yaml:"version" jsonschema:"required" jsonschema_description:"Schema version; must be 1."`
	Scenario  string                 `yaml:"scenario" jsonschema:"required" jsonschema_description:"Scenario id: lower-case letters or digits, then . _ - allowed; also the file name without .yaml."`
	Covers    []string               `yaml:"covers" jsonschema_description:"Review IDs or features this scenario covers (H8, S116A-1)."`
	Status    Status                 `yaml:"status,omitempty" jsonschema:"enum=active,enum=known_bug" jsonschema_description:"active (default) gates; known_bug documents an unfixed defect and runs only with KQ_SCENARIO_KNOWN_BUGS=1."`
	KnownBug  string                 `yaml:"known_bug,omitempty" jsonschema_description:"Review ID of the open defect; required with status known_bug and refused otherwise."`
	LLM       string                 `yaml:"llm" jsonschema:"enum=scripted,enum=replay,enum=live" jsonschema_description:"scripted (default; the only mode for PR CI), replay (a committed cassette) or live (nightly)."`
	Miss      string                 `yaml:"miss,omitempty" jsonschema:"enum=error,enum=scripted" jsonschema_description:"llm: replay only. A request the cassette lacks fails (error) or goes to llm_script (scripted)."` // replay only: a request the cassette lacks fails (error, default) or goes to llm_script (scripted)
	Level     string                 `yaml:"level" jsonschema:"enum=wire,enum=event" jsonschema_description:"wire (default) or event; recorded only in v1."`                                                               // wire (default) | event; recorded only in v1
	Runs      int                    `yaml:"runs,omitempty" jsonschema_description:"Runs per matrix row (default 1)."`
	PassRate  float64                `yaml:"pass_rate,omitempty" jsonschema_description:"Share of runs that must pass, 0 to 1 (default 1)."`
	Matrix    map[string][]string    `yaml:"matrix,omitempty" jsonschema_description:"Name to values; the file runs once per combination."`
	Base      string                 `yaml:"base,omitempty" jsonschema:"enum=main,enum=stack-head" jsonschema_description:"Informative: the base the scenario was written against."` // main | stack-head; informative
	Clock     ClockSpec              `yaml:"clock" jsonschema:"required" jsonschema_description:"The instance clock; a scenario controls it."`
	Instance  InstanceSpec           `yaml:"instance,omitempty" jsonschema_description:"Merged over the sandbox profile before the instance starts."`
	Profile   map[string]any         `yaml:"profile,omitempty" jsonschema_description:"Proactive profile (time zone, active hours), applied through call_tool."`
	Market    map[string]MarketInit  `yaml:"market,omitempty" jsonschema_description:"Symbol to starting price model. The venue must match the account."`
	Account   map[string]AccountSeed `yaml:"account,omitempty" jsonschema_description:"venue or venue/account to its simulated balances and positions."`
	Faults    []map[string]any       `yaml:"faults,omitempty" jsonschema_description:"Fault rules, each the arguments of sandbox_fault add."`
	Web       []map[string]any       `yaml:"web,omitempty" jsonschema_description:"Fake web pages, each the arguments of sandbox_web put."`
	LLMScript []map[string]any       `yaml:"llm_script,omitempty" jsonschema_description:"Scripted model rules (match, respond) for llm: scripted."`
	Setup     []Step                 `yaml:"setup,omitempty" jsonschema_description:"Steps that run before the clock moves (no at)."`
	Timeline  []Step                 `yaml:"timeline,omitempty" jsonschema_description:"Steps over sim time; at offsets must not decrease."`
	Expect    []Expectation          `yaml:"expect" jsonschema:"required" jsonschema_description:"What the journal must show afterwards; at least one."`

	// MatrixRow is the combination an expanded file stands for (nil: no matrix).
	MatrixRow map[string]string `yaml:"-"`

	raw       *yaml.Node // the document, for Expand
	keyIssues []Issue    // unknown keys found by Parse; Validate reports them
}

// ClockSpec maps to the instance clock options (KQ_SANDBOX_CLOCK_*).
type ClockSpec struct {
	Start      string `yaml:"start" jsonschema:"required" jsonschema_description:"RFC 3339 start time, such as 2026-10-20T09:00:00+07:00."` // RFC 3339
	Mode       string `yaml:"mode" jsonschema:"enum=manual"`                                                                                // manual (default)
	Resolution string `yaml:"resolution,omitempty" jsonschema_description:"Go duration; the clock default when empty."`                     // Go duration; the clock's default when empty
}

// InstanceSpec is merged over the sandbox profile before the instance starts.
type InstanceSpec struct {
	Config    map[string]any    `yaml:"config,omitempty" jsonschema_description:"Partial gateway config merged over the profile."`
	Workspace map[string]string `yaml:"workspace,omitempty" jsonschema_description:"Path under the workspace to file content."` // path under <home>/workspace -> content
	// Accounts asks the sandbox profile for a placeholder exchange account on these
	// venues (binance, okx), so tools find them configured. The scenario never carries a
	// credential: the profile writes constants that cannot be real.
	Accounts []string `yaml:"accounts,omitempty" jsonschema_description:"Venues (binance, okx) that get a placeholder account; never a credential."`
}

// MarketInit is the starting price model of one symbol. Model names the market op
// (set by default); every other key is that op's argument, verbatim.
type MarketInit struct {
	Venue string         `yaml:"venue,omitempty"`
	Model string         `yaml:"model,omitempty"`
	Args  map[string]any `yaml:",inline"`
}

// AccountSeed seeds one venue's simulated account. The key is "<venue>[/<account>]".
type AccountSeed struct {
	Balances  map[string]float64 `yaml:"balances,omitempty" jsonschema_description:"Currency to amount."`
	Positions []map[string]any   `yaml:"positions,omitempty" jsonschema_description:"Open positions, in the shape sandbox_account seed takes."`
	Replace   bool               `yaml:"replace,omitempty" jsonschema_description:"Replace the venue's seeded account instead of merging."`
	Baseline  *bool              `yaml:"baseline,omitempty"`
	Options   map[string]any     `yaml:"options,omitempty"` // the venue's knobs (settrade: auction_open, auction_afternoon)
}

// Issue is one problem with a scenario; Path is a JSON pointer into the document.
type Issue struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

func (i Issue) String() string {
	if i.Path == "" {
		return "/: " + i.Msg
	}
	return i.Path + ": " + i.Msg
}

// Parse reads one scenario. It fails on YAML that is not a mapping or has a value of
// the wrong type; unknown keys are kept for Validate, which gives them a JSON pointer.
func Parse(r io.Reader) (*File, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("scenario: file is larger than %d bytes", maxFile)
	}
	var root yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("scenario: the file is empty")
		}
		return nil, fmt.Errorf("scenario: %w", err)
	}
	return decodeDoc(&root)
}

func decodeDoc(root *yaml.Node) (*File, error) {
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil, errors.New("scenario: a scenario is a YAML mapping")
	}
	f := &File{raw: doc}
	if err := doc.Decode(f); err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	f.keyIssues = unknownKeys(doc, reflect.TypeOf(*f), "")
	return f, nil
}

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// unknownKeys reports every mapping key that no struct field of t accepts. A type that
// decodes itself (Step, Expectation) is checked by Validate instead.
func unknownKeys(n *yaml.Node, t reflect.Type, ptr string) []Issue {
	if n.Kind != yaml.MappingNode || reflect.PointerTo(t).Implements(unmarshalerType) {
		return nil
	}
	var out []Issue
	switch t.Kind() {
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		inline := false
		for i := range t.NumField() {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			switch {
			case name == "-":
			case strings.Contains(opts, "inline"):
				inline = true
			case name != "":
				fields[name] = f.Type
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			ft, ok := fields[k]
			switch {
			case ok:
				out = append(out, unknownKeys(v, deref(ft), ptr+"/"+esc(k))...)
			case !inline:
				out = append(out, Issue{ptr + "/" + esc(k), "unknown field"})
			}
		}
	case reflect.Map:
		if et := deref(t.Elem()); et.Kind() == reflect.Struct {
			for i := 0; i+1 < len(n.Content); i += 2 {
				out = append(out, unknownKeys(n.Content[i+1], et, ptr+"/"+esc(n.Content[i].Value))...)
			}
		}
	}
	return out
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// esc escapes one JSON-pointer segment (RFC 6901).
func esc(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// RunCount is runs, at least 1.
func (f *File) RunCount() int { return max(f.Runs, 1) }

// RequiredPassRate is pass_rate, 1 when unset.
func (f *File) RequiredPassRate() float64 {
	if f.PassRate == 0 {
		return 1
	}
	return f.PassRate
}

// LLMMode is llm, scripted when unset.
func (f *File) LLMMode() string {
	if f.LLM == "" {
		return "scripted"
	}
	return f.LLM
}

// IsKnownBug says whether the scenario documents an unfixed defect.
func (f *File) IsKnownBug() bool { return f.Status == StatusKnownBug }

// StartTime is clock.start (the zero time when it is missing or invalid).
func (f *File) StartTime() time.Time {
	t, _ := time.Parse(time.RFC3339, f.Clock.Start)
	return t
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
