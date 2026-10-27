package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/fileutil"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
)

// CallInfo is what the engine knows about one model call.
type CallInfo struct {
	Turn   string // a proactive.TurnKind value, "complete" for providers.Complete, "unknown" if the gateway told nothing
	Origin string
	TurnID string
	Sender string
	Caller string // news | seahorse | complete; "" for turns
	Model  string
	Tools  []string // the tools offered
}

// Faults lets the fault injector (SBX-13) delay or fail a call before a rule answers.
type Faults interface {
	BeforeLLM(ctx context.Context, c CallInfo) (delay time.Duration, err error)
}

// Journal is where the engine records llm.call and llm.complete. It is defined here, in
// terms of plain values, because pkg/sandbox/labjournal (through pkg/replay) imports
// pkg/proactive/engine, whose tests import pkg/providers: a dependency on it would be a cycle.
type Journal interface {
	Record(kind, subject string, payload json.RawMessage)
}

// EngineOptions configures NewEngine.
type EngineOptions struct {
	Journal Journal
	// TurnInfo reads the turn's kind, origin, id and sender from the call's context. The
	// gateway supplies it: this package cannot import pkg/tools. Nil: every turn is "unknown".
	TurnInfo func(ctx context.Context) CallInfo
	Faults   Faults // nil: none
	Store    string // <HOME>/lab/llm_script.json; "" keeps the script in memory only
}

// Engine answers the model calls of one sandbox instance from its script.
type Engine struct {
	o EngineOptions

	mu        sync.Mutex
	rules     []Rule
	fallback  string // fallbackText or fallbackError
	hits      map[string]int
	unmatched int
	loops     map[string]int // turn id → consecutive tool-call responses
	callSeq   int64
}

// scriptFile is what Store holds.
type scriptFile struct {
	Rules    []Rule `json:"rules"`
	Fallback string `json:"fallback,omitempty"`
}

// NewEngine builds an engine. With a Store it starts from the script saved there, so a
// restart of the instance keeps it; a Store that exists but cannot be read is an error.
func NewEngine(o EngineOptions) (*Engine, error) {
	e := &Engine{o: o, fallback: fallbackText, hits: map[string]int{}, loops: map[string]int{}}
	if o.Store == "" {
		return e, nil
	}
	b, err := os.ReadFile(o.Store)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sim: reading the saved script: %w", err)
	}
	var f scriptFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("sim: the saved script %s is not valid: %w", o.Store, err)
	}
	if err := e.install(f.Rules, f.Fallback, false); err != nil {
		return nil, fmt.Errorf("sim: the saved script %s is not valid: %w", o.Store, err)
	}
	return e, nil
}

// Mode is the engine's mode.
func (e *Engine) Mode() string { return ModeScripted }

// Put installs a script: it replaces the rules, or appends to them when replace is
// false. fallback ("text" or "error", "" keeps the current one) says what a call no rule
// matches gets. Hit counts restart. The script is saved to the Store first, so what is
// installed is what a restart finds.
func (e *Engine) Put(rules []Rule, replace bool, fallback string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := rules
	if !replace {
		next = append(append([]Rule(nil), e.rules...), rules...)
	}
	return e.install(next, fallback, true)
}

// install validates and sets the script; the caller holds e.mu (or owns e).
func (e *Engine) install(rules []Rule, fallback string, save bool) error {
	if err := validateRules(rules); err != nil {
		return err
	}
	if fallback != "" && fallback != fallbackText && fallback != fallbackError {
		return fmt.Errorf("fallback must be %q or %q", fallbackText, fallbackError)
	}
	if fallback == "" {
		fallback = e.fallback
	}
	if save && e.o.Store != "" {
		b, err := json.MarshalIndent(scriptFile{Rules: rules, Fallback: fallback}, "", "  ")
		if err == nil {
			err = fileutil.WriteFileAtomic(e.o.Store, b, 0o600)
		}
		if err != nil {
			return fmt.Errorf("saving the script: %w", err)
		}
	}
	e.rules, e.fallback = rules, fallback
	e.hits, e.unmatched = map[string]int{}, 0
	return nil
}

// RuleStat is a rule with the number of times it has answered since the script was put.
type RuleStat struct {
	Rule
	Hits int `json:"hits"`
}

// Script is the installed script and how it has been used.
type Script struct {
	Mode      string     `json:"mode"`
	Fallback  string     `json:"fallback"`
	Rules     []RuleStat `json:"rules"`
	Unmatched int        `json:"unmatched"`
}

// Script returns the installed script with its hit counts.
func (e *Engine) Script() Script {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Script{Mode: ModeScripted, Fallback: e.fallback, Rules: []RuleStat{}, Unmatched: e.unmatched}
	for _, r := range e.rules {
		s.Rules = append(s.Rules, RuleStat{Rule: r, Hits: e.hits[r.ID]})
	}
	return s
}

// decision is the outcome of one call.
type decision struct {
	rule      string
	unmatched bool
	resp      *protocoltypes.LLMResponse
	err       error
}

