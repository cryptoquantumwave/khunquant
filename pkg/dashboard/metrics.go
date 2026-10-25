package dashboard

import (
	"context"
	"sort"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
)

// HarmSource is the harm guardrail's source (*feedback.Service). It is read here, for
// display, and by nothing that decides.
type HarmSource interface {
	HarmMetric(ctx context.Context, now time.Time) (feedback.Harm, error)
}

// Metric is one number (or structure) and why there is none. A metric with no data has
// a nil Value and a Reason; it is never reported as 0. Counts are 0 when their source
// exists and saw nothing; a rate is nil when its denominator is 0.
type Metric struct {
	Value  any    `json:"value"`
	Reason string `json:"reason,omitempty"`
}

func val(v any) Metric          { return Metric{Value: v} }
func none(reason string) Metric { return Metric{Reason: reason} }

// Metrics is the dashboard's numbers over the last Days owner-local days, the current
// day included. Days are the owner's calendar days (the profile's time zone).
type Metrics struct {
	Days int       `json:"days"`
	Zone string    `json:"zone"`
	From string    `json:"from"` // first day, 2006-01-02
	To   string    `json:"to"`   // last day (today)
	At   time.Time `json:"server_time"`

	// SendsPerDay: messages put in front of the owner by day. A send is an intervention
	// (one message; decisions delivered together share it) with a decision delivered = 1
	// whose disposition is notify or escalate, counted on the day of its first delivery.
	// Value {total, average, series[{day, sends}]}. Nil when attention recorded no
	// decision in the period.
	SendsPerDay Metric `json:"sends_per_day"`
	// PrecisionAtSent = useful / (useful + not_useful) over the sent interventions of the
	// period; an intervention counts once, by the latest of its useful and not_useful
	// responses. Nil without a rating (and without feedback_responses).
	PrecisionAtSent Metric `json:"precision_at_sent"`
	// NotUsefulRate = sent interventions whose verdict is not_useful / sent interventions.
	NotUsefulRate Metric `json:"not_useful_rate"`
	// MuteRate = sent interventions with a mute response / sent interventions.
	MuteRate Metric `json:"mute_rate"`
	// NoResponseRate = sent interventions with a none response (nothing in 24 hours) /
	// sent interventions delivered more than 24 hours ago, the only ones that could have one.
	NoResponseRate Metric `json:"no_response_rate"`
	// ResponsesByKind: the feedback responses recorded in the period, counted by kind.
	ResponsesByKind Metric `json:"responses_by_kind"`
	// CriticalRecall needs labelled data; there is none to compute it from here.
	CriticalRecall Metric `json:"critical_recall"`
	// MedianQueueLatencySeconds = median of (delivered_at - decided_at) over decisions
	// delivered as notify or escalate in the period. Nil when there are none.
	MedianQueueLatencySeconds Metric `json:"median_queue_latency_seconds"`
	// LLMUsagePerDay: cognition_usage by day, wakes and input+output tokens summed over
	// kinds. Value [{day, wakes, tokens}] for the days with a row. Nil without the table or rows.
	LLMUsagePerDay Metric `json:"llm_usage_per_day"`
	// PostAlertTrading is the harm guardrail (feedback.HarmMetric), over its own fixed
	// window of complete days. SHOWN, NEVER OPTIMISED: nothing may read it to decide.
	PostAlertTrading Metric `json:"post_alert_trading"`
	// ApprovalRates: proposals created in the period, by group, with approval rate =
	// approved / (approved + rejected + expired). Approved is approved, executing, executed
	// or failed (the failure came after approval); cancelled and pending are not counted.
	// Groups: T1 (an order that only reduces exposure), T2 (a mandate), T3 (any other
	// order), then memory and reflection, which have no tier. Each row's rate is nil
	// without a decided proposal.
	ApprovalRates Metric `json:"approval_rates"`
	// LearningPerDay: decisions learning moved from a send to the digest, by day:
	// low_acceptance (reason) and holdout (the holdout flag). Nil when no decision of the
	// period carries a learning mark (learning is off, or has not decided yet).
	LearningPerDay Metric `json:"learning_per_day"`
}

type daySends struct {
	Day   string `json:"day"`
	Sends int    `json:"sends"`
}

type dayUsage struct {
	Day    string `json:"day"`
	Wakes  int    `json:"wakes"`
	Tokens int    `json:"tokens"`
}

