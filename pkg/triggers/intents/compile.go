// Package intents compiles the intent ledger (pkg/intent) into trigger specs
// (pkg/triggers) and keeps the two in step.
//
// The ledger is the source of truth; the specs made from it are derived and can
// always be rebuilt from it. An ACTIVE watch becomes a price_cross or
// indicator_cross spec, an ACTIVE reminder a time_at spec with a timer on the clock
// source, an ACTIVE standing order its trigger as a Notice that proposes nothing and
// acts on nothing, an ACTIVE goal that carries an expectation a goal_discrepancy spec
// (it fires when the world no longer matches what the goal assumed). A paused
// intent's spec is switched off (its state is kept); the spec of a cancelled, done or
// expired intent is removed; a spec with an intent origin and no live intent is an
// orphan and is removed. A goal without an expectation, and a preference, compile to
// nothing: they are context.
//
// It lives here, next to pkg/triggers/migrate, and not in pkg/intent because it must
// see both: pkg/triggers does not import pkg/intent, and pkg/intent must not import
// the trigger registry.
package intents

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// MinReminderInterval is the least time between two firings of one recurring
// reminder.
const MinReminderInterval = time.Minute

// defaultZone is the owner's zone when the profile gives none (the clock source's).
const defaultZone = "Asia/Bangkok"

// Compilable reports whether an intent of this type is compiled into a trigger.
func Compilable(t intent.IntentType) bool {
	return t == intent.TypeWatch || t == intent.TypeStandingOrder || t == intent.TypeReminder
}

// Compiles reports whether this intent is compiled into a trigger: a watch, a standing
// order or a reminder, or a goal that carries an expectation.
func Compiles(it intent.Intent) bool {
	return Compilable(it.Type) || (it.Type == intent.TypeGoal && it.Expected != nil)
}

// TriggerKinds are the trigger kinds a watch or a standing order can use.
func TriggerKinds() []string { return []string{triggers.KindPriceCross, triggers.KindIndicatorCross} }

// BadKindMessage is the sentence that names the kinds that work.
func BadKindMessage(kind string) string {
	return fmt.Sprintf("a watch or standing order can use only a %s or an %s trigger; %q is not one that works yet",
		triggers.KindPriceCross, triggers.KindIndicatorCross, oneLine(kind, 40))
}

// Env is what compiling needs besides the intent.
type Env struct {
	// Now is the present; a reminder given as {in} counts from ActivatedAt.
	Now time.Time
	// Loc is the owner's time zone.
	Loc *time.Location
	// ActivatedAt is when the intent first became active (Now for one that is about
	// to).
	ActivatedAt time.Time
}

// Plan is what an intent compiles to.
type Plan struct {
	Kind   string
	Params json.RawMessage
	Origin string
	// Timer is the timer a reminder needs (nil for the others). Its ID is filled by
	// the compiler; for a recurring reminder At is the zero time and the compiler
	// sets the next occurrence.
	Timer *perception.Timer
	// Due is the absolute due time of a one-shot reminder (zero otherwise).
	Due time.Time
}

// ErrNotCompiled is returned for an intent type that compiles to nothing.
var ErrNotCompiled = errors.New("intents: this type compiles to nothing")

// Compile turns an intent into the spec (and timer) that carry it out. It is pure:
// it reads the intent and env only. The error text is for the owner, built by code
// from what the intent said (never a raw parse error that could echo model text).
func Compile(it intent.Intent, env Env) (Plan, error) {
	switch it.Type {
	case intent.TypeWatch, intent.TypeStandingOrder:
		return compileWatch(it)
	case intent.TypeReminder:
		return compileReminder(it, env)
	case intent.TypeGoal:
		if it.Expected != nil {
			return compileGoal(it)
		}
	}
	return Plan{}, ErrNotCompiled
}

// compileGoal makes the goal_discrepancy spec of a goal's expectation. The expectation
// is compiled here (the same way the trigger's factory will), so a bad one is heard
// about when the goal is made. The goal's title and description travel in the params
// for the candidate's Quote and nowhere else.
func compileGoal(it intent.Intent) (Plan, error) {
	e := it.Expected
	if e.Severity < proactive.SeverityInfo || e.Severity > proactive.SeverityWarning {
		return Plan{}, errors.New("an expectation's severity is info, notice or warning")
	}
	p := triggers.GoalParams{
		Expr: e.Expr, Severity: e.Severity.String(), Consecutive: e.Consecutive, EveryMinutes: e.EveryMinutes,
		Title: triggers.CleanAlertMessage(it.Title), Description: triggers.CleanAlertMessage(e.Description),
	}
	if err := p.Validate(); err != nil {
		return Plan{}, fmt.Errorf("the expectation is not valid: %s", errText(err))
	}
	return Plan{Kind: triggers.KindGoalDiscrepancy, Params: p.JSON(), Origin: originFor(it)}, nil
}

