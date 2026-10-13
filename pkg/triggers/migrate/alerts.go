package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// The legacy alert jobs' names and payload, copied from pkg/tools (which this
// package must not import): a test in this package checks the copies against the
// originals, so a drift fails the build instead of silently dropping a field.
const (
	jobPrefixPriceAlert     = "price_alert:"
	jobPrefixIndicatorAlert = "indicator_alert:"
)

type legacyPrice struct {
	ProviderID      string  `json:"provider"`
	Account         string  `json:"account"`
	Symbol          string  `json:"symbol"`
	Condition       string  `json:"condition"`
	Threshold       float64 `json:"threshold"`
	AlertMsg        string  `json:"alert_msg"`
	Recurring       bool    `json:"recurring"`
	CooldownMinutes int     `json:"cooldown_minutes"`
	LastFiredMS     int64   `json:"last_fired_ms"`
}

type legacyIndicator struct {
	ProviderID      string  `json:"provider"`
	Account         string  `json:"account"`
	Symbol          string  `json:"symbol"`
	Timeframe       string  `json:"timeframe"`
	Indicator       string  `json:"indicator"`
	Period          int     `json:"period"`
	Condition       string  `json:"condition"`
	Threshold       float64 `json:"threshold"`
	AlertMsg        string  `json:"alert_msg"`
	Recurring       bool    `json:"recurring"`
	CooldownMinutes int     `json:"cooldown_minutes"`
	LastFiredMS     int64   `json:"last_fired_ms"`
}

// AlertFamily is the price and indicator alert jobs.
func AlertFamily() Family {
	return Family{
		Name:        "alerts",
		Prefixes:    []string{jobPrefixPriceAlert, jobPrefixIndicatorAlert},
		Kinds:       []string{triggers.KindPriceCross, triggers.KindIndicatorCross},
		ToolOrigin:  "tool:",
		StandIn:     standInAlert,
		Reactivate:  reactivateAlert,
		Import:      importAlert,
		Same:        sameAlert,
		Fingerprint: fingerprintAlert,
		OneShot:     oneShotAlert,
	}
}

// MigrateAlertJobs is Sync for the alert family: it imports every price_alert:* and
// indicator_alert:* job (idempotently, in shadow) and, in Live mode, promotes them;
// in Shadow mode it hands back anything promoted. Call it at startup, on a reload
// and now and then, with Live only when the registry is live and running.
func MigrateAlertJobs(ctx context.Context, jobs Jobs, specs Specs, mode Mode, opts ...Option) (Report, error) {
	return New(AlertFamily(), jobs, specs, opts...).Sync(ctx, mode)
}

func oneShotAlert(job cron.CronJob) bool {
	switch {
	case strings.HasPrefix(job.Name, jobPrefixPriceAlert):
		var p legacyPrice
		return json.Unmarshal([]byte(job.Payload.Message), &p) == nil && !p.Recurring
	case strings.HasPrefix(job.Name, jobPrefixIndicatorAlert):
		var p legacyIndicator
		return json.Unmarshal([]byte(job.Payload.Message), &p) == nil && !p.Recurring
	}
	return false
}

func importAlert(job cron.CronJob) (string, json.RawMessage, error) {
	switch {
	case strings.HasPrefix(job.Name, jobPrefixPriceAlert):
		p, err := priceParams(job)
		if err != nil {
			return "", nil, err
		}
		return triggers.KindPriceCross, p.JSON(), nil
	case strings.HasPrefix(job.Name, jobPrefixIndicatorAlert):
		p, err := indicatorParams(job)
		if err != nil {
			return "", nil, err
		}
		return triggers.KindIndicatorCross, p.JSON(), nil
	}
	return "", nil, fmt.Errorf("not an alert job")
}

func effectiveCooldown(m int) int {
	if m <= 0 {
		return 60 // tools.DefaultAlertCooldownMinutes
	}
	return min(m, 7*24*60)
}