type dayLearning struct {
	Day           string `json:"day"`
	LowAcceptance int    `json:"low_acceptance"`
	Holdout       int    `json:"holdout"`
}

// ApprovalRate is one row of ApprovalRates.
type ApprovalRate struct {
	Group    string   `json:"group"`
	Approved int      `json:"approved"`
	Rejected int      `json:"rejected"`
	Expired  int      `json:"expired"`
	Pending  int      `json:"pending"`
	Rate     *float64 `json:"rate"`
}

const (
	reasonNoDecisions = "attention recorded no decision in this period"
	reasonNoFeedback  = "learning is off, so ratings are not recorded (proactive.learning)"
	reasonNoSends     = "nothing was sent in this period"
)

var dayLayout = "2006-01-02"

// Metrics computes the dashboard's numbers over the last days days (1 to 90).
func (r *Reader) Metrics(ctx context.Context, days int) (Metrics, error) {
	if days < 1 || days > 90 {
		return Metrics{}, badf("days is from 1 to 90")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*queryTimeout)
	defer cancel()
	now, loc := r.now(), r.loc(ctx)
	y, m, d := now.In(loc).Date()
	first := time.Date(y, m, d-(days-1), 0, 0, 0, 0, loc)
	dayList := make([]string, 0, days)
	for i := 0; i < days; i++ {
		dayList = append(dayList, time.Date(y, m, d-(days-1)+i, 0, 0, 0, 0, loc).Format(dayLayout))
	}
	out := Metrics{Days: days, Zone: loc.String(), From: dayList[0], To: dayList[days-1], At: now.UTC(),
		CriticalRecall: none("needs labels: run khunquant replay")}
	start := first.UnixMilli()

	sent, err := r.scanDecisions(ctx, &out, start, loc, dayList)
	if err != nil {
		return out, err
	}
	if err := r.feedbackMetrics(ctx, &out, start, sent, now); err != nil {
		return out, err
	}
	if err := r.usageMetric(ctx, &out, dayList); err != nil {
		return out, err
	}
	if err := r.approvalMetric(ctx, &out, start); err != nil {
		return out, err
	}
	out.PostAlertTrading = r.harmMetric(ctx, now)
	return out, nil
}

// sentIntervention is what the feedback metrics need of a send.
type sentIntervention struct{ deliveredMs int64 }

