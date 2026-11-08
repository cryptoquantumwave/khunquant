package fault

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"time"
)

// The rule table outlives a gateway process (lab.Persistent): a venue that was failing
// before the instance was killed is still failing after it. A rule is saved as stored,
// with its resolved window (the "+10m" of Add was counted from the moment of Add, not from
// the restart), its firing count and the state of its probability stream.

// StateKey is the file name of the injector's saved state (<home>/lab/world/faults.json).
func (i *Injector) StateKey() string { return "faults" }

// SetOnChange sets the function called, with no lock held, after the rules or a firing
// count changed. The lab persists the table there, before the response that fired a rule.
func (i *Injector) SetOnChange(f func()) {
	i.mu.Lock()
	i.onChange = f
	i.mu.Unlock()
}

func (i *Injector) changed() {
	i.mu.Lock()
	f := i.onChange
	i.mu.Unlock()
	if f != nil {
		f()
	}
}

type ruleDump struct {
	Rule  Rule      `json:"rule"`
	Fired int       `json:"fired,omitempty"`
	From  time.Time `json:"from,omitzero"`
	To    time.Time `json:"to,omitzero"`
	RNG   []byte    `json:"rng"`
}

type tableDump struct {
	Seq   int        `json:"seq"`
	Rules []ruleDump `json:"rules"`
}

// MarshalState saves the table.
func (i *Injector) MarshalState() ([]byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	t := tableDump{Seq: i.seq, Rules: []ruleDump{}}
	for _, e := range i.rules {
		rng, err := e.src.MarshalBinary()
		if err != nil {
			return nil, err
		}
		t.Rules = append(t.Rules, ruleDump{Rule: e.Rule, Fired: e.fired, From: e.from, To: e.to, RNG: rng})
	}
	return json.Marshal(t)
}

// RestoreState replaces the table with the saved one. A rule is checked as Add checks it
// (the order effects must be enabled first), and nothing changes on an error.
func (i *Injector) RestoreState(data []byte) error {
	var t tableDump
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	rules := make([]*entry, 0, len(t.Rules))
	for _, d := range t.Rules {
		kind, pair, err := i.validate(d.Rule)
		if err != nil {
			return fmt.Errorf("rule %q: %w", d.Rule.ID, err)
		}
		src := rand.NewPCG(0, 0)
		if err := src.UnmarshalBinary(d.RNG); err != nil {
			return fmt.Errorf("rule %q: %w", d.Rule.ID, err)
		}
		rules = append(rules, &entry{Rule: d.Rule, kind: kind, ly: kindLayer[kind], pair: pair, from: d.From, to: d.To,
			fired: d.Fired, rng: rand.New(src), src: src})
	}
	i.mu.Lock()
	i.rules, i.seq = rules, t.Seq
	i.refreshLocked()
	i.mu.Unlock()
	return nil
}
