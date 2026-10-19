package feedback

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The harm guardrail.
//
// Proactive messages can push an owner to trade (the effect regulators found in
// push-notification trading apps). This is the number that watches for it: how often
// the owner trades in the 24 hours after being messaged, against days with no message.
// It is SURFACED (the dashboard shows it) and never optimised for: nothing in
// pkg/attention, pkg/triggers or pkg/policy may import this package (see
// TestHarmMetricIsNotADecisionInput), and no code path reads it to decide anything.
//
// Definition, over the HarmDays complete owner-local days before the day of `now`:
//
//   - an alert is a message delivered to the owner (an intervention with notify_sent);
//     an alert day is a day with at least one, a quiet day one with none;
//   - a trade is a fill in feedback_fills (a real, unscheduled order fill; a paper order
//     and a scheduled DCA buy are not); a contract trade is one whose symbol holds ":"
//     ("BTC/USDT:USDT"), the proxy for leverage: a perpetual swap is a leveraged
//     position, a spot symbol is not;
//   - PostAlert counts, over alert days, the trades that happened within 24 hours after
//     some alert (an alert at 20:00 shields a trade at 09:00 the next day);
//   - Quiet counts all the trades of quiet days;
//   - the rates are those counts per alert day and per quiet day.
//
// The fills log only holds what happened while the recorder ran, so days before it began
// (FillsSince) are left out rather than counted as days with no trades.

// HarmDays is the length of the trailing window, in days.
const HarmDays = 30

// HarmDay is one local day.
type HarmDay struct {
	Day                     string // 2006-01-02, owner-local
	Alerts                  int
	Trades                  int
	ContractTrades          int
	PostAlertTrades         int
	PostAlertContractTrades int
}

// Harm is the harm guardrail metric.
type Harm struct {
	Days                                                   []HarmDay
	AlertDays, QuietDays                                   int
	PostAlertTrades, PostAlertContractTrades               int // over alert days
	QuietTrades, QuietContractTrades                       int // over quiet days
	PostAlertPerAlertDay, QuietPerQuietDay                 float64
	PostAlertContractPerAlertDay, QuietContractPerQuietDay float64
}

type fillAt struct {
	at       time.Time
	contract bool
}

// harm computes the metric from alert delivery times and fills. It is pure. first is the
// first local day (midnight) it may count, last the day after the final one; days are
// the local days in [first, last) in loc, at most HarmDays.
func harm(alerts []time.Time, fills []fillAt, first, last time.Time, loc *time.Location) Harm {
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Before(alerts[j]) })
	postAlert := func(t time.Time) bool {
		i := sort.Search(len(alerts), func(i int) bool { return alerts[i].After(t) }) // alerts[:i] are <= t
		return i > 0 && t.Sub(alerts[i-1]) < TradeWindow
	}
	var out Harm
	idx := map[string]int{}
	for d := first.In(loc); d.Before(last); d = time.Date(d.Year(), d.Month(), d.Day()+1, 0, 0, 0, 0, loc) {
		idx[d.Format("2006-01-02")] = len(out.Days)
		out.Days = append(out.Days, HarmDay{Day: d.Format("2006-01-02")})
	}
	for _, a := range alerts {
		if i, ok := idx[a.In(loc).Format("2006-01-02")]; ok {
			out.Days[i].Alerts++
		}
	}
	for _, f := range fills {
		i, ok := idx[f.at.In(loc).Format("2006-01-02")]
		if !ok {
			continue
		}
		d := &out.Days[i]
		d.Trades++
		if f.contract {
			d.ContractTrades++
		}
		if postAlert(f.at) {
			d.PostAlertTrades++
			if f.contract {
				d.PostAlertContractTrades++
			}
		}
	}
	for _, d := range out.Days {
		if d.Alerts > 0 {
			out.AlertDays++
			out.PostAlertTrades += d.PostAlertTrades
			out.PostAlertContractTrades += d.PostAlertContractTrades
		} else {
			out.QuietDays++
			out.QuietTrades += d.Trades
			out.QuietContractTrades += d.ContractTrades
		}
	}
	out.PostAlertPerAlertDay = perDay(out.PostAlertTrades, out.AlertDays)
	out.PostAlertContractPerAlertDay = perDay(out.PostAlertContractTrades, out.AlertDays)
	out.QuietPerQuietDay = perDay(out.QuietTrades, out.QuietDays)
	out.QuietContractPerQuietDay = perDay(out.QuietContractTrades, out.QuietDays)
	return out
}

func perDay(n, days int) float64 {
	if days == 0 {
		return 0
	}
	return float64(n) / float64(days)
}

// HarmMetric reads the stored alerts and fills and returns the guardrail metric as of
// now, in the owner's zone. Safe on nil (an empty metric). It is for the dashboard.
func (s *Service) HarmMetric(ctx context.Context, now time.Time) (Harm, error) {
	if s == nil {
		return Harm{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 4*dbTimeout)
	defer cancel()
	loc := time.UTC
	if s.zone != nil {
		if l := s.zone(ctx); l != nil {
			loc = l
		}
	}
	y, m, d := now.In(loc).Date()
	last := time.Date(y, m, d, 0, 0, 0, 0, loc)
	first := time.Date(y, m, d-HarmDays, 0, 0, 0, 0, loc)
	// Days that began before the fills log did are unknown, not empty.
	for first.Before(s.fillsSince) && first.Before(last) {
		first = time.Date(first.Year(), first.Month(), first.Day()+1, 0, 0, 0, 0, loc)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT MIN(d.delivered_at_ms) FROM attention_decisions d
		  WHERE d.delivered = 1 AND d.delivered_at_ms >= ? AND d.delivered_at_ms < ?
		    AND EXISTS (SELECT 1 FROM notify_sent n WHERE n.intervention_id = COALESCE(d.intervention_id, d.id))
		  GROUP BY COALESCE(d.intervention_id, d.id)`,
		first.Add(-TradeWindow).UnixMilli(), last.UnixMilli())
	if err != nil {
		return Harm{}, fmt.Errorf("feedback: read alerts: %w", err)
	}
	var alerts []time.Time
	for rows.Next() {
		var ms int64
		if err := rows.Scan(&ms); err != nil {
			_ = rows.Close()
			return Harm{}, fmt.Errorf("feedback: read alerts: %w", err)
		}
		alerts = append(alerts, time.UnixMilli(ms))
	}
	if err := rows.Close(); err != nil {
		return Harm{}, err
	}
	frows, err := s.db.QueryContext(ctx, `SELECT at_ms, symbol FROM feedback_fills WHERE at_ms >= ? AND at_ms < ?`,
		first.UnixMilli(), last.UnixMilli())
	if err != nil {
		return Harm{}, fmt.Errorf("feedback: read fills: %w", err)
	}
	defer frows.Close()
	var fills []fillAt
	for frows.Next() {
		var ms int64
		var sym string
		if err := frows.Scan(&ms, &sym); err != nil {
			return Harm{}, fmt.Errorf("feedback: read fills: %w", err)
		}
		fills = append(fills, fillAt{at: time.UnixMilli(ms), contract: strings.Contains(sym, ":")})
	}
	if err := frows.Err(); err != nil {
		return Harm{}, err
	}
	return harm(alerts, fills, first, last, loc), nil
}
