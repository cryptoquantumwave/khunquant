package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// KindGoalDiscrepancy is the goal discrepancy trigger: it notices that the world no
// longer matches the assumption behind a goal ("you wanted crypto under 30%; it is
// 41%"; "your delta-neutral plan assumed funding above 10% APY; it has been below 4%").
// One spec per ACTIVE goal that carries an expectation, compiled from the intent by
// pkg/triggers/intents with the origin "intent:<id>". It notifies and never acts.
//
// An expectation is a true/false expression of the world model's expectation language
// (worldmodel.CompileExpr). The trigger judges it at most once per window while the
// answer is known:
//
//   - TRUE: the goal holds. It arms the trigger (a goal that was never seen to hold has
//     no assumption to break) and, after a firing, re-arms it.
//   - FALSE, armed and not yet fired: one more false check. At Consecutive of them in a
//     row it fires ONCE and does not fire again until the expectation has been true.
//   - UNKNOWN (a fact the expression reads is missing, stale, restored, below exchange
//     trust or not finite): nothing. It is not a false check, it does not break a run of
//     them, it does not re-arm, and it does not use the window up (it is retried after
//     unknownRetry).
//
// What reaches a model is code-built: the title, subject and detail are made from
// numbers and validated names, the evidence is the intent's id, the number of false
// checks and the values the expression read. The goal's title and description, which the
// owner wrote, travel only in the Quote; the expression's text appears nowhere.
const KindGoalDiscrepancy = "goal_discrepancy"

const (
	goalDefaultConsecutive = 2
	goalDefaultEvery       = 10 * time.Minute
	// goalUnknownRetry is how soon an unknown evaluation is tried again.
	goalUnknownRetry = 30 * time.Second
	goalDedupWindow  = 24 * time.Hour
	maxGoalTextRunes = 300
)

