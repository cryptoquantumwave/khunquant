package scenario

//go:generate go run ./internal/schemagen -o ../../../testdata/scenarios/schema.v1.json

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

// SchemaJSON is the JSON Schema of a scenario file, generated from File. It describes
// the shape for editors and agents; Parse and Validate stay the authority. The schema
// is deliberately loose wherever the loader's rule is a grammar or depends on other
// fields (count, durations, offsets, journal values, unique ids, step order), so it
// never rejects a file the loader accepts; an optional field may be null and a closed
// set may be "", which the loader reads as unset.
func SchemaJSON() ([]byte, error) {
	r := &jsonschema.Reflector{FieldNameTag: "yaml", RequiredFromJSONSchemaTags: true, ExpandedStruct: true}
	s := r.Reflect(&File{})
	seen := map[*jsonschema.Schema]bool{}
	looseUnset(s, seen)
	for _, d := range s.Definitions {
		looseUnset(d, seen)
	}
	s.ID ="https://github.com/cryptoquantumwave/khunquant/testdata/scenarios/schema.v1.json"
	s.Title = "KhunQuant Sandbox Lab scenario (schema v1)"
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// looseUnset lets every optional property be null and every closed set hold "": the
// loader reads both as "not given", so the schema must not reject them. Required
// properties keep their strict form.
func looseUnset(s *jsonschema.Schema, seen map[*jsonschema.Schema]bool) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true
	if s.Properties == nil {
		return
	}
	for pair := s.Properties.Oldest(); pair != nil; pair = pair.Next() {
		p := pair.Value
		if p == jsonschema.TrueSchema || p == jsonschema.FalseSchema || p == nil {
			continue
		}
		looseUnset(p, seen)
		looseUnset(p.Items, seen)
		if len(p.Enum) > 0 {
			p.Enum = append(p.Enum, "")
		}
		if slices.Contains(s.Required, pair.Key) {
			continue
		}
		inner := *p
		inner.Description = ""
		pair.Value = &jsonschema.Schema{Description: p.Description, AnyOf: []*jsonschema.Schema{&inner, {Type: "null"}}}
	}
}

// scalar is any YAML scalar: the loader reads a number or a bool as its text.
func scalar(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Description: desc, AnyOf: []*jsonschema.Schema{{Type: "string"}, {Type: "number"}}}
}

func props(kv ...any) *orderedmap.OrderedMap[string, *jsonschema.Schema] {
	m := orderedmap.New[string, *jsonschema.Schema]()
	for i := 0; i+1 < len(kv); i += 2 {
		m.Set(kv[i].(string), kv[i+1].(*jsonschema.Schema))
	}
	return m
}

func names(keys ...string) []any {
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, k)
	}
	return out
}

var (
	no  = jsonschema.FalseSchema
	yes = jsonschema.TrueSchema
)

// JSONSchema implements jsonschema's custom-schema hook: a step is a mapping with one
// kind key, whose value depends on the kind, and an optional at.
func (Step) JSONSchema() *jsonschema.Schema {
	kinds := sortedKeys(stepKinds)
	p := props("at", scalar("Offset from clock.start, such as +5m. Timeline only; offsets must not decrease."))
	for _, k := range kinds {
		spec := stepKinds[k]
		d := "Runner step."
		if spec.Group != "" {
			d = "Served by sandbox_" + spec.Group
			if spec.Op != "" {
				d += " op " + spec.Op
			}
			d += " (" + spec.PR + ")."
		}
		s := &jsonschema.Schema{Description: d}
		if len(spec.Req) > 0 { // the loader reads these from a mapping and reports each missing one
			s.Type, s.Required = "object", spec.Req
		}
		p.Set(k, s)
	}
	one := uint64(1)
	return &jsonschema.Schema{
		Type: "object", Properties: p, MinProperties: &one,
		PropertyNames: &jsonschema.Schema{Enum: names(append([]string{"at"}, kinds...)...)},
		Description:   "One kind key (chat, market.path, advance, ...) and an optional at.",
	}
}

// JSONSchema implements the custom-schema hook: an expectation is one kind key, an
// optional id and the count / within / during / replay_match modifiers, which may sit
// inside the kind's mapping or beside it.
func (Expectation) JSONSchema() *jsonschema.Schema {
	mods := props(
		"count", scalar(`How many matching entries: 1, ">= 1", "<= 2" or "0" for absence.`),
		"within", &jsonschema.Schema{Type: "object", Description: "Sim-time window as offsets from clock.start.", AdditionalProperties: no,
			Properties: props("from", scalar("Start offset; 0 when empty."), "to", scalar("End offset; open when empty."))},
		"during", &jsonschema.Schema{Type: "string", Description: "HH:MM-HH:MM in the profile time zone; wraps midnight when the end is not after the start."},
		"replay_match", &jsonschema.Schema{Type: "string", Description: "message with count: 0 and a within with from and to only: the should-not-fire label this expectation exports as a replay case (export_replay); the run verdict ignores it."},
	)
	p := props("id", scalar("Unique id, shown in the report."))
	for pair := mods.Oldest(); pair != nil; pair = pair.Next() {
		p.Set(pair.Key, pair.Value)
	}
	keys := sortedKeys(expectSpec)
	for _, k := range keys {
		kp := props()
		for pair := mods.Oldest(); pair != nil; pair = pair.Next() {
			kp.Set(pair.Key, pair.Value)
		}
		for _, f := range expectSpec[k].Fields {
			kp.Set(f, yes)
		}
		// No type: the loader also accepts a scalar or nothing here (logs: {}), so only a mapping is checked.
		p.Set(k, &jsonschema.Schema{Description: "Expectation of kind " + k + ".", Properties: kp, AdditionalProperties: no})
	}
	one := uint64(1)
	return &jsonschema.Schema{
		Type: "object", Properties: p, MinProperties: &one,
		PropertyNames: &jsonschema.Schema{Enum: names(append([]string{"id", "count", "within", "during", "replay_match"}, keys...)...)},
		Description:   "One kind key (" + strings.Join(keys, ", ") + ") and optional id and modifiers.",
	}
}

// JSONSchema implements the custom-schema hook: venue and model, then the model op's own arguments.
func (MarketInit) JSONSchema() *jsonschema.Schema {
	models := []string{"set", "walk", "replay", "recording"}
	slices.Sort(models)
	return &jsonschema.Schema{
		Type: "object", AdditionalProperties: yes,
		Description: "Starting price model; every other key is the model op's argument (price, vol, seed, ...).",
		Properties: props(
			"venue", &jsonschema.Schema{Type: "string", Description: "Venue of the symbol; must match the account."},
			"model", &jsonschema.Schema{Type: "string", Enum: names(models...), Description: "Market op that sets the symbol up (set by default)."},
		),
	}
}
