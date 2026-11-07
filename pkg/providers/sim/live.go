package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/protocoltypes"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// ErrBudget is what a live call gets once the budget would be exceeded.
var ErrBudget = errors.New("sim: live budget exhausted")

// LLM is the inner model of record and live: the Chat of a pkg/providers provider, which
// this package cannot import.
type LLM interface {
	Chat(ctx context.Context, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition,
		model string, options map[string]any) (*protocoltypes.LLMResponse, error)
}

// Price is USD per million input and output tokens.
type Price struct{ In, Out float64 }

// DefaultPrices are the list prices this release knows. A model with no price is refused
// for live and record, never guessed; KQ_SANDBOX_LLM_PRICES adds one.
var DefaultPrices = map[string]Price{
	"claude-sonnet-4.6": {In: 3, Out: 15},
}

// LiveOptions is the inner model of one record or live provider. The budget and the model
// allowlist are not here: the budget lives on the Engine (a config reload builds a new
// provider, and spending must not start again from zero), the allowlist is checked where
// the provider is built.
type LiveOptions struct {
	Inner      LLM
	InnerModel string // the model id the inner provider is called with
	Price      Price
	// Key is held only to be removed from the inner provider's errors; it is never
	// journaled, logged or returned.
	Key string
}

// defaultMaxOut is the output a call is assumed to be able to produce when its request
// sets no max_tokens.
const defaultMaxOut = 8192

// BudgetState is what the budget op reports.
type BudgetState struct {
	LimitUSD  float64 `json:"limit_usd"`
	LimitTok  int     `json:"limit_tok"`
	SpentUSD  float64 `json:"spent_usd"`
	SpentTok  int     `json:"spent_tok"`
	Calls     int     `json:"calls"`
	Exhausted bool    `json:"exhausted"`

	// The dearest call so far. A response can report more usage than the request's bytes
	// bound (it should not, but the bound is ours and the report is the provider's), so the
	// next call is reserved at the larger of the two and cannot repeat the surprise.
	maxTok int
	maxUSD float64
}

// Budget reports the budget. Zero limits mean this instance has no live model.
func (e *Engine) Budget() BudgetState {
	e.liveMu.Lock()
	defer e.liveMu.Unlock()
	return e.bud
}

// LowerBudget lowers the limits; a limit that is not below the current one is refused (the
// budget can only shrink once the instance has started). 0 leaves a dimension alone.
func (e *Engine) LowerBudget(usd float64, tok int) (BudgetState, error) {
	if m := e.Mode(); m != ModeLive && m != ModeRecord {
		return BudgetState{}, errors.New("sim: this instance has no live model, so no budget")
	}
	if usd < 0 || tok < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return BudgetState{}, errors.New("sim: a budget limit must be a positive number")
	}
	if usd == 0 && tok == 0 {
		return BudgetState{}, errors.New("sim: give lower_usd or lower_tok")
	}
	e.liveMu.Lock()
	defer e.liveMu.Unlock()
	if usd > 0 && e.bud.LimitUSD > 0 && usd >= e.bud.LimitUSD {
		return e.bud, fmt.Errorf("sim: lower_usd %g is not below the limit %g: the budget can only be lowered", usd, e.bud.LimitUSD)
	}
	if tok > 0 && e.bud.LimitTok > 0 && tok >= e.bud.LimitTok {
		return e.bud, fmt.Errorf("sim: lower_tok %d is not below the limit %d: the budget can only be lowered", tok, e.bud.LimitTok)
	}
	if usd > 0 {
		e.bud.LimitUSD = usd
	}
	if tok > 0 {
		e.bud.LimitTok = tok
	}
	e.budgetEventLocked("lowered")
	return e.bud, nil
}

// budgetEventLocked journals llm.budget; the caller holds liveMu.
func (e *Engine) budgetEventLocked(event string) {
	if e.o.Journal == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"event": event, "limit_usd": e.bud.LimitUSD, "limit_tok": e.bud.LimitTok,
		"spent_usd": e.bud.SpentUSD, "spent_tok": e.bud.SpentTok, "calls": e.bud.Calls})
	e.o.Journal.Record("llm.budget", e.Mode(), b)
}

// exceeds says whether spending more would pass a limit.
func (b BudgetState) exceeds(usd float64, tok int) bool {
	return (b.LimitUSD > 0 && b.SpentUSD+usd > b.LimitUSD) || (b.LimitTok > 0 && b.SpentTok+tok > b.LimitTok)
}

// bound is the most a call can cost, as an upper bound and not a guess: a token is at
// least one byte, so the request's bytes bound its input tokens, and the output is bounded
// by the request's max_tokens. Used to refuse a call before it is made, and for the cost of
// a call whose response reports no usage.
func bound(msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition, options map[string]any, p Price) (tok int, usd float64) {
	in := len(mustJSON(msgs)) + len(mustJSON(tools))
	out := defaultMaxOut
	switch v := options["max_tokens"].(type) {
	case int:
		out = v
	case int64:
		out = int(v)
	case float64:
		out = int(v)
	}
	return in + out, (float64(in)*p.In + float64(out)*p.Out) / 1e6
}

