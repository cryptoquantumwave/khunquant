package scenario

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Step is one entry of setup or timeline: one kind key (chat, market.path, advance, ...)
// whose value is the step's arguments, plus an optional at. Several kind keys, or none,
// are kept and reported by Validate with a JSON pointer rather than failing the decode.
type Step struct {
	At   string    // "+5m" from clock.start; "" = no move before the step
	Kind string    // the kind key
	Args yaml.Node // its value, decoded per kind

	kinds []string // every non-at key, in order
	bad   string   // why the entry is not a mapping
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Step) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		s.bad = "a step is a mapping with one kind key"
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if k == "at" {
			s.At = v.Value
			continue
		}
		if s.Kind == "" {
			s.Kind, s.Args = k, *v
		}
		s.kinds = append(s.kinds, k)
	}
	return nil
}

// Map decodes the arguments as a mapping (an absent value is an empty mapping).
func (s *Step) Map() (map[string]any, error) {
	m := map[string]any{}
	if s.Args.Kind == 0 || (s.Args.Kind == yaml.ScalarNode && s.Args.Tag == "!!null") {
		return m, nil
	}
	if s.Args.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s takes a mapping", s.Kind)
	}
	if err := s.Args.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// Scalar is the argument when it is a plain value (advance: 15m).
func (s *Step) Scalar() (string, bool) {
	if s.Args.Kind != yaml.ScalarNode {
		return "", false
	}
	return s.Args.Value, true
}

// Offset parses an at / run_until offset: "+90m", "90m" or "0".
func Offset(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimPrefix(strings.TrimSpace(s), "+"))
	if err != nil {
		return 0, fmt.Errorf("%q is not an offset such as +90m", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("offset %q is negative", s)
	}
	return d, nil
}

// Target is where a run_until stops.
type Target struct {
	Offset time.Duration // from clock.start, when Kind is "offset"
	At     time.Time     // when Kind is "time"
	Minute int           // minutes since midnight, when Kind is "next"
	Kind   string        // offset | time | next
}

// ParseTarget reads "+2h", an RFC 3339 time or "next HH:MM".
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "next "); ok {
		m, err := clockMinutes(rest)
		if err != nil || m >= 24*60 {
			return Target{}, fmt.Errorf("%q is not next HH:MM", s)
		}
		return Target{Kind: "next", Minute: m}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return Target{Kind: "time", At: t}, nil
	}
	d, err := Offset(s)
	if err != nil {
		return Target{}, fmt.Errorf("%q is not +offset, an RFC 3339 time or next HH:MM", s)
	}
	return Target{Kind: "offset", Offset: d}, nil
}

// Resolve is the sim instant of the target, given the scenario start and the sim time
// now. "next HH:MM" is the first such wall-clock time in loc after now.
func (t Target) Resolve(start, now time.Time, loc *time.Location) time.Time {
	switch t.Kind {
	case "offset":
		return start.Add(t.Offset)
	case "time":
		return t.At
	}
	n := now.In(loc)
	at := time.Date(n.Year(), n.Month(), n.Day(), t.Minute/60, t.Minute%60, 0, 0, loc)
	if !at.After(n) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}
