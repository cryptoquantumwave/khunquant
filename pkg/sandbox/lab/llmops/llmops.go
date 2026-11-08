// Package llmops is the lab group "llm": script the simulated model (pkg/providers/sim).
// It is a package of its own because sim is imported by pkg/providers, and lab reaches
// pkg/proactive/engine, whose tests import pkg/providers.
package llmops

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/sim"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/lab"
)

// Group script the simulated model and read what it was asked. (What each call asked is
// in the journal: llm.call and llm.complete.)
func Group(e *sim.Engine, home string) lab.Group {
	return lab.Group{
		Name: "llm",
		Description: "Script the simulated LLM: ordered match/respond rules answer every model call " +
			"(agent turns and providers.Complete). The journal records each call as llm.call / llm.complete.",
		Ops: []lab.Op{
			{
				Name:        "mode",
				Description: "the model mode (scripted, replay, record or live), fixed when the instance started; set only restates it",
				Args:        json.RawMessage(`{"set":{"type":"string","enum":["scripted","replay","record","live"]}}`),
				Run:         modeOp(e),
			},
			{
				Name: "cassette_load",
				Description: "replace the replay set: path (a JSONL file inside the instance home) or inline[] (cassette entries), " +
					"miss: error (default; a request with no entry fails and the journal names its key) or scripted (the rules answer)",
				Args: json.RawMessage(`{"path":{"type":"string"},"inline":{"type":"array","items":{"type":"object"}},` +
					`"miss":{"type":"string","enum":["error","scripted"]}}`),
				Run: cassetteLoadOp(e, home),
			},
			{
				Name: "budget",
				Description: "a live or record instance's budget: {limit_usd, limit_tok, spent_usd, spent_tok, calls, exhausted}. " +
					"lower_usd / lower_tok lower a limit (never raise it: the start-time budget is the ceiling); a call that would pass " +
					"a limit is refused with `sim: live budget exhausted` and journaled as llm.budget",
				Args: json.RawMessage(`{"lower_usd":{"type":"number"},"lower_tok":{"type":"integer"}}`),
				Run:  budgetOp(e),
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

// modeOp reports the mode. The mode is the instance's, fixed at start by --llm (it
// decides what the process may reach and what keys it holds), so set only restates it.
func modeOp(e *sim.Engine) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct{ Set string }
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		if a.Set != "" && a.Set != e.Mode() {
			return nil, lab.Errorf(lab.CodeUnavailable, "this instance runs %q: the mode is chosen at start (sandbox up --llm), not switched", e.Mode())
		}
		return map[string]string{"mode": e.Mode()}, nil
	}
}

// budgetOp reports the budget and lowers it; a raise is refused.
func budgetOp(e *sim.Engine) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			LowerUSD float64 `json:"lower_usd"`
			LowerTok int     `json:"lower_tok"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		if m := e.Mode(); m != sim.ModeLive && m != sim.ModeRecord {
			return nil, lab.Errorf(lab.CodeUnavailable, "this instance runs %q: there is no live model and no budget", m)
		}
		if a.LowerUSD == 0 && a.LowerTok == 0 {
			return e.Budget(), nil
		}
		st, err := e.LowerBudget(a.LowerUSD, a.LowerTok)
		if err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		return st, nil
	}
}

// maxCassetteFile bounds a cassette read from disk.
const maxCassetteFile = 16 << 20

// cassetteLoadOp replaces the replay set from a file inside the instance home or from
// inline entries. A relative path is relative to the home (the gateway's working directory).
func cassetteLoadOp(e *sim.Engine, home string) lab.OpFunc {
	return func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct {
			Path   string            `json:"path"`
			Inline []json.RawMessage `json:"inline"`
			Miss   string            `json:"miss"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		if (a.Path == "") == (len(a.Inline) == 0) {
			return nil, lab.Errorf(lab.CodeBadArgs, "give exactly one of path and inline")
		}
		var entries []sim.Entry
		var err error
		if a.Path != "" {
			p := a.Path
			if !filepath.IsAbs(p) {
				p = filepath.Join(home, p)
			}
			if home == "" || !sandbox.PathInside(home, p) {
				return nil, lab.Errorf(lab.CodeBadArgs, "path %q is outside the instance home", a.Path)
			}
			f, openErr := os.Open(p)
			if openErr != nil {
				return nil, lab.Errorf(lab.CodeBadArgs, "%v", openErr)
			}
			defer f.Close()
			entries, err = sim.ParseEntries(io.LimitReader(f, maxCassetteFile))
		} else {
			entries, err = sim.ParseEntries(strings.NewReader(joinLines(a.Inline)))
		}
		if err == nil {
			err = e.LoadCassette(entries, a.Miss)
		}
		if err != nil {
			return nil, lab.Errorf(lab.CodeBadArgs, "%v", err)
		}
		n, miss := e.CassetteStat()
		return map[string]any{"entries": n, "miss": miss}, nil
	}
}

func joinLines(raw []json.RawMessage) string {
	var b strings.Builder
	for _, r := range raw {
		var one bytes.Buffer
		if json.Compact(&one, r) == nil { // a line per entry; a malformed one fails the parse
			r = one.Bytes()
		}
		b.Write(r)
		b.WriteByte('\n')
	}
	return b.String()
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