// live answers from the real model under the budget. Calls are serialised: the budget is
// checked and the spend accounted while holding liveMu, so two turns cannot both pass the
// check before either is counted.
func (e *Engine) live(ctx context.Context, lo *LiveOptions, msgs []protocoltypes.Message, tools []protocoltypes.ToolDefinition,
	options map[string]any, creq CanonicalRequest, key string,
) decision {
	if lo == nil || lo.Inner == nil {
		return decision{rule: "live", err: errors.New("sim: this provider has no inner model")}
	}
	e.liveMu.Lock()
	defer e.liveMu.Unlock()

	reserveTok, reserveUSD := bound(msgs, tools, options, lo.Price)
	reserveTok, reserveUSD = max(reserveTok, e.bud.maxTok), max(reserveUSD, e.bud.maxUSD)
	if e.bud.Exhausted || e.bud.exceeds(reserveUSD, reserveTok) {
		if !e.bud.Exhausted {
			e.bud.Exhausted = true // sticky: later calls are refused too
		}
		e.budgetEventLocked("refused")
		return decision{rule: "live", err: ErrBudget, extra: map[string]any{"mode": e.Mode()}}
	}

	resp, err := lo.Inner.Chat(ctx, msgs, tools, lo.InnerModel, options)
	e.bud.Calls++
	if err != nil {
		// The request may have been processed: charge its input at the upper bound.
		inTok := len(mustJSON(msgs)) + len(mustJSON(tools))
		e.spendLocked(inTok, float64(inTok)*lo.Price.In/1e6)
		return decision{rule: "live", err: scrub(err, lo.Key), extra: map[string]any{"mode": e.Mode(), "spent_usd": e.bud.SpentUSD}}
	}

	tok, usd, estimated := 0, 0.0, false
	if u := resp.Usage; u != nil && u.PromptTokens+u.CompletionTokens > 0 {
		tok = u.PromptTokens + u.CompletionTokens
		usd = (float64(u.PromptTokens)*lo.Price.In + float64(u.CompletionTokens)*lo.Price.Out) / 1e6
	} else { // no usage reported: the upper bound, with the output bounded by what came back
		estimated = true
		inTok := len(mustJSON(msgs)) + len(mustJSON(tools))
		outTok := len(resp.Content) + len(resp.ReasoningContent) + len(mustJSON(resp.ToolCalls))
		tok, usd = inTok+outTok, (float64(inTok)*lo.Price.In+float64(outTok)*lo.Price.Out)/1e6
	}
	e.spendLocked(tok, usd)
	d := decision{rule: "live", resp: resp, extra: map[string]any{
		"mode": e.Mode(), "usage": resp.Usage, "cost_usd": usd, "spent_usd": e.bud.SpentUSD, "usage_estimated": estimated,
	}}
	if e.Mode() == ModeRecord && e.o.RecordPath != "" {
		if err := AppendEntry(e.o.RecordPath, Entry{Key: key, Request: creq, Response: ReplyOf(resp), Usage: resp.Usage}); err != nil {
			d.resp, d.err = nil, fmt.Errorf("sim: recording the cassette: %w", err)
		}
	}
	return d
}

// spendLocked accounts a call and flags the budget exhausted once a limit is reached.
func (e *Engine) spendLocked(tok int, usd float64) {
	e.bud.SpentTok += tok
	e.bud.SpentUSD += usd
	e.bud.maxTok, e.bud.maxUSD = max(e.bud.maxTok, tok), max(e.bud.maxUSD, usd)
	if !e.bud.Exhausted && (e.bud.LimitUSD > 0 && e.bud.SpentUSD >= e.bud.LimitUSD || e.bud.LimitTok > 0 && e.bud.SpentTok >= e.bud.LimitTok) {
		e.bud.Exhausted = true
		e.budgetEventLocked("exhausted")
	}
}

// scrub removes the key from an error before anything sees it.
func scrub(err error, key string) error {
	msg := err.Error()
	if key == "" || !strings.Contains(msg, key) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, key, "[redacted]"))
}

// AppendEntry appends one entry to a JSONL cassette, creating its directory (0700) and
// the file (0600).
func AppendEntry(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// LiveEnv is the environment of a record or live instance, read once from the process
// environment by the code that builds the provider and the engine.
type LiveEnv struct {
	Key       string
	Models    []string
	BudgetUSD float64
	BudgetTok int
	Prices    map[string]Price
	APIBase   string
}

// LiveEnvFrom reads and checks the environment. It never puts the key in an error.
func LiveEnvFrom(getenv func(string) string) (LiveEnv, error) {
	le := LiveEnv{Key: getenv(sandbox.EnvLLMKey), Models: sandbox.ParseLLMModels(getenv(sandbox.EnvLLMModels)),
		APIBase: getenv(sandbox.EnvLLMAPIBase), Prices: map[string]Price{}}
	for m, p := range DefaultPrices {
		le.Prices[m] = p
	}
	extra, err := sandbox.ParseLLMPrices(getenv(sandbox.EnvLLMPrices))
	if err != nil {
		return le, fmt.Errorf("%s: %w", sandbox.EnvLLMPrices, err)
	}
	for m, p := range extra {
		le.Prices[m] = Price{In: p[0], Out: p[1]}
	}
	if b := getenv(sandbox.EnvLLMBudget); b != "" {
		if le.BudgetUSD, le.BudgetTok, err = sandbox.ParseLLMBudget(b); err != nil {
			return le, fmt.Errorf("%s: %w", sandbox.EnvLLMBudget, err)
		}
	}
	return le, nil
}
