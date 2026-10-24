package reflection

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// Item types a reflection may propose. new_watch and profile_update are deliberately not
// here: they would let a model create an active watch, or change the profile, with one
// tap on a card that summarises it. A later change can add them with their own review.
const (
	TypeThreshold = "threshold_change"
	TypeIntent    = "intent_cleanup"
	TypeMemory    = "memory_fact"
	TypeMute      = "mute_trigger"
)

// MaxItems is how many valid items one reflection keeps.
const MaxItems = 5

// defaultMuteDays is used when a mute item names no duration.
const defaultMuteDays = 7

// unmutable kinds guard the owner's money and positions: a reflection may tune or
// mute what is noise, never what is a safety alarm.
var unmutable = map[string]bool{triggers.KindLiquidationDistance: true, triggers.KindDrawdown: true}

// World is what the items are checked against: the data the model was shown, and no more.
type World struct {
	// Decisions maps the id of every decision shown in today's input to its trigger kind.
	Decisions map[string]string
	// Specs are the trigger specs, by id.
	Specs map[string]triggers.Spec
	// Intents are the open intents shown, by id.
	Intents map[string]intent.Intent
	// Kinds are the trigger kinds that have a spec or a decision today.
	Kinds map[string]bool
}

// Checked is a valid item as the request that makes its proposal.
type Checked struct {
	Request approval.ReflectionRequest // set for every type but memory_fact
	Memory  *approval.MemoryRequest    // set for memory_fact
}

// Verdict is the outcome of Validate: the valid items, how many were dropped, and why
// (counts by reason, for the log; never model text).
type Verdict struct {
	Valid   []Checked
	Dropped int
	Reasons map[string]int
}

// Validate checks every item in code. The model's words decide nothing: a target must
// exist, a change must lie inside the bounds the trigger kind declares, evidence must be
// decisions that were shown today, the rationale is cleaned and capped, and an invalid
// item is dropped and counted. At most MaxItems valid ones are kept.
func Validate(items []proactive.ReflectionItem, w World) Verdict {
	v := Verdict{Reasons: map[string]int{}}
	seen := map[string]bool{}
	for _, it := range items {
		c, key, reason := validateOne(it, w)
		switch {
		case reason != "":
			v.Dropped++
			v.Reasons[reason]++
		case seen[key]:
			v.Dropped++
			v.Reasons["duplicate"]++
		case len(v.Valid) >= MaxItems:
			v.Dropped++
			v.Reasons["over the limit"]++
		default:
			seen[key] = true
			v.Valid = append(v.Valid, c)
		}
	}
	return v
}

func validateOne(it proactive.ReflectionItem, w World) (c Checked, key, reason string) {
	why := approval.CleanReflectionRationale(it.Rationale)
	if why == "" && it.Type != TypeMemory {
		return c, "", "no rationale"
	}
	kinds, reason := evidenceKinds(it.EvidenceIDs, w)
	if reason != "" {
		return c, "", reason
	}
	var change map[string]json.RawMessage
	if len(bytes.TrimSpace(it.Change)) > 0 {
		if json.Unmarshal(it.Change, &change) != nil || change == nil {
			return c, "", "change is not an object"
		}
	}
	target := strings.TrimSpace(it.Target)
	switch it.Type {
	case TypeThreshold:
		return checkThreshold(target, change, kinds, why, w)
	case TypeMute:
		return checkMute(target, change, kinds, why, w)
	case TypeIntent:
		return checkIntent(target, change, why, w)
	case TypeMemory:
		return checkMemory(change)
	}
	return c, "", "type not allowed" // new_watch and profile_update among them
}

// evidenceKinds resolves the cited ids to the trigger kinds of those decisions. Every id
// must be a decision shown in today's input.
func evidenceKinds(ids []string, w World) (map[string]bool, string) {
	if len(ids) > 10 {
		return nil, "too much evidence"
	}
	kinds := map[string]bool{}
	for _, id := range ids {
		k, ok := w.Decisions[id]
		if !ok {
			return nil, "evidence is not a decision of today"
		}
		kinds[k] = true
	}
	return kinds, ""
}

