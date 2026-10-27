// Package llmops is the lab group "llm": script the simulated model (pkg/providers/sim).
// It is a package of its own because sim is imported by pkg/providers, and lab reaches
// pkg/proactive/engine, whose tests import pkg/providers.
package llmops

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// Group script the simulated model and read what it was asked. (What each call asked is
// in the journal: llm.call and llm.complete.)
func Group(e *sim.Engine) lab.Group {
	return lab.Group{
		Name: "llm",
		Description: "Script the simulated LLM: ordered match/respond rules answer every model call " +
			"(agent turns and providers.Complete). The journal records each call as llm.call / llm.complete.",
		Ops: []lab.Op{
			{
				Name:        "mode",
				Description: "get or set the model mode; only scripted exists yet",
				Args:        json.RawMessage(`{"set":{"type":"string","enum":["scripted"]}}`),
				Run:         modeOp(e),
			},
			{
				Name: "script_put",
				Description: "install rules (first match wins). match: turn (user, proactive, heartbeat_legacy, cron_legacy, " +
					"approval_followup, reflection, subturn, complete), origin, complete (news|seahorse|complete), prompt_contains[], " +
					"system_contains[], tools_include[], last_tool, last_tool_result_contains[], sender. respond: text, tool+args, " +
					"tool_calls[{name,args}], then_text, error, usage. Args are sent as written, schema or not.",
				Args: json.RawMessage(`{"rules":{"type":"array","items":{"type":"object"},"description":"[{id, match, respond, times?}]"},` +
					`"replace":{"type":"boolean","description":"default true; false appends to the installed rules"},` +
					`"fallback":{"type":"string","enum":["text","error"],"description":"what a call no rule matches gets"}}`),
				Required: []string{"rules"},
				Run:      putOp(e),
			},
			{Name: "script_list", Description: "the installed rules with their hit counts, and how many calls matched none", Run: listOp(e)},
		},
	}
}

func modeOp(e *sim.Engine) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct{ Set string }
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		if a.Set != "" && a.Set != sim.ModeScripted {
			return nil, lab.Errorf(lab.CodeUnavailable, "mode %q is not available yet (only %q)", a.Set, sim.ModeScripted)
		}
		return map[string]string{"mode": e.Mode()}, nil
	}
}

func putOp(e *sim.Engine) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Rules    json.RawMessage `json:"rules"`
			Replace  *bool           `json:"replace"`
			Fallback string          `json:"fallback"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		// Strict: a misspelt match field would otherwise match every call.
		var rules []sim.Rule
		dec := json.NewDecoder(bytes.NewReader(a.Rules))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&rules); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "rules: %v", err)
		}
		if err := e.Put(rules, a.Replace == nil || *a.Replace, a.Fallback); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		s := e.Script()
		return map[string]any{"rules": len(s.Rules), "fallback": s.Fallback}, nil
	}
}

func listOp(e *sim.Engine) lab.OpFunc {
	return func(context.Context, json.RawMessage) (any, error) { return e.Script(), nil }
}
