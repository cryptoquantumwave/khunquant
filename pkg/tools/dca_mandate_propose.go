package tools

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/adhocore/gronx"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
)

// MandateProposer is what the DCA plan tools need from the approval service.
// *approval.Service satisfies it.
type MandateProposer interface {
	ProposeMandate(ctx context.Context, r approval.MandateRequest) (approval.Proposal, error)
}

var mandateProposerHolder atomic.Pointer[MandateProposer]

// SetMandateProposer attaches the approval service to create_dca_plan and
// update_dca_plan; nil detaches it. The gateway sets it with the order proposer.
func SetMandateProposer(p MandateProposer) {
	if p == nil {
		mandateProposerHolder.Store(nil)
		return
	}
	mandateProposerHolder.Store(&p)
}

func currentMandateProposer() MandateProposer {
	if p := mandateProposerHolder.Load(); p != nil {
		return *p
	}
	return nil
}

const (
	// mandateHeadroom is the margin the default per-run cap leaves over the plan's
	// amount, so that a small move of the quote currency's USD rate between the
	// card and a run does not turn every run into a question.
	mandateHeadroom = 1.10
	// defaultMandateMonths is the default life of a mandate.
	defaultMandateMonths = 6
	maxRunsPerPeriod     = 400
)

// mandateAsk holds the optional limits the owner chose in the plan tool's
// arguments, in the plan's quote currency. Zero means "use the default"; whatever
// is used appears on the card the owner approves.
type mandateAsk struct {
	maxPerRun, maxPerPeriod float64
	period                  string
	months                  int
	// requested: the owner (via the model) asked for a mandate in this call, even
	// though nothing about the plan changed.
	requested bool
}

func mandateAskFrom(cfg *config.Config, args map[string]any) (mandateAsk, string) {
	var a mandateAsk
	if cfg == nil || !cfg.Proactive.Effective().Mandates {
		return a, "" // mandates are off: the mandate_* arguments mean nothing and are not even read
	}
	a.maxPerRun = numberArg(args, "mandate_max_per_run")
	a.maxPerPeriod = numberArg(args, "mandate_max_per_period")
	a.period, _ = args["mandate_period"].(string)
	a.months = int(numberArg(args, "mandate_months"))
	a.requested, _ = args["propose_mandate"].(bool)
	a.requested = a.requested || a.maxPerRun > 0 || a.maxPerPeriod > 0 || a.period != "" || a.months > 0
	for _, v := range []float64{a.maxPerRun, a.maxPerPeriod} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return a, "mandate limits must be positive, finite numbers"
		}
	}
	if a.period != "" && a.period != approval.PeriodWeek && a.period != approval.PeriodMonth {
		return a, "mandate_period must be week or month"
	}
	if a.months < 0 || a.months > approval.MaxMandateMonths {
		return a, fmt.Sprintf("mandate_months must be between 1 and %d", approval.MaxMandateMonths)
	}
	return a, ""
}

// mandateParameters are the DCA plan tool arguments that carry the mandate's limits.
var mandateParameters = map[string]any{
	"mandate_max_per_run":    map[string]any{"type": "number", "description": "Optional. The most one run may spend, in the plan's quote currency, for the mandate the owner is asked to approve (default: the plan's amount plus 10%)."},
	"mandate_max_per_period": map[string]any{"type": "number", "description": "Optional. The most all runs may spend in one period, in the plan's quote currency (default: the runs the schedule makes in a period)."},
	"mandate_period":         map[string]any{"type": "string", "enum": []string{"week", "month"}, "description": "Optional. Period of the per-period limit (default month)."},
	"propose_mandate":        map[string]any{"type": "boolean", "description": "Optional (update_dca_plan). Ask the owner for a new mandate for the plan as it is (after one expired, was revoked, or needs renewing)."},
	"mandate_months":         map[string]any{"type": "integer", "description": "Optional. How long the mandate lasts, in months (default 6, at most 12)."},
}

// runsPerPeriod counts the runs a cron schedule makes in the next period.
func runsPerPeriod(expr, tz, period string, from time.Time) int {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	end := from.AddDate(0, 1, 0)
	if period == approval.PeriodWeek {
		end = from.AddDate(0, 0, 7)
	}
	n, ref := 0, from.In(loc)
	for n < maxRunsPerPeriod {
		next, err := gronx.NextTickAfter(expr, ref, false)
		if err != nil || !next.Before(end) {
			break
		}
		n++
		ref = next
	}
	return max(n, 1)
}

func ceil2(v float64) float64 { return math.Ceil(v*100-1e-9) / 100 }

