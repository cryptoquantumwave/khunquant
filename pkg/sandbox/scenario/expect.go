package scenario

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Window is a span of sim time given as offsets from clock.start. An empty end is open.
type Window struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Bounds returns the offsets; an empty From is 0 and an empty To is "no end" (hasTo false).
func (w *Window) Bounds() (from, to time.Duration, hasTo bool, err error) {
	if w == nil {
		return 0, 0, false, nil
	}
	if w.From != "" {
		if from, err = Offset(w.From); err != nil {
			return 0, 0, false, err
		}
	}
	if w.To != "" {
		if to, err = Offset(w.To); err != nil {
			return 0, 0, false, err
		}
		hasTo = true
	}
	return from, to, hasTo, nil
}

// Expectation is one entry of expect: an optional id, one kind key (message, event, ...)
// whose mapping holds the kind's fields, and the count / within / during / replay_match
// modifiers, which may sit inside that mapping or beside it.
type Expectation struct {
	ID          string
	Kind        string         // message | event | decision | order | workflow | proposal | policy | probe | llm | web | logs
	Fields      map[string]any // the kind's own fields
	Count       string         // "1", ">= 1", "<= 2", "0": labjournal.Match's count grammar
	Within      *Window
	During      string // "22:00-07:00" in the profile's time zone
	ReplayMatch string // message only; read by SBX-21, ignored by the runner

	kinds []string // every key that is not id or a modifier
	dups  []string // modifiers given both inside and beside the kind mapping
	bad   string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (e *Expectation) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		e.bad = "an expectation is a mapping with one kind key"
		return nil
	}
	var inner map[string]*yaml.Node // modifiers found inside the kind mapping
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if k == "id" {
			e.ID = v.Value
			continue
		}
		if modifier(k) {
			if err := e.setModifier(k, v); err != nil {
				return err
			}
			continue
		}
		e.kinds = append(e.kinds, k)
		if len(e.kinds) > 1 {
			continue
		}
		e.Kind, e.Fields = k, map[string]any{}
		if v.Kind == yaml.MappingNode {
			if err := v.Decode(&e.Fields); err != nil {
				return err
			}
			inner = map[string]*yaml.Node{}
			for j := 0; j+1 < len(v.Content); j += 2 {
				inner[v.Content[j].Value] = v.Content[j+1]
			}
		}
	}
	for k := range e.Fields {
		if modifier(k) {
			delete(e.Fields, k)
			if err := e.setModifier(k, inner[k]); err != nil {
				return err
			}
		}
	}
	return nil
}

func modifier(k string) bool {
	return k == "count" || k == "within" || k == "during" || k == "replay_match"
}

func (e *Expectation) setModifier(k string, v *yaml.Node) error {
	set := func(cur *string) {
		if *cur != "" {
			e.dups = append(e.dups, k)
		}
		*cur = v.Value
	}
	switch k {
	case "count":
		set(&e.Count)
	case "during":
		set(&e.During)
	case "replay_match":
		set(&e.ReplayMatch)
	case "within":
		if e.Within != nil {
			e.dups = append(e.dups, k)
		}
		var w Window
		if err := v.Decode(&w); err != nil {
			return fmt.Errorf("within: %w", err)
		}
		e.Within = &w
	}
	return nil
}

// ParseDuring reads "HH:MM-HH:MM" as minutes since midnight. A window whose end is not
// after its start wraps midnight.
func ParseDuring(s string) (from, to int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("during %q is not HH:MM-HH:MM", s)
	}
	if from, err = clockMinutes(a); err == nil {
		to, err = clockMinutes(b)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("during %q is not HH:MM-HH:MM", s)
	}
	return from, to, nil
}

func clockMinutes(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return hh*60 + mm, nil
}