func originFor(it intent.Intent) string {
	if it.Type == intent.TypeStandingOrder {
		return triggers.IntentOrderOrigin(it.ID)
	}
	return triggers.IntentOrigin(it.ID)
}

func compileWatch(it intent.Intent) (Plan, error) {
	if it.Trigger == nil {
		return Plan{}, fmt.Errorf("it needs a trigger: a %s or an %s", triggers.KindPriceCross, triggers.KindIndicatorCross)
	}
	raw := it.Trigger.Params
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	cooldown, err := cooldownMinutes(it.Cooldown)
	if err != nil {
		return Plan{}, err
	}
	msg := triggers.CleanAlertMessage(it.Title)
	recurring := it.FireBudget != 1
	plan := Plan{Kind: it.Trigger.Kind, Origin: originFor(it)}
	switch it.Trigger.Kind {
	case triggers.KindPriceCross:
		p, err := triggers.ParsePriceCross(raw)
		if err != nil {
			return Plan{}, fmt.Errorf("the %s trigger is not valid: %s", it.Trigger.Kind, errText(err))
		}
		p.Recurring, p.Message = recurring, msg
		if cooldown > 0 {
			p.CooldownMin = cooldown
		}
		if err := p.Validate(); err != nil {
			return Plan{}, fmt.Errorf("the %s trigger is not valid: %s", it.Trigger.Kind, errText(err))
		}
		plan.Params = p.JSON()
	case triggers.KindIndicatorCross:
		p, err := triggers.ParseIndicatorCross(raw)
		if err != nil {
			return Plan{}, fmt.Errorf("the %s trigger is not valid: %s", it.Trigger.Kind, errText(err))
		}
		p.Recurring, p.Message = recurring, msg
		if cooldown > 0 {
			p.CooldownMin = cooldown
		}
		if err := p.Validate(); err != nil {
			return Plan{}, fmt.Errorf("the %s trigger is not valid: %s", it.Trigger.Kind, errText(err))
		}
		plan.Params = p.JSON()
	default:
		return Plan{}, errors.New(BadKindMessage(it.Trigger.Kind))
	}
	return plan, nil
}

// cooldownMinutes maps the intent's cooldown to the crossing kinds' own parameter,
// rounding up to whole minutes.
func cooldownMinutes(d time.Duration) (int, error) {
	if d <= 0 {
		return 0, nil
	}
	m := int((d + time.Minute - 1) / time.Minute)
	if m > triggers.MaxCooldownMinutes {
		return 0, fmt.Errorf("a cooldown over %d minutes (7 days) is not supported for a watch", triggers.MaxCooldownMinutes)
	}
	return m, nil
}

// errText keeps a validation error to the sentence the trigger kind wrote. Those
// sentences are fixed text; a decoder error names a field of the JSON, which is the
// shape of the parameters and not the owner's words.
func errText(err error) string { return oneLine(err.Error(), 200) }

// reminderParams is what a reminder's Params may say about when. Exactly one of At,
// In and Cron is set; Other keys (a reminder's text) are ignored.
type reminderParams struct {
	At   string `json:"at"`
	In   string `json:"in"`
	Cron string `json:"cron"`
	TZ   string `json:"tz"`
}

var delayPattern = regexp.MustCompile(`^(\d{1,4})([mhdw])$`)

// ParseDelay parses "90m", "12h", "3d" or "2w".
func ParseDelay(s string) (time.Duration, error) {
	m := delayPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, errors.New(`"in" must be a number and a unit (m, h, d or w), for example 3d`)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	d := time.Duration(n) * unit
	if d < time.Minute || d > intent.MaxLifetime {
		return 0, errors.New(`"in" must be from 1m to 365d`)
	}
	return d, nil
}

var atLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"}

// parseAt reads an absolute time. One with a zone (RFC 3339) is taken as it is; one
// without is read in the owner's zone.
func parseAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, l := range atLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New(`"at" must be a date and time such as 2026-10-16T09:00 (in the owner's time zone) or 2026-10-16T09:00:00+07:00`)
}