// proposeMandateFor asks the owner to approve a mandate for the plan, when mandates
// are on and the turn is the owner's own root turn. It returns a line for the tool's
// result ("" when mandates are not in play). It places and enables nothing: until the
// owner approves, the plan keeps running on its scheduled job as before.
func proposeMandateFor(ctx context.Context, cfg *config.Config, store *dca.Store, plan *dca.Plan, ask mandateAsk) string {
	if cfg == nil || !cfg.Proactive.Effective().Mandates {
		return ""
	}
	mp := currentMandateProposer()
	if mp == nil {
		return "\nMandates are on but order approvals are not running, so no mandate was proposed; the plan keeps running on its scheduled job."
	}
	if _, v := checkOwnerTurn(ctx, cfg, true); v != ownerYes {
		return "\nNo mandate was proposed: a mandate can be proposed only from the owner's own conversation. The plan keeps running on its scheduled job."
	}
	switch {
	case plan.Side != "" && plan.Side != "buy":
		return "\nNo mandate was proposed: mandates cover buys only. The plan keeps running on its scheduled job."
	case plan.Trigger != nil:
		return "\nNo mandate was proposed: a plan with an indicator trigger is not covered by mandates in this change. The plan keeps running on its scheduled job."
	case strings.Contains(plan.Symbol, ":"):
		return "\nNo mandate was proposed: mandates cover spot symbols only. The plan keeps running on its scheduled job."
	}
	params, err := buildMandateParams(ctx, cfg, plan, ask, time.Now())
	if err != nil {
		return "\nNo mandate was proposed (" + err.Error() + "). The plan keeps running on its scheduled job."
	}
	p, err := mp.ProposeMandate(ctx, approval.MandateRequest{Params: params})
	if err != nil {
		return "\nNo mandate was proposed (" + refusalMessage(err) + "). The plan keeps running on its scheduled job."
	}
	if err := store.SetMandatePending(ctx, plan.ID, p.ID, time.Now()); err != nil {
		return "\nA mandate card was sent (" + p.Code + ") but could not be recorded against the plan: " + err.Error()
	}
	return fmt.Sprintf("\nStatus: awaiting_mandate. A mandate card (%s) was sent to the owner; until they approve it the plan keeps running on its scheduled job, "+
		"and once approved it runs by itself inside the limits on the card, asking about anything outside them.", p.Code)
}

// buildMandateParams computes the mandate from the plan: the limits in the quote
// currency, converted once to USD at the current rate (the card says the rate and its
// source), the period and the expiry.
func buildMandateParams(ctx context.Context, cfg *config.Config, plan *dca.Plan, ask mandateAsk, now time.Time) (approval.MandateParams, error) {
	_, real, err := broker.TradingProviderFor(cfg, plan.Provider, plan.Account)
	if err != nil {
		return approval.MandateParams{}, err
	}
	md, ok := real.(broker.MarketDataProvider)
	if !ok {
		return approval.MandateParams{}, fmt.Errorf("provider %q has no market data", plan.Provider)
	}
	tk, err := md.FetchTicker(ctx, plan.Symbol)
	if err != nil || tk.Last == nil || !positiveFinite(*tk.Last) {
		return approval.MandateParams{}, fmt.Errorf("no price for %s to size the limits", plan.Symbol)
	}
	base := plan.AmountPerOrder
	if plan.AmountUnit != "base" {
		base /= *tk.Last
	}
	est, err := broker.EstimateNotionalUSDWithFallback(ctx, cfg, md, plan.Symbol, "buy", base, nil)
	if err != nil || !est.USDKnown || !positiveFinite(est.USDPerQuote) {
		return approval.MandateParams{}, fmt.Errorf("the USD value of the plan is unknown, so USD limits cannot be set: %v", err)
	}
	period := ask.period
	if period == "" {
		period = approval.PeriodMonth
	}
	perRun := ask.maxPerRun
	if perRun <= 0 {
		perRun = ceil2(est.QuoteNotional * mandateHeadroom)
	}
	perPeriod := ask.maxPerPeriod
	if perPeriod <= 0 {
		perPeriod = ceil2(perRun * float64(runsPerPeriod(plan.FrequencyExpr, plan.Timezone, period, now)))
	}
	months := ask.months
	if months <= 0 {
		months = defaultMandateMonths
	}
	src := est.RateSource
	if src == "" {
		src = est.Source
	}
	if src == "" {
		src = "the quote currency is USD-like"
	}
	return approval.MandateParams{
		Kind: "dca", PlanID: plan.ID, Provider: plan.Provider, Account: plan.Account, Symbol: plan.Symbol, Side: "buy",
		Quote: est.Quote, MaxPerRunQuote: perRun, MaxPerPeriodQuote: perPeriod,
		MaxPerRunUSD: perRun * est.USDPerQuote, MaxPerPeriodUSD: perPeriod * est.USDPerQuote,
		RateUSDPerQuote: est.USDPerQuote, RateSource: src, Period: period,
		ExpiresAtMS: now.AddDate(0, months, 0).UnixMilli(),
	}, nil
}
