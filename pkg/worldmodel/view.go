package worldmodel

import (
	"sort"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// View returns the read side of the store. It is a handle, not a snapshot: every
// method takes the read lock, copies out what it returns and releases it, so it
// is cheap to call on every event and safe from any goroutine. Nothing it returns
// aliases the store's memory.
func (s *Store) View() View { return view{s} }

type view struct{ s *Store }

func (v view) Now() time.Time { return v.s.clock.Now() }

func (v view) Fresh(at time.Time, maxAge time.Duration) bool {
	if at.IsZero() || maxAge < 0 {
		return false
	}
	now := v.s.clock.Now()
	if at.After(now.Add(freshSkew)) {
		return false
	}
	return now.Sub(at) <= maxAge
}

func (v view) Price(provider, symbol string) (float64, time.Time, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	sr := v.s.series[seriesKey{provider, symbol}]
	if sr == nil {
		return 0, time.Time{}, false
	}
	return sr.price, sr.at, true
}

func (v view) PriceTrust(provider, symbol string) (proactive.Trust, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	sr := v.s.series[seriesKey{provider, symbol}]
	if sr == nil {
		return proactive.TrustUntrusted, false
	}
	return sr.trust, true
}

func (v view) Portfolio() (PortfolioView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	p := v.s.pf
	if p == nil {
		return PortfolioView{}, false
	}
	out := PortfolioView{
		At: p.At, SnapshotID: p.SnapshotID, TotalUSD: p.TotalUSD,
		ByClass:            make(map[string]float64, len(p.ByClass)),
		Positions:          append([]PositionView(nil), p.Positions...),
		PositionsTruncated: p.Truncated,
		UnpricedCount:      p.UnpricedN, UnpricedTruncated: p.UnpricedTruncated,
		DayStartUSD: p.DayStartUSD, DayStartAt: p.DayStartAt, DayDate: p.DayDate, DayStartRebased: p.DayStartRebased,
		Trust: p.Trust,
	}
	for k, x := range p.ByClass {
		out.ByClass[k] = x
	}
	for _, d := range p.Peaks {
		out.Peak30dUSD = max(out.Peak30dUSD, d.Max)
	}
	return out, true
}

// accountDegradedLocked says the futures source cannot read the account. The
// caller holds the read lock.
func (s *Store) accountDegradedLocked(account string) bool {
	st := s.sources[FuturesSource]
	if st == nil {
		return false
	}
	if st.whole {
		return true
	}
	_, bad := st.failed[account]
	return bad
}

func (v view) FuturesPosition(account, symbol string) (FuturesView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	fa := v.s.futures[account]
	if fa == nil {
		return FuturesView{}, false
	}
	// In hedge mode a symbol has a long and a short; report the larger.
	var best FuturesView
	found := false
	for _, p := range fa.positions {
		if p.Symbol != symbol {
			continue
		}
		if !found || p.Contracts > best.Contracts || (p.Contracts == best.Contracts && p.Side < best.Side) {
			best, found = p, true
		}
	}
	if found {
		best.AccountDegraded = v.s.accountDegradedLocked(account)
	}
	return best, found
}

func (v view) FuturesPositions(account string) ([]FuturesView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	fa := v.s.futures[account]
	if fa == nil {
		return nil, false
	}
	deg := v.s.accountDegradedLocked(account)
	out := make([]FuturesView, 0, len(fa.positions))
	for _, p := range fa.positions {
		p.AccountDegraded = deg
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Symbol != out[j].Symbol {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Side < out[j].Side
	})
	return out, true
}

func (v view) Plan(kind string, id int64) (PlanView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	ps := v.s.plans[planKey{kind, id}]
	if ps == nil {
		return PlanView{}, false
	}
	out := ps.view
	if ps.view.Health != nil {
		out.Health = make(map[string]float64, len(ps.view.Health))
		for k, x := range ps.view.Health {
			out.Health[k] = x
		}
	}
	return out, true
}

func (v view) Series(name string) (SeriesView, bool) {
	rest, ok := strings.CutPrefix(name, "price:")
	if !ok {
		return SeriesView{}, false
	}
	hour := false
	if r, ok := strings.CutSuffix(rest, "@1h"); ok {
		rest, hour = r, true
	} else if r, ok := strings.CutSuffix(rest, "@1m"); ok {
		rest = r
	}
	prov, sym, ok := strings.Cut(rest, ":")
	if !ok {
		return SeriesView{}, false
	}
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	sr := v.s.series[seriesKey{prov, sym}]
	if sr == nil {
		return SeriesView{}, false
	}
	out := SeriesView{At: sr.at, Trust: sr.trust}
	if hour {
		out.Name = "price:" + prov + ":" + sym + "@1h"
		out.Bucket, out.Mean, out.Sigma, out.Samples = sr.h1.Bucket, sr.h1.E.Mean, sr.h1.E.Sigma(), sr.h1.E.N
		out.LastZ, out.LastZValid = sr.zh, sr.zhok
		return out, true
	}
	out.Name = "price:" + prov + ":" + sym + "@1m"
	out.Bucket, out.Mean, out.Sigma, out.Samples = sr.m1.Bucket, sr.m1.E.Mean, sr.m1.E.Sigma(), sr.m1.E.N
	out.LastZ, out.LastZValid = sr.z1, sr.z1ok
	out.CUSUMPos, out.CUSUMNeg = sr.cusum.Pos, sr.cusum.Neg
	return out, true
}

func (v view) Source(name string) (perception.SourceHealth, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	st := v.s.sources[name]
	if st == nil {
		return perception.SourceHealth{}, false
	}
	return perception.SourceHealth{
		LastEventAt: st.lastEvent, LastErrorAt: st.lastErr, LastError: st.reason, ConsecutiveErrors: st.consecutive,
	}, true
}

func (v view) SourceState(name string) (SourceView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	st := v.s.sources[name]
	if st == nil {
		return SourceView{}, false
	}
	out := SourceView{
		Name: name, Degraded: st.degraded(), Reason: st.reason,
		LastEventAt: st.lastEvent, LastErrorAt: st.lastErr, ConsecutiveDegraded: st.consecutive,
	}
	for n := range st.failed {
		out.Failed = append(out.Failed, n)
	}
	sort.Strings(out.Failed)
	return out, true
}

func (v view) LastFill(account, symbol string) (FillView, bool) {
	v.s.mu.RLock()
	defer v.s.mu.RUnlock()
	fs := v.s.fills[fillKey{account, symbol}]
	if fs == nil {
		return FillView{}, false
	}
	return fs.view, true
}