func number(raw json.RawMessage) (float64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func checkThreshold(target string, change map[string]json.RawMessage, kinds map[string]bool, why string, w World) (c Checked, key, reason string) {
	id := target
	if _, ok := w.Specs[id]; !ok { // the trigger id form "<kind>:<spec id>"
		if _, rest, found := strings.Cut(target, ":"); found {
			id = rest
		}
	}
	spec, ok := w.Specs[id]
	switch {
	case !ok:
		return c, "", "target does not exist"
	case triggers.OwnerAlertSpec(spec.Kind, spec.Origin):
		return c, "", "the trigger is an alert the owner set" // a level chosen on purpose
	case spec.IntentID != "":
		return c, "", "the trigger belongs to an intent" // the intent compiler would put it back
	case spec.Mode == triggers.ModeOff:
		return c, "", "the trigger is off"
	case !kinds[spec.Kind]:
		return c, "", "evidence is not about the trigger"
	case len(change) != 1:
		return c, "", "a change sets exactly one parameter"
	}
	for param, raw := range change {
		to, ok := number(raw)
		t, tunable := triggers.SpecBounds(spec)[param]
		switch {
		case !tunable:
			return c, "", "parameter has no declared bounds"
		case !ok || to < t.Min || to > t.Max:
			return c, "", "change is outside the bounds"
		case to == t.Value:
			return c, "", "change changes nothing"
		}
		p := approval.TriggerParamParams{SpecID: spec.ID, Kind: spec.Kind, Param: param, From: t.Value, To: to}
		return Checked{Request: approval.ReflectionRequest{Trigger: &p, Rationale: why}}, "t:" + spec.ID + ":" + param, ""
	}
	return c, "", "empty change"
}

func checkMute(target string, change map[string]json.RawMessage, kinds map[string]bool, why string, w World) (c Checked, key, reason string) {
	days := defaultMuteDays
	if raw, ok := change["days"]; ok {
		f, isNum := number(raw)
		if !isNum || f != math.Trunc(f) || f < 1 || f > approval.MaxMuteDays {
			return c, "", "change is outside the bounds"
		}
		days = int(f)
	}
	if _, rest, found := strings.Cut(target, ":"); found { // "<kind>:<spec id>" names one spec, not a kind
		if s, ok := w.Specs[rest]; ok && triggers.OwnerAlertSpec(s.Kind, s.Origin) {
			return c, "", "the trigger is an alert the owner set"
		}
		return c, "", "a mute names a trigger kind"
	}
	switch {
	case !w.Kinds[target]:
		return c, "", "target does not exist"
	case unmutable[target]:
		return c, "", "a safety alarm cannot be muted"
	case !kinds[target]:
		return c, "", "evidence is not about the trigger"
	}
	p := approval.MuteParams{Prefix: target + ":", Days: days}
	return Checked{Request: approval.ReflectionRequest{Mute: &p, Rationale: why}}, "m:" + target, ""
}

func checkIntent(target string, change map[string]json.RawMessage, why string, w World) (c Checked, key, reason string) {
	it, ok := w.Intents[target]
	if !ok {
		return c, "", "target does not exist"
	}
	var action string
	if raw, ok := change["action"]; !ok || json.Unmarshal(raw, &action) != nil {
		return c, "", "change names no action"
	}
	if action != approval.CleanupPause && action != approval.CleanupCancel {
		return c, "", "change is outside the bounds" // never activate, create or edit
	}
	if action == approval.CleanupPause && it.Status != intent.StatusActive {
		return c, "", "only an active intent can be paused"
	}
	p := approval.IntentCleanupParams{IntentID: it.ID, Action: action, Title: approval.CleanLine(it.Title, 60)}
	return Checked{Request: approval.ReflectionRequest{Intent: &p, Rationale: why}}, "i:" + it.ID, ""
}

func checkMemory(change map[string]json.RawMessage) (c Checked, key, reason string) {
	var fact, category string
	if raw, ok := change["fact"]; !ok || json.Unmarshal(raw, &fact) != nil {
		return c, "", "change names no fact"
	}
	if raw, ok := change["category"]; !ok || json.Unmarshal(raw, &category) != nil || !approval.ValidCategory(category) {
		return c, "", "change names no category"
	}
	clean, err := approval.CleanFact(fact)
	if err != nil {
		return c, "", "fact refused"
	}
	m := approval.MemoryRequest{Fact: clean, Category: category, Source: "reflection", Expiry: approval.ReflectionExpiry}
	return Checked{Memory: &m}, "f:" + clean, ""
}