// Chat answers one model call.
func (e *Engine) Chat(ctx context.Context, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition,
	model string, options map[string]any,
) (*protocoltypes.LLMResponse, error) {
	ci := e.classify(ctx, tools, options)
	ci.Model = model
	if e.o.Faults != nil {
		delay, err := e.o.Faults.BeforeLLM(ctx, ci)
		if err == nil && delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-t.C:
			case <-ctx.Done():
				err = ctx.Err()
			}
			t.Stop()
		}
		if err != nil {
			e.record(ci, msgs, decision{err: err})
			return nil, err
		}
	}
	d := e.answer(ci, msgs)
	e.record(ci, msgs, d)
	return d.resp, d.err
}

func (e *Engine) answer(ci CallInfo, msgs []protocoltypes.Message) decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The history ends with the results of our own tool calls. Only a rule that reads a
	// tool result may answer it again; otherwise the rule that made the calls says what
	// follows. Without this a scripted tool call would be answered by the same rule, for ever.
	if ruleID, ok := followUpOf(msgs); ok {
		if r := e.pick(ci, msgs, true); r != nil {
			return e.respond(ci, r)
		}
		then := defaultThen
		for i := range e.rules {
			if e.rules[i].ID == ruleID && e.rules[i].Respond.ThenText != nil {
				then = *e.rules[i].Respond.ThenText
			}
		}
		delete(e.loops, ci.TurnID)
		return decision{rule: ruleID, resp: stop(then, nil)}
	}
	if r := e.pick(ci, msgs, false); r != nil {
		return e.respond(ci, r)
	}
	e.unmatched++
	if e.fallback == fallbackError {
		return decision{unmatched: true, err: errors.New("sim: no rule matched")}
	}
	return decision{unmatched: true, resp: stop(noMatchText, nil)}
}

// pick returns the first rule that matches and still has uses left, and counts the use.
func (e *Engine) pick(ci CallInfo, msgs []protocoltypes.Message, followUp bool) *Rule {
	for i := range e.rules {
		r := &e.rules[i]
		reads := r.Match.LastTool != "" || len(r.Match.LastToolResult) > 0
		if (followUp && !reads) || (r.Times > 0 && e.hits[r.ID] >= r.Times) || !r.Match.matches(ci, msgs) {
			continue
		}
		e.hits[r.ID]++
		return r
	}
	return nil
}

// respond builds the rule's answer; the caller holds e.mu.
func (e *Engine) respond(ci CallInfo, r *Rule) decision {
	if r.Respond.Error != "" {
		return decision{rule: r.ID, err: errors.New(r.Respond.Error)}
	}
	specs := r.calls()
	if len(specs) == 0 {
		delete(e.loops, ci.TurnID)
		return decision{rule: r.ID, resp: stop(r.Respond.Text, r.Respond.Usage)}
	}
	if ci.TurnID != "" { // without an id the turns cannot be told apart
		if len(e.loops) > 1024 {
			clear(e.loops)
		}
		if e.loops[ci.TurnID]++; e.loops[ci.TurnID] > maxToolRounds {
			delete(e.loops, ci.TurnID)
			return decision{rule: r.ID, err: errors.New("sim: tool loop")}
		}
	}
	resp := &protocoltypes.LLMResponse{Content: r.Respond.Text, FinishReason: "tool_calls", Usage: r.Respond.Usage}
	for _, s := range specs {
		e.callSeq++
		args := argsCopy(s.Args)
		raw, _ := json.Marshal(args)
		resp.ToolCalls = append(resp.ToolCalls, protocoltypes.ToolCall{
			ID: callID(r.ID, e.callSeq), Type: "function", Name: s.Name, Arguments: args,
			Function: &protocoltypes.FunctionCall{Name: s.Name, Arguments: string(raw)},
		})
	}
	return decision{rule: r.ID, resp: resp}
}

func stop(text string, usage *protocoltypes.UsageInfo) *protocoltypes.LLMResponse {
	return &protocoltypes.LLMResponse{Content: text, FinishReason: "stop", Usage: usage}
}

// record writes the call to the journal: llm.complete for providers.Complete, llm.call
// for an agent turn.
func (e *Engine) record(ci CallInfo, msgs []protocoltypes.Message, d decision) {
	if e.o.Journal == nil {
		return
	}
	p := map[string]any{"rule": d.rule, "unmatched": d.unmatched}
	var resp protocoltypes.LLMResponse
	if d.resp != nil {
		resp = *d.resp
	}
	p["text"] = truncate(resp.Content, maxJournalText)
	if d.err != nil {
		p["error"] = d.err.Error()
	}
	kind, subject := "llm.call", ci.Turn
	if ci.Turn == turnComplete {
		kind, subject = "llm.complete", ci.Caller
		p["caller"], p["prompt"] = ci.Caller, truncate(lastContent(msgs, "user"), maxJournalText)
	} else {
		calls := []map[string]any{}
		for _, tc := range resp.ToolCalls {
			calls = append(calls, map[string]any{"name": tc.Name, "args": tc.Arguments})
		}
		p["turn"], p["origin"], p["turn_id"], p["sender"] = ci.Turn, ci.Origin, ci.TurnID, ci.Sender
		p["model"], p["tools"], p["tool_calls"] = ci.Model, append([]string{}, ci.Tools...), calls
	}
	b, _ := json.Marshal(p)
	e.o.Journal.Record(kind, subject, b)
}