func compileReminder(it intent.Intent, env Env) (Plan, error) {
	loc := env.Loc
	if loc == nil {
		loc = DefaultLocation()
	}
	var r reminderParams
	if len(it.Params) > 0 {
		if err := json.Unmarshal(it.Params, &r); err != nil {
			return Plan{}, errors.New(`a reminder's "at", "in", "cron" and "tz" must be text`)
		}
	}
	set := 0
	for _, s := range []string{r.At, r.In, r.Cron} {
		if strings.TrimSpace(s) != "" {
			set++
		}
	}
	if set != 1 {
		return Plan{}, errors.New(`a reminder needs exactly one of "at" (a date and time), "in" (a delay such as 3d) or "cron" (a schedule, with "tz")`)
	}
	if r.TZ != "" && strings.TrimSpace(r.Cron) == "" {
		return Plan{}, errors.New(`"tz" goes with "cron"`)
	}
	p := triggers.TimeAtParams{
		Zone: loc.String(), Message: triggers.CleanAlertMessage(it.Title),
	}
	if it.ExpiresAt != nil {
		p.UntilMS = it.ExpiresAt.UnixMilli()
	}
	plan := Plan{Kind: triggers.KindTimeAt, Origin: originFor(it)}

	if strings.TrimSpace(r.Cron) == "" {
		var due time.Time
		if strings.TrimSpace(r.At) != "" {
			t, err := parseAt(r.At, loc)
			if err != nil {
				return Plan{}, err
			}
			due = t
		} else {
			d, err := ParseDelay(r.In)
			if err != nil {
				return Plan{}, err
			}
			base := env.ActivatedAt
			if base.IsZero() {
				base = env.Now
			}
			due = base.Add(d)
		}
		due = due.UTC().Truncate(time.Millisecond)
		p.AtMS, p.Budget = due.UnixMilli(), 1 // an absolute moment passes once
		plan.Due = due
		plan.Timer = &perception.Timer{At: due}
	} else {
		expr := strings.TrimSpace(r.Cron)
		tz := strings.TrimSpace(r.TZ)
		if tz == "" {
			tz = loc.String()
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return Plan{}, errors.New(`"tz" must be an IANA time zone such as Asia/Bangkok`)
		}
		if err := checkInterval(expr, tz, env.Now); err != nil {
			return Plan{}, err
		}
		p.Cron, p.TZ, p.Zone, p.Budget = expr, tz, tz, it.FireBudget
		plan.Timer = &perception.Timer{Cron: expr, TZ: tz, Budget: it.FireBudget}
		if it.ExpiresAt != nil {
			plan.Timer.Until = *it.ExpiresAt
		}
	}
	if err := p.Validate(); err != nil {
		return Plan{}, fmt.Errorf("the reminder is not valid: %s", errText(err))
	}
	plan.Params = p.JSON()
	return plan, nil
}

// checkInterval refuses a schedule that never fires or fires more often than
// MinReminderInterval. It looks at the next few gaps, so a schedule that is dense
// only in some hours is caught too.
func checkInterval(expr, tz string, now time.Time) error {
	at := now
	var prev time.Time
	for i := 0; i < 8; i++ {
		ms, err := cron.NextRun(cron.CronSchedule{Kind: "cron", Expr: expr, TZ: tz}, at)
		if err != nil {
			return errors.New(`"cron" is not a schedule the clock understands (five fields, for example 0 9 * * 5)`)
		}
		if ms == nil {
			return errors.New(`"cron" never comes due`)
		}
		next := time.UnixMilli(*ms)
		if !prev.IsZero() && next.Sub(prev) < MinReminderInterval {
			return fmt.Errorf("a reminder may repeat at most once a minute; %q is tighter", oneLine(expr, 60))
		}
		prev, at = next, next
	}
	return nil
}

// NextOccurrence is the first time the recurring timer fires after now.
func NextOccurrence(t perception.Timer, now time.Time) (time.Time, bool) {
	ms, err := cron.NextRun(cron.CronSchedule{Kind: "cron", Expr: t.Cron, TZ: t.TZ}, now)
	if err != nil || ms == nil {
		return time.Time{}, false
	}
	return time.UnixMilli(*ms), true
}

// DefaultLocation is the zone used until the profile gives one.
func DefaultLocation() *time.Location {
	if l, err := time.LoadLocation(defaultZone); err == nil {
		return l
	}
	return time.FixedZone("ICT", 7*3600)
}

// OneShotReminder reports whether a reminder fires once (an {at} or an {in}).
func OneShotReminder(it intent.Intent) bool {
	var r reminderParams
	if len(it.Params) == 0 || json.Unmarshal(it.Params, &r) != nil {
		return true
	}
	return strings.TrimSpace(r.Cron) == ""
}

// oneLine reduces s to a single printable line of at most max runes.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