func priceParams(job cron.CronJob) (triggers.PriceCrossParams, error) {
	var l legacyPrice
	if err := json.Unmarshal([]byte(job.Payload.Message), &l); err != nil {
		return triggers.PriceCrossParams{}, fmt.Errorf("decode payload: %w", err)
	}
	never := l.LastFiredMS <= 0
	p := triggers.PriceCrossParams{
		Provider: l.ProviderID, Account: l.Account, Symbol: l.Symbol, Direction: l.Condition,
		Threshold: l.Threshold, Recurring: l.Recurring, CooldownMin: effectiveCooldown(l.CooldownMinutes),
		// A job that has fired already delivered "it is true now": the trigger waits for
		// the next real crossing instead of saying it again.
		FireIfAlready: &never,
		Message:       triggers.CleanAlertMessage(l.AlertMsg),
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

func indicatorParams(job cron.CronJob) (triggers.IndicatorCrossParams, error) {
	var l legacyIndicator
	if err := json.Unmarshal([]byte(job.Payload.Message), &l); err != nil {
		return triggers.IndicatorCrossParams{}, fmt.Errorf("decode payload: %w", err)
	}
	// The legacy handler's normalisation: upper case, and "SMA20"/"EMA9" spelt as one.
	ind := strings.ToUpper(strings.TrimSpace(l.Indicator))
	period := l.Period
	if digits := strings.TrimLeftFunc(ind, func(r rune) bool { return !unicode.IsDigit(r) }); digits != "" {
		var n int
		if _, err := fmt.Sscanf(digits, "%d", &n); err == nil && period <= 0 {
			period = n
		}
		ind = strings.TrimRightFunc(ind, unicode.IsDigit)
	}
	kind := ""
	switch ind {
	case "RSI":
		kind = triggers.IndRSI
		if period <= 0 {
			period = 14
		}
	case "SMA":
		kind = triggers.IndSMA
		if period <= 0 {
			period = 20
		}
	case "EMA":
		kind = triggers.IndEMA
		if period <= 0 {
			period = 20
		}
	case "MACD":
		// The legacy alert compared the MACD line only.
		kind, period = triggers.IndMACDLine, 0
	default:
		return triggers.IndicatorCrossParams{}, fmt.Errorf("unknown indicator %q", l.Indicator)
	}
	tf := l.Timeframe
	if tf == "" {
		tf = "1h"
	}
	never := l.LastFiredMS <= 0
	p := triggers.IndicatorCrossParams{
		Provider: l.ProviderID, Account: l.Account, Symbol: l.Symbol, Timeframe: tf, Indicator: kind,
		Period: period, Direction: l.Condition, Threshold: l.Threshold, Recurring: l.Recurring,
		CooldownMin: effectiveCooldown(l.CooldownMinutes), FireIfAlready: &never,
		Message: triggers.CleanAlertMessage(l.AlertMsg),
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// definition is the part of an alert a user chose; the rest of a spec's parameters
// (hysteresis, fire_if_already) belongs to the trigger.
type definition struct {
	Kind      string  `json:"kind"`
	Provider  string  `json:"provider"`
	Account   string  `json:"account"`
	Symbol    string  `json:"symbol"`
	Timeframe string  `json:"timeframe"`
	Indicator string  `json:"indicator"`
	Period    int     `json:"period"`
	Direction string  `json:"direction"`
	Threshold float64 `json:"threshold"`
	Recurring bool    `json:"recurring"`
	Cooldown  int     `json:"cooldown"`
	Message   string  `json:"message"`
}

func definitionOfSpec(spec triggers.Spec) (definition, bool) {
	switch spec.Kind {
	case triggers.KindPriceCross:
		var p triggers.PriceCrossParams
		if json.Unmarshal(spec.Params, &p) != nil {
			return definition{}, false
		}
		return definition{
			Kind: spec.Kind, Provider: p.Provider, Account: p.Account, Symbol: p.Symbol, Direction: p.Direction,
			Threshold: p.Threshold, Recurring: p.Recurring, Cooldown: effectiveCooldown(p.CooldownMin), Message: p.Message,
		}, true
	case triggers.KindIndicatorCross:
		var p triggers.IndicatorCrossParams
		if json.Unmarshal(spec.Params, &p) != nil {
			return definition{}, false
		}
		return definition{
			Kind: spec.Kind, Provider: p.Provider, Account: p.Account, Symbol: p.Symbol, Timeframe: p.Timeframe,
			Indicator: p.Indicator, Period: p.Period, Direction: p.Direction, Threshold: p.Threshold,
			Recurring: p.Recurring, Cooldown: effectiveCooldown(p.CooldownMin), Message: p.Message,
		}, true
	}
	return definition{}, false
}

func definitionOfJob(job cron.CronJob) (definition, bool) {
	kind, params, err := importAlert(job)
	if err != nil {
		return definition{}, false
	}
	return definitionOfSpec(triggers.Spec{Kind: kind, Params: params})
}

func sameAlert(job cron.CronJob, spec triggers.Spec) bool {
	a, ok1 := definitionOfJob(job)
	b, ok2 := definitionOfSpec(spec)
	return ok1 && ok2 && a == b
}

// fingerprintAlert digests what a user can edit: the job's name, where it delivers,
// its schedule and the alert's definition. It leaves out what the legacy handler
// rewrites as it runs (last_fired_ms, consecutive_errors) and the runtime state, so
// a job that merely ran in between is not taken for an edited one.
func fingerprintAlert(job cron.CronJob) string {
	d, ok := definitionOfJob(job)
	var def any = d
	if !ok {
		def = job.Payload.Message
	}
	b, _ := json.Marshal(struct {
		Name, Channel, To, Expr, Kind string
		Every                         int64
		Def                           any
	}{
		Name: job.Name, Channel: job.Payload.Channel, To: job.Payload.To, Expr: job.Schedule.Expr,
		Kind: job.Schedule.Kind, Every: derefInt(job.Schedule.EveryMS), Def: def,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// standInAlert builds the legacy cron alert that delivers for a tool-made spec while
// the registry does not: the same name and payload the cron tools write, polled every
// minute. A spec the old path cannot express (a MACD signal or histogram) is refused.
func standInAlert(spec triggers.Spec) (name, message string, sched cron.CronSchedule, err error) {
	sched = cron.CronSchedule{Kind: "cron", Expr: "* * * * *"}
	switch spec.Kind {
	case triggers.KindPriceCross:
		var p triggers.PriceCrossParams
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return "", "", sched, err
		}
		raw, err := json.Marshal(legacyPrice{
			ProviderID: p.Provider, Account: p.Account, Symbol: p.Symbol, Condition: p.Direction,
			Threshold: p.Threshold, AlertMsg: p.Message, Recurring: p.Recurring, CooldownMinutes: p.CooldownMin,
		})
		if err != nil {
			return "", "", sched, err
		}
		return fmt.Sprintf("%s%s:%s:%s:%.8g", jobPrefixPriceAlert, p.Provider, p.Symbol, p.Direction, p.Threshold), string(raw), sched, nil
	case triggers.KindIndicatorCross:
		var p triggers.IndicatorCrossParams
		if err := json.Unmarshal(spec.Params, &p); err != nil {
			return "", "", sched, err
		}
		family := ""
		switch p.Indicator {
		case triggers.IndRSI:
			family = "RSI"
		case triggers.IndSMA:
			family = "SMA"
		case triggers.IndEMA:
			family = "EMA"
		case triggers.IndMACDLine:
			family = "MACD"
		default:
			return "", "", sched, fmt.Errorf("the cron alert cannot express %s", p.Indicator)
		}
		raw, err := json.Marshal(legacyIndicator{
			ProviderID: p.Provider, Account: p.Account, Symbol: p.Symbol, Timeframe: p.Timeframe, Indicator: family,
			Period: p.Period, Condition: p.Direction, Threshold: p.Threshold, AlertMsg: p.Message,
			Recurring: p.Recurring, CooldownMinutes: p.CooldownMin,
		})
		if err != nil {
			return "", "", sched, err
		}
		tag := ""
		if p.Period > 0 {
			tag = fmt.Sprintf("%d", p.Period)
		}
		return fmt.Sprintf("%s%s:%s:%s%s:%s:%.4g", jobPrefixIndicatorAlert, p.Provider, p.Symbol, family, tag, p.Direction, p.Threshold),
			string(raw), sched, nil
	}
	return "", "", sched, fmt.Errorf("not an alert spec")
}

// reactivateAlert is a stand-in's spec as it goes back to the registry. The state is
// cleared by the caller (UpdateFresh), so the next fresh observation is a first one.
// A recurring alert then must not announce "already above" again: the stand-in has
// been delivering while the registry was away. A one-shot is still here, so it never
// fired, and keeps its own fire_if_already.
func reactivateAlert(spec triggers.Spec) triggers.Spec {
	no := false
	switch spec.Kind {
	case triggers.KindPriceCross:
		var p triggers.PriceCrossParams
		if json.Unmarshal(spec.Params, &p) == nil && p.Recurring {
			p.FireIfAlready = &no
			spec.Params = p.JSON()
		}
	case triggers.KindIndicatorCross:
		var p triggers.IndicatorCrossParams
		if json.Unmarshal(spec.Params, &p) == nil && p.Recurring {
			p.FireIfAlready = &no
			spec.Params = p.JSON()
		}
	}
	return spec
}
