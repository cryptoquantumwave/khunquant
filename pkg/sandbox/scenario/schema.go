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
	Version   int                    `yaml:"version"`
	Scenario  string                 `yaml:"scenario"`
	Covers    []string               `yaml:"covers"`
	Status    Status                 `yaml:"status,omitempty"`
	KnownBug  string                 `yaml:"known_bug,omitempty"`
	LLM       string                 `yaml:"llm"`
	Level     string                 `yaml:"level"` // wire (default) | event; recorded only in v1
	Runs      int                    `yaml:"runs,omitempty"`
	PassRate  float64                `yaml:"pass_rate,omitempty"`
	Matrix    map[string][]string    `yaml:"matrix,omitempty"`
	Base      string                 `yaml:"base,omitempty"` // main | stack-head; informative
	Clock     ClockSpec              `yaml:"clock"`
	Instance  InstanceSpec           `yaml:"instance,omitempty"`
	Profile   map[string]any         `yaml:"profile,omitempty"`
	Market    map[string]MarketInit  `yaml:"market,omitempty"`
	Account   map[string]AccountSeed `yaml:"account,omitempty"`
	Faults    []map[string]any       `yaml:"faults,omitempty"`
	Web       []map[string]any       `yaml:"web,omitempty"`
	LLMScript []map[string]any       `yaml:"llm_script,omitempty"`
	Setup     []Step                 `yaml:"setup,omitempty"`
	Timeline  []Step                 `yaml:"timeline,omitempty"`
	Expect    []Expectation          `yaml:"expect"`

	// MatrixRow is the combination an expanded file stands for (nil: no matrix).
	MatrixRow map[string]string `yaml:"-"`

	raw       *yaml.Node // the document, for Expand
	keyIssues []Issue    // unknown keys found by Parse; Validate reports them
}

// ClockSpec maps to the instance clock options (KQ_SANDBOX_CLOCK_*).
type ClockSpec struct {
	Start      string `yaml:"start"`                // RFC 3339
	Mode       string `yaml:"mode"`                 // manual (default)
	Resolution string `yaml:"resolution,omitempty"` // Go duration; the clock's default when empty
}

// InstanceSpec is merged over the sandbox profile before the instance starts.
type InstanceSpec struct {
	Config    map[string]any    `yaml:"config,omitempty"`
	Workspace map[string]string `yaml:"workspace,omitempty"` // path under <home>/workspace -> content
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
	Balances  map[string]float64 `yaml:"balances,omitempty"`
	Positions []map[string]any   `yaml:"positions,omitempty"`
	Replace   bool               `yaml:"replace,omitempty"`
	Baseline  *bool              `yaml:"baseline,omitempty"`
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
