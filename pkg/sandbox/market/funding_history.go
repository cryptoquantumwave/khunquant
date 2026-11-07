package market

import (
	"net/url"
	"strconv"
	"time"
)

type fundingRow struct {
	at         time.Time
	rate, mark float64
}

// fundingRows lists the settlements of p's perpetual in [from, to] (none in the future),
// oldest first, with the rate paid at each and the mark of that instant: the same numbers
// the simulator pays funding with. At most limit rows are walked.
func (b *Book) fundingRows(venue string, p Pair, from, to time.Time, limit int) []fundingRow {
	to = minTime(to, b.Now())
	var rows []fundingRow
	sched := b.schedule(venue, p)
	for f := sched.Next(from.Add(-time.Nanosecond)); !f.After(to) && len(rows) < limit; f = sched.Next(f) {
		mark, ok := b.Price(venue, p, f)
		if !ok {
			continue
		}
		b.mu.RLock()
		rate := b.termsAt(venue, p, f).rate
		b.mu.RUnlock()
		rows = append(rows, fundingRow{f, rate, mark})
	}
	return rows
}

// queryTime reads a millisecond timestamp parameter: the zero time when it is absent.
func queryTime(q url.Values, key string) time.Time {
	if t, ok := queryMS(q, key); ok {
		return t
	}
	return time.Time{}
}

// historyFrom is where a history request with no start begins: limit settlements before to.
func historyFrom(from, to time.Time, limit int, s FundingSchedule) time.Time {
	if from.IsZero() {
		return to.Add(-time.Duration(limit) * s.Interval)
	}
	return from
}

// binanceFundingRate serves /fapi/v1/fundingRate: the settlements of one symbol, oldest
// first. With a startTime the first `limit` from it, else the latest `limit`.
func (b *Book) binanceFundingRate(q url.Values) (int, []byte, bool) {
	p, _, ok := ParseWire("binance", q.Get("symbol"))
	if !ok {
		return 0, nil, false
	}
	if _, has := b.Price("binance", p, b.Now()); !has {
		return 0, nil, false
	}
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 1000)
	}
	start, to := queryTime(q, "startTime"), queryTime(q, "endTime")
	if to.IsZero() {
		to = b.Now()
	}
	rows := b.fundingRows("binance", p, historyFrom(start, to, limit, b.schedule("binance", p)), to, 1<<16)
	if len(rows) > limit {
		if start.IsZero() {
			rows = rows[len(rows)-limit:]
		} else {
			rows = rows[:limit]
		}
	}
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, map[string]any{"symbol": q.Get("symbol"), "fundingRate": num(r.rate), "fundingTime": ms(r.at), "markPrice": num(r.mark)})
	}
	return reply(200, out)
}

// okxFundingHistory serves /api/v5/public/funding-rate-history: one swap's settlements,
// newest first; before is a timestamp the rows are newer than, after one they are older than.
func (b *Book) okxFundingHistory(q url.Values) (int, []byte, bool) {
	p, swap, ok := ParseWire("okx", q.Get("instId"))
	if !ok || !swap {
		return 0, nil, false
	}
	if _, has := b.Price("okx", p, b.Now()); !has {
		return 0, nil, false
	}
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, 100)
	}
	newer, older := queryTime(q, "before"), queryTime(q, "after")
	to := b.Now()
	if !older.IsZero() {
		to = older.Add(-time.Millisecond)
	}
	var from time.Time
	if !newer.IsZero() {
		from = newer.Add(time.Millisecond)
	}
	from = historyFrom(from, to, limit, b.schedule("okx", p))
	rows, out := b.fundingRows("okx", p, from, to, 1<<16), []map[string]string{}
	for i := len(rows) - 1; i >= 0 && len(out) < limit; i-- {
		r := rows[i]
		out = append(out, map[string]string{"instType": "SWAP", "instId": q.Get("instId"), "fundingRate": num(r.rate), "realizedRate": num(r.rate),
			"fundingTime": strconv.FormatInt(ms(r.at), 10), "method": "current_period"})
	}
	return okxReply(out)
}