// GoalParams are the parameters of a goal_discrepancy spec.
//
// Title and Description are the owner's words about the goal: outside text, cleaned
// and capped, shown only as a quotation (Candidate.Quote).
type GoalParams struct {
	Expr string `json:"expr"`
	// Severity is "info", "notice" (the default) or "warning". Never critical.
	Severity string `json:"severity,omitempty"`
	// Consecutive is how many false checks in a row fire it: 0 for the default (2), or 1..10.
	Consecutive int `json:"consecutive,omitempty"`
	// EveryMinutes is the least time between two checks: 0 for the default (10), or 1..1440.
	EveryMinutes int    `json:"every_minutes,omitempty"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
}

func (p GoalParams) severity() proactive.Severity {
	if p.Severity == "" {
		return proactive.SeverityNotice
	}
	s, _ := proactive.ParseSeverity(p.Severity)
	return s
}

func (p GoalParams) consecutive() int {
	if p.Consecutive == 0 {
		return goalDefaultConsecutive
	}
	return p.Consecutive
}

func (p GoalParams) every() time.Duration {
	if p.EveryMinutes == 0 {
		return goalDefaultEvery
	}
	return time.Duration(p.EveryMinutes) * time.Minute
}

// Validate checks the parameters, compiling the expression.
func (p GoalParams) Validate() error {
	if _, err := worldmodel.CompileExpr(p.Expr); err != nil {
		return err
	}
	if p.Severity != "" {
		if s, err := proactive.ParseSeverity(p.Severity); err != nil || s > proactive.SeverityWarning {
			return fmt.Errorf("severity must be info, notice or warning")
		}
	}
	if p.Consecutive < 0 || p.Consecutive > 10 {
		return fmt.Errorf("consecutive must be from 1 to 10")
	}
	if p.EveryMinutes < 0 || p.EveryMinutes > 24*60 {
		return fmt.Errorf("every_minutes must be from 1 to 1440")
	}
	for _, s := range []string{p.Title, p.Description} {
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxGoalTextRunes {
			return fmt.Errorf("title and description must be valid text of at most %d characters", maxGoalTextRunes)
		}
	}
	return nil
}

// JSON encodes the parameters for a Spec.
func (p GoalParams) JSON() json.RawMessage {
	b, _ := json.Marshal(p) // plain fields: cannot fail
	return b
}

// GoalConfig is what the goal_discrepancy kind is given by the gateway.
type GoalConfig struct {
	// Language returns "th" for Thai, anything else for English. Nil means English.
	Language func() string
	// Providers are the providers price() looks a symbol up on, in order. Nil: price()
	// is unknown.
	Providers func() []string
	// Location is the owner's zone, for hour_local(); it is called only when an
	// expression uses it. Nil or nil result: UTC.
	Location func() *time.Location
	// Stats receives the counters. Nil counts nothing.
	Stats *GoalStats
}

// GoalStats count what the kind did. Counts only.
type GoalStats struct {
	Checked, Unknown, Failed, Fired, Rearmed atomic.Uint64
}

// GoalDiscrepancyFactory builds goal_discrepancy triggers.
func GoalDiscrepancyFactory(cfg GoalConfig) Factory {
	if cfg.Stats == nil {
		cfg.Stats = &GoalStats{}
	}
	return func(spec Spec) (Trigger, error) {
		var p GoalParams
		if err := decodeStrict(spec.Params, &p); err != nil {
			return nil, err
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		prog, err := worldmodel.CompileExpr(p.Expr)
		if err != nil {
			return nil, err
		}
		subject := spec.IntentID
		if subject == "" {
			subject = spec.ID
		}
		return &goalDiscrepancy{id: spec.Kind + ":" + spec.ID, subject: subject, p: p, cfg: cfg, prog: prog}, nil
	}
}

type goalState struct {
	// Held: the expectation has been true at least once since the spec was made.
	Held bool `json:"held,omitempty"`
	// False is the run of known false checks since it was last true.
	False int `json:"false,omitempty"`
	// Fired: it has fired for this run and waits for the expectation to hold again.
	Fired bool `json:"fired,omitempty"`
}

type goalDiscrepancy struct {
	id      string
	subject string // the intent's id
	p       GoalParams
	cfg     GoalConfig
	prog    *worldmodel.ExprProgram

	mu     sync.Mutex
	nextAt time.Time // the earliest the next check; in memory only
}

func (t *goalDiscrepancy) ID() string { return t.id }

// Kinds are the events whose facts an expectation reads: a price, the portfolio, a
// plan's health. The window (not the event rate) decides how often it is judged.
func (t *goalDiscrepancy) Kinds() []events.Kind {
	return []events.Kind{
		events.KindPerceptionMarketTick, events.KindPerceptionPortfolioSnapshot, events.KindPerceptionPlanHealth,
	}
}

// Demands: none. The expression reads what the world model already holds; it never
// makes a source read anything.
func (t *goalDiscrepancy) Demands() []perception.Demand { return nil }

// due reports whether a check may run now: the window since the last known check has
// passed. A clock that moved back does not make it wait more than one window.
func (t *goalDiscrepancy) due(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nextAt.IsZero() || !now.Before(t.nextAt) || t.nextAt.Sub(now) > t.p.every()
}

func (t *goalDiscrepancy) next(at time.Time) {
	t.mu.Lock()
	t.nextAt = at
	t.mu.Unlock()
}

func (t *goalDiscrepancy) Evaluate(ctx context.Context, v worldmodel.View, d worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	now := v.Now()
	if !t.due(now) {
		return nil, nil
	}
	ectx, cancel := context.WithTimeout(ctx, worldmodel.ExprTimeBound)
	defer cancel()
	sc := worldmodel.ExprEnv(v, worldmodel.WithExprProviders(t.cfg.Providers), worldmodel.WithExprLocation(t.cfg.Location))
	holds, known, err := t.prog.Eval(ectx, sc)
	t.cfg.Stats.Checked.Add(1)
	if err != nil {
		t.cfg.Stats.Failed.Add(1)
	}
	if !known {
		t.cfg.Stats.Unknown.Add(1)
		t.next(now.Add(goalUnknownRetry))
		return nil, nil
	}
	t.next(now.Add(t.p.every()))

	var s goalState
	if len(st.Raw) > 0 && json.Unmarshal(st.Raw, &s) != nil {
		s = goalState{}
	}
	prev := s
	var out []proactive.Candidate
	switch {
	case holds:
		if s.Fired {
			t.cfg.Stats.Rearmed.Add(1)
		}
		s = goalState{Held: true}
	case !s.Held || s.Fired:
		// never seen to hold, or already said: nothing to count
	default:
		s.False++
		if s.False >= t.p.consecutive() {
			s.Fired = true
			out = append(out, t.candidate(s.False, sc, MinInputTrust(d, sc.Trust()), now))
			t.cfg.Stats.Fired.Add(1)
		}
	}
	if s != prev {
		marshalState(st, s)
	}
	return out, nil
}

// candidate builds the candidate. Nothing the owner or a model wrote is in it but the
// Quote: the numbers are the ones the expression read, their names were built by code
// from validated arguments, and the intent's id is the ledger's.
func (t *goalDiscrepancy) candidate(falses int, sc *worldmodel.ExprScope, trust proactive.Trust, now time.Time) proactive.Candidate {
	sev := t.p.severity()
	thai := CrossConfig{Language: t.cfg.Language}.isThai()
	refs := sc.Refs()
	vars := make(map[string]any, len(refs))
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		vars[r.Name] = r.Value
		parts = append(parts, r.Name+" = "+fmtNumber(r.Value))
	}
	n := strconv.Itoa(falses)
	title := "A goal no longer holds: its check has failed " + n + " times in a row"
	if thai {
		title = "เป้าหมายไม่เป็นไปตามที่คาด: ตรวจไม่ผ่านติดกัน " + n + " ครั้ง"
	}
	every := strconv.Itoa(int(t.p.every() / time.Minute))
	detail := "checked every " + every + " min"
	if thai {
		detail = "ตรวจทุก " + every + " นาที"
	}
	if len(parts) > 0 {
		detail += " · " + strings.Join(parts, " · ")
	}
	var quote []string
	for _, s := range []string{t.p.Title, t.p.Description} {
		if c := CleanAlertMessage(s); c != "" {
			quote = append(quote, c)
		}
	}
	return proactive.Candidate{
		DedupKey: GoalKey(t.subject),
		Severity: sev, Salience: 0.3 + 0.2*float64(sev), Subject: "goal",
		Title: title, Detail: detail, Quote: strings.Join(quote, "\n"),
		Evidence: map[string]any{
			"intent_id": t.subject, "kind": KindGoalDiscrepancy, "consecutive_false": falses,
			"required": t.p.consecutive(), "vars": vars,
		},
		Trust: trust, Compose: proactive.ComposeLLM, DedupWindow: goalDedupWindow, At: now,
	}
}

// GoalKey is goal_discrepancy:<intent id>:goal. The registry requires the kind as the
// prefix and a variant after the subject.
func GoalKey(intentID string) string {
	return KindGoalDiscrepancy + ":" + cleanKeyToken(intentID) + ":goal"
}