// scanDecisions reads the period's decisions once and fills the metrics that come from
// attention_decisions alone. It returns the sent interventions by id.
func (r *Reader) scanDecisions(ctx context.Context, out *Metrics, start int64, loc *time.Location, dayList []string) (map[string]sentIntervention, error) {
	out.SendsPerDay, out.MedianQueueLatencySeconds = none(reasonNoDecisions), none(reasonNoDecisions)
	out.LearningPerDay = none("learning is off, or has not decided anything yet")
	if ok, err := r.hasTable(ctx, "attention_decisions"); err != nil || !ok {
		return nil, err
	}
	rows, err := r.DB.QueryContext(ctx,
		`SELECT at_ms, disposition, delivered, COALESCE(delivered_at_ms, at_ms), reason, holdout, propensity IS NOT NULL,
		        COALESCE(intervention_id, id)
		   FROM attention_decisions WHERE at_ms >= ?`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var total, learned int
	sent := map[string]sentIntervention{}
	var latencies []float64
	sendDay := map[string]int{}
	learnDay := map[string]*dayLearning{}
	for rows.Next() {
		var at, deliveredMs int64
		var disp, reason, key string
		var delivered, holdout, hasProp int
		if err := rows.Scan(&at, &disp, &delivered, &deliveredMs, &reason, &holdout, &hasProp, &key); err != nil {
			return nil, err
		}
		total++
		if delivered == 1 && (disp == "notify" || disp == "escalate") && deliveredMs >= start {
			latencies = append(latencies, float64(deliveredMs-at)/1000)
			if s, ok := sent[key]; !ok || deliveredMs < s.deliveredMs {
				sent[key] = sentIntervention{deliveredMs}
			}
		}
		if hasProp == 1 || holdout == 1 || reason == attention.ReasonLowAcceptance {
			learned++
			dl := learnDay[time.UnixMilli(at).In(loc).Format(dayLayout)]
			if dl == nil {
				dl = &dayLearning{}
				learnDay[time.UnixMilli(at).In(loc).Format(dayLayout)] = dl
			}
			if holdout == 1 {
				dl.Holdout++
			}
			if reason == attention.ReasonLowAcceptance {
				dl.LowAcceptance++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if total == 0 {
		return sent, nil
	}
	for _, s := range sent {
		sendDay[time.UnixMilli(s.deliveredMs).In(loc).Format(dayLayout)]++
	}
	series := make([]daySends, 0, len(dayList))
	for _, day := range dayList {
		series = append(series, daySends{day, sendDay[day]})
	}
	out.SendsPerDay = val(map[string]any{"total": len(sent), "average": float64(len(sent)) / float64(len(dayList)), "series": series})
	if len(latencies) > 0 {
		sort.Float64s(latencies)
		mid := len(latencies) / 2
		med := latencies[mid]
		if len(latencies)%2 == 0 {
			med = (latencies[mid-1] + latencies[mid]) / 2
		}
		out.MedianQueueLatencySeconds = val(med)
	} else {
		out.MedianQueueLatencySeconds = none(reasonNoSends)
	}
	if learned > 0 {
		list := make([]dayLearning, 0, len(dayList))
		for _, day := range dayList {
			dl := dayLearning{Day: day}
			if v := learnDay[day]; v != nil {
				dl.LowAcceptance, dl.Holdout = v.LowAcceptance, v.Holdout
			}
			list = append(list, dl)
		}
		out.LearningPerDay = val(list)
	}
	return sent, nil
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}

// feedbackMetrics fills the metrics that rest on the owner's responses.
func (r *Reader) feedbackMetrics(ctx context.Context, out *Metrics, start int64, sent map[string]sentIntervention, now time.Time) error {
	noFB := none(reasonNoFeedback)
	out.PrecisionAtSent, out.NotUsefulRate, out.MuteRate, out.NoResponseRate, out.ResponsesByKind = noFB, noFB, noFB, noFB, noFB
	if ok, err := r.hasTable(ctx, "feedback_responses"); err != nil || !ok {
		return err
	}
	rows, err := r.DB.QueryContext(ctx, `SELECT intervention_id, kind, at_ms FROM feedback_responses WHERE at_ms >= ? ORDER BY at_ms, id`, start)
	if err != nil {
		return err
	}
	defer rows.Close()
	type verdict struct {
		kind string
		at   int64
	}
	byKind := map[string]int{}
	verdicts, muted, unanswered := map[string]verdict{}, map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var id, kind string
		var at int64
		if err := rows.Scan(&id, &kind, &at); err != nil {
			return err
		}
		byKind[kind]++
		if _, isSent := sent[id]; !isSent {
			continue
		}
		switch kind {
		case string(feedback.KindUseful), string(feedback.KindNotUseful):
			verdicts[id] = verdict{kind, at} // rows are in time order: the latest wins
		case string(feedback.KindMute):
			muted[id] = true
		case string(feedback.KindNone):
			unanswered[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	out.ResponsesByKind = val(byKind)
	var useful, notUseful, resolved, noneN int
	for _, v := range verdicts {
		if v.kind == string(feedback.KindUseful) {
			useful++
		} else {
			notUseful++
		}
	}
	cutoff := now.Add(-feedback.UnansweredAfter).UnixMilli()
	for id, s := range sent {
		if s.deliveredMs <= cutoff {
			resolved++
			if unanswered[id] {
				noneN++
			}
		}
	}
	rate := func(n, d int, empty string) Metric {
		if v := ratio(n, d); v != nil {
			return val(*v)
		}
		return none(empty)
	}
	out.PrecisionAtSent = rate(useful, useful+notUseful, "no message was rated Useful or Not useful in this period")
	out.NotUsefulRate = rate(notUseful, len(sent), reasonNoSends)
	out.MuteRate = rate(len(muted), len(sent), reasonNoSends)
	out.NoResponseRate = rate(noneN, resolved, "no message sent in this period is old enough (24 hours) to count as unanswered")
	return nil
}

// usageMetric reads cognition_usage, whose day is already an owner-local date.
func (r *Reader) usageMetric(ctx context.Context, out *Metrics, dayList []string) error {
	out.LLMUsagePerDay = none("the model scheduler (proactive.cognition) recorded no usage")
	if ok, err := r.hasTable(ctx, "cognition_usage"); err != nil || !ok {
		return err
	}
	rows, err := r.DB.QueryContext(ctx,
		`SELECT day, SUM(wakes), SUM(input_tokens) + SUM(output_tokens) FROM cognition_usage WHERE day >= ? AND day <= ? GROUP BY day ORDER BY day`,
		dayList[0], dayList[len(dayList)-1])
	if err != nil {
		return err
	}
	defer rows.Close()
	list := []dayUsage{}
	for rows.Next() {
		var u dayUsage
		if err := rows.Scan(&u.Day, &u.Wakes, &u.Tokens); err != nil {
			return err
		}
		list = append(list, u)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(list) > 0 {
		out.LLMUsagePerDay = val(list)
	}
	return nil
}

// approvalMetric groups the period's proposals into the tiers of the policy.
func (r *Reader) approvalMetric(ctx context.Context, out *Metrics, start int64) error {
	out.ApprovalRates = none("approvals are off, so there are no proposals")
	if ok, err := r.hasTable(ctx, "proposals"); err != nil || !ok {
		return err
	}
	rows, err := r.DB.QueryContext(ctx,
		`SELECT kind, reducing, status, COUNT(*) FROM proposals WHERE created_at_ms >= ? GROUP BY kind, reducing, status`, start)
	if err != nil {
		return err
	}
	type count struct {
		kind, status string
		reducing, n  int
	}
	var counts []count
	for rows.Next() {
		var c count
		if err := rows.Scan(&c.kind, &c.reducing, &c.status, &c.n); err != nil {
			_ = rows.Close()
			return err
		}
		counts = append(counts, c)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	groups := []string{"T1", "T2", "T3", "memory", "reflection"}
	by := map[string]*ApprovalRate{}
	for _, g := range groups {
		by[g] = &ApprovalRate{Group: g}
	}
	for _, c := range counts {
		a := by[ApprovalGroup(approval.Kind(c.kind), c.reducing == 1)]
		switch approval.Status(c.status) {
		case approval.StatusApproved, approval.StatusExecuting, approval.StatusExecuted, approval.StatusFailed:
			a.Approved += c.n
		case approval.StatusRejected:
			a.Rejected += c.n
		case approval.StatusExpired:
			a.Expired += c.n
		case approval.StatusPending:
			a.Pending += c.n
		}
	}
	list := make([]ApprovalRate, 0, len(groups))
	for _, g := range groups {
		a := by[g]
		a.Rate = ratio(a.Approved, a.Approved+a.Rejected+a.Expired)
		list = append(list, *a)
	}
	out.ApprovalRates = val(list)
	return nil
}

// ApprovalGroup is the tier a proposal is counted under: T1 an order that only reduces
// exposure, T2 a mandate, T3 any other order; memory and reflection kinds have none.
func ApprovalGroup(k approval.Kind, reducing bool) string {
	switch {
	case k == approval.KindOrder && reducing:
		return "T1"
	case k == approval.KindOrder:
		return "T3"
	case k == approval.KindMandate:
		return "T2"
	case k == approval.KindMemory:
		return "memory"
	case approval.IsReflectionKind(k):
		return "reflection"
	}
	return "T3" // an unknown kind is counted as the least trusted tier
}

func (r *Reader) harmMetric(ctx context.Context, now time.Time) Metric {
	if r.Harm == nil {
		return none("learning is off, so the owner's fills are not recorded (proactive.learning)")
	}
	h, err := r.Harm.HarmMetric(ctx, now)
	if err != nil {
		return none("the harm metric could not be read")
	}
	if len(h.Days) == 0 {
		return none("no complete day with a fill history yet")
	}
	return val(map[string]any{
		"window_days": len(h.Days), "alert_days": h.AlertDays, "quiet_days": h.QuietDays,
		"post_alert_trades": h.PostAlertTrades, "quiet_trades": h.QuietTrades,
		"post_alert_contract_trades": h.PostAlertContractTrades, "quiet_contract_trades": h.QuietContractTrades,
		"post_alert_per_alert_day": ratio(h.PostAlertTrades, h.AlertDays), "quiet_per_quiet_day": ratio(h.QuietTrades, h.QuietDays),
		"post_alert_contract_per_alert_day": ratio(h.PostAlertContractTrades, h.AlertDays),
		"quiet_contract_per_quiet_day":      ratio(h.QuietContractTrades, h.QuietDays),
	})
}
