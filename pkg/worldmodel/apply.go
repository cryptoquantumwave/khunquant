package worldmodel

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// Apply folds one event into the model and reports what changed. It must be called
// from one goroutine only (the pipeline's). It never writes to the database, never
// panics (a panic is recovered, counted and returned as an error) and is
// deterministic: its output depends only on the events applied so far, the
// injected clock (read once, to place an event that carries no time) and the zone
// function.
//
// An event the model does not track, and an input it refuses (a non-finite number,
// a name over its bound, a simulated order), gives an empty Diff and no error; the
// refusal is counted in Stats. An error means the payload of a tracked kind could
// not be decoded, or Apply panicked.
func (s *Store) Apply(_ context.Context, ev events.Event) (d Diff, err error) {
	d = Diff{Event: envelope(ev)}
	defer func() {
		if r := recover(); r != nil {
			s.c.panics.Add(1)
			d = Diff{Event: envelope(ev), At: d.At}
			err = fmt.Errorf("worldmodel: applying %s panicked: %v", ev.Kind, r)
		}
		s.afterApply()
	}()

	at := s.eventTime(ev)
	d.At = at
	switch ev.Kind {
	case events.KindPerceptionMarketTick:
		err = s.applyTick(ev, at, &d)
	case events.KindPerceptionPortfolioSnapshot:
		err = s.applyPortfolio(ev, at, &d)
	case events.KindPerceptionFuturesPositions:
		err = s.applyFutures(ev, at, &d)
	case events.KindPerceptionPlanHealth, events.KindPerceptionPlanDCADue:
		err = s.applyPlan(ev, at, &d)
	case events.KindPerceptionSourceHealth:
		err = s.applyHealth(ev, at, &d)
	case events.KindPerceptionOrderFilled:
		err = s.applyFill(ev, at, &d)
	case events.KindPerceptionClockDayStart:
		err = s.applyDayStart(ev)
	default:
		s.c.ignored.Add(1)
		d.Payload = ev.Payload // for the trigger that handles this kind; see Diff.Payload
		return d, nil
	}
	if errors.Is(err, errNonFinite) {
		s.c.droppedInvalid.Add(1)
		return Diff{Event: d.Event, At: at}, nil
	}
	if err != nil {
		s.c.decodeErrors.Add(1)
		return Diff{Event: d.Event, At: at}, fmt.Errorf("worldmodel: %s: %w", ev.Kind, err)
	}
	s.c.applied.Add(1)
	return d, nil
}

// Tracks reports whether the model folds events of this kind into its state. An
// event of any other kind (a session boundary, a device, an external trigger, a
// placed order) is ignored by the model and never changes a Diff, so a consumer
// that wants to be told about those has to be passed them on, which is what the
// gateway's stage adapter does.
func Tracks(kind events.Kind) bool {
	switch kind {
	case events.KindPerceptionMarketTick, events.KindPerceptionPortfolioSnapshot, events.KindPerceptionFuturesPositions,
		events.KindPerceptionPlanHealth, events.KindPerceptionPlanDCADue, events.KindPerceptionSourceHealth,
		events.KindPerceptionOrderFilled, events.KindPerceptionClockDayStart:
		return true
	}
	return false
}

// envelope is the event without its payload and attributes.
func envelope(ev events.Event) events.Event {
	return events.Event{
		ID: ev.ID, Kind: ev.Kind, Time: ev.Time, Source: ev.Source,
		Correlation: ev.Correlation, Severity: ev.Severity, Trust: ev.Trust,
	}
}

// eventTime is when the event happened: its own stamp, or the clock when it has
// none or is stamped more than futureSkew ahead.
func (s *Store) eventTime(ev events.Event) time.Time {
	if ev.Time.IsZero() {
		return s.clock.Now()
	}
	if now := s.clock.Now(); ev.Time.After(now.Add(futureSkew)) {
		s.c.futureClamped.Add(1)
		return now
	}
	return ev.Time
}

// afterApply logs the warnings that were noted while the lock was held.
func (s *Store) afterApply() {
	if s.noteDropped.Swap(false) {
		s.warn("log-drop", "World model change log is full; oldest changes dropped", nil)
	}
	if s.noteEvicted.Swap(false) {
		s.warn("evict", "World model series limit reached; least recently updated series evicted", nil)
	}
}

// ---- recording ------------------------------------------------------------

func (s *Store) touchRow(k rowKey) {
	s.ver++
	s.dirtyRows[k] = s.ver
}

func (s *Store) touchDet(k detKey) {
	s.ver++
	s.dirtyDets[k] = s.ver
}

// record adds a change to the diff and, unless it is minor, to the change log.
// The caller holds the write lock.
func (s *Store) record(d *Diff, at time.Time, c Change) {
	d.Changes = append(d.Changes, c)
	if !c.Minor {
		s.logChange(at, c.Subject, c.Key, c.Old, c.New, c.Trust)
	}
}

// recordSignal adds a fired signal to the diff and to the change log, as an entry
// whose key is "signal:<detector>" and whose new value is the signal's value.
func (s *Store) recordSignal(d *Diff, at time.Time, g Signal) {
	d.Signals = append(d.Signals, g)
	s.logChange(at, g.Series, signalPrefix+g.Detector, nil, g.Value, g.Trust)
}

// noteSource records that a source spoke. The caller holds the write lock.
func (s *Store) noteSource(name string, at time.Time) *sourceState {
	if !validToken(name, MaxTokenLen) {
		return nil
	}
	st := s.sources[name]
	if st == nil {
		if len(s.sources) >= MaxSources {
			return nil
		}
		st = &sourceState{}
		s.sources[name] = st
	}
	if at.After(st.lastEvent) {
		st.lastEvent = at
	}
	return st
}

// ---- market ticks ---------------------------------------------------------

func (s *Store) applyTick(ev events.Event, at time.Time, d *Diff) error {
	p, err := decode[TickPayload](ev.Payload)
	if err != nil {
		return err
	}
	if !validToken(p.Provider, MaxTokenLen) || !validSymbol(p.Symbol, MaxSymbolLen) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	if !finite(p.Last) || p.Last <= 0 {
		s.c.droppedInvalid.Add(1)
		return nil
	}
	trust := ev.TrustLevel()
	k := seriesKey{p.Provider, p.Symbol}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)

	sr := s.series[k]
	if sr == nil {
		if len(s.series) >= MaxSeries {
			s.evictSeriesLocked()
		}
		sr = newSeries()
		s.series[k] = sr
	} else if at.Before(sr.at) {
		s.c.droppedOutOfOrder.Add(1)
		return nil
	}
	sr.price, sr.at, sr.trust = p.Last, at, trust
	sr.src = sourceTag(ev.Source.Name)
	s.touchRow(rowKey{kind: rkPrice, a: k.provider, b: k.symbol})

	// One-minute bucket.
	if c, closed := sr.m1.Observe(at, p.Last); closed {
		ct := sr.tr1.observe(trust, true)
		s.onMinuteClose(d, at, k, sr, c, ct)
	} else {
		sr.tr1.observe(trust, false)
	}
	s.touchDet(detKey{k, detMinute})

	// One-hour bucket.
	if c, closed := sr.h1.Observe(at, p.Last); closed {
		ct := sr.trh.observe(trust, true)
		s.onHourClose(d, at, k, sr, c, ct)
	} else {
		sr.trh.observe(trust, false)
	}
	s.touchDet(detKey{k, detHour})
	return nil
}

func (s *Store) onMinuteClose(d *Diff, at time.Time, k seriesKey, sr *series, c closeInfo, trust proactive.Trust) {
	if c.PrevClose > 0 && c.Close != c.PrevClose {
		s.record(d, at, Change{Subject: k.subject(), Key: "close_1m", Old: c.PrevClose, New: c.Close, Trust: trust, Minor: true})
	}
	if !c.HasReturn {
		return
	}
	sr.z1, sr.z1ok = c.Z, c.ZValid
	if !c.ZValid {
		return
	}
	name := k.subject() + "@1m"
	if math.Abs(c.Z) >= zFire {
		s.recordSignal(d, at, Signal{
			Series: name, Detector: "ewma_z", Value: c.Z, Fired: true, Trust: trust,
			Detail: map[string]float64{"z": c.Z, "return": c.Return, "mean": sr.m1.E.Mean, "sigma": sr.m1.E.Sigma()},
		})
	}
	fired, dir, stat := sr.cusum.Update(c.Z)
	s.touchDet(detKey{k, detCUSUM})
	if fired {
		s.recordSignal(d, at, Signal{
			Series: name, Detector: "cusum", Value: float64(dir) * stat, Fired: true, Trust: trust,
			Detail: map[string]float64{"z": c.Z, "threshold": sr.cusum.H},
		})
	}
}

func (s *Store) onHourClose(d *Diff, at time.Time, k seriesKey, sr *series, c closeInfo, trust proactive.Trust) {
	if c.PrevClose > 0 && c.Close != c.PrevClose {
		s.record(d, at, Change{Subject: k.subject(), Key: "close_1h", Old: c.PrevClose, New: c.Close, Trust: trust})
	}
	if !c.HasReturn {
		return
	}
	sr.zh, sr.zhok = c.Z, c.ZValid
	if c.ZValid && math.Abs(c.Z) >= zFire {
		s.recordSignal(d, at, Signal{
			Series: k.subject() + "@1h", Detector: "ewma_z", Value: c.Z, Fired: true, Trust: trust,
			Detail: map[string]float64{"z": c.Z, "return": c.Return, "mean": sr.h1.E.Mean, "sigma": sr.h1.E.Sigma()},
		})
	}
}

// evictSeriesLocked removes the least recently updated tenth of the series, so
// that the sort is paid once per MaxSeries/10 new symbols. The caller holds the
// write lock.
func (s *Store) evictSeriesLocked() {
	n := max(MaxSeries/10, 1)
	gone := evictOldest(s.series, n, func(sr *series) time.Time { return sr.at },
		func(a, b seriesKey) bool {
			if a.provider != b.provider {
				return a.provider < b.provider
			}
			return a.symbol < b.symbol
		})
	for _, k := range gone {
		s.touchRow(rowKey{kind: rkPrice, a: k.provider, b: k.symbol})
		for _, w := range []uint8{detMinute, detHour, detCUSUM} {
			s.touchDet(detKey{k, w})
		}
	}
	s.c.seriesEvicted.Add(uint64(len(gone)))
	s.noteEvicted.Store(true)
}

// sourceTag is a source name usable as a database tag, or "".
func sourceTag(name string) string {
	if validToken(name, MaxTokenLen) {
		return name
	}
	return ""
}

// ---- portfolio ------------------------------------------------------------

const dateLayout = "2006-01-02"

func (s *Store) applyPortfolio(ev events.Event, at time.Time, d *Diff) error {
	p, err := decode[PortfolioPayload](ev.Payload)
	if err != nil {
		return err
	}
	if !p.TakenAt.IsZero() && !p.TakenAt.After(at.Add(futureSkew)) {
		at = p.TakenAt
		d.At = at
	}
	if q := p.Quote; q != "" && q != "USD" && q != "USDT" {
		s.c.droppedInvalid.Add(1) // the total would not be dollars
		return nil
	}
	if !nonNegative(p.TotalValueUSD) || len(p.ByClass) > MaxClasses {
		s.c.droppedInvalid.Add(1)
		return nil
	}
	byClass := make(map[string]float64, len(p.ByClass))
	for name, v := range p.ByClass {
		if !validToken(name, MaxTokenLen) {
			s.c.droppedBadName.Add(1)
			return nil
		}
		if !nonNegative(v) {
			s.c.droppedInvalid.Add(1)
			return nil
		}
		byClass[name] = v
	}
	positions := make([]PositionView, 0, min(len(p.Positions), MaxPortfolioPositions))
	for _, q := range p.Positions {
		if !nonNegative(q.Value) || !nonNegative(q.Qty) {
			s.c.droppedInvalid.Add(1)
			return nil
		}
		if !validSymbol(q.Asset, MaxSymbolLen) || !validToken(q.Source, MaxTokenLen) || (q.Class != "" && !validToken(q.Class, MaxTokenLen)) {
			s.c.droppedBadName.Add(1)
			continue
		}
		positions = append(positions, PositionView{Asset: q.Asset, Venue: q.Source, Class: q.Class, Value: q.Value, Qty: q.Qty})
	}
	truncated := p.Truncated
	if len(positions) > MaxPortfolioPositions {
		positions, truncated = positions[:MaxPortfolioPositions], true
		s.c.truncated.Add(1)
	}
	sort.Slice(positions, func(i, j int) bool {
		a, b := positions[i], positions[j]
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		if a.Venue != b.Venue {
			return a.Venue < b.Venue
		}
		return a.Asset < b.Asset
	})
	key := unpricedKey(p.Unpriced, p.UnpricedTruncated, at)

	loc, fromFunc := s.resolveZone()
	date := at.In(loc).Format(dateLayout)
	cutoff := at.In(loc).AddDate(0, 0, -29).Format(dateLayout)
	trust := ev.TrustLevel()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)
	if fromFunc {
		s.lastLoc = loc
	}

	prev := s.pf
	if prev != nil && at.Before(prev.At) {
		s.c.droppedOutOfOrder.Add(1)
		return nil
	}
	// A complete snapshot proves every account could be read.
	s.recoverSource(d, at, PortfolioSource, nil, trust)
	cur := &portfolioState{
		At: at, SnapshotID: p.SnapshotID, TotalUSD: p.TotalValueUSD, ByClass: byClass,
		Positions: positions, Truncated: truncated,
		Key: key, UnpricedN: len(p.Unpriced), UnpricedTruncated: p.UnpricedTruncated,
		Trust: trust, Src: sourceTag(ev.Source.Name),
	}

	comparable := prev != nil && prev.Key == key
	if prev != nil {
		ct := proactive.MinTrust(prev.Trust, trust)
		if comparable {
			s.portfolioChanges(d, at, prev, cur, ct)
		} else {
			// The pair is not comparable: an asset entering or leaving the valuation
			// would read as a gain or a loss. No change fact for the values, and every
			// baseline moves to this snapshot below.
			s.c.snapshotsRebased.Add(1)
			if prev.UnpricedN != cur.UnpricedN {
				s.record(d, at, Change{Subject: "portfolio", Key: "unpriced_assets", Old: prev.UnpricedN, New: cur.UnpricedN, Trust: ct})
			}
		}
	}

	// The day-start baseline: the first complete snapshot of the owner's local day,
	// whether or not a day_start event was seen.
	switch {
	case prev == nil || prev.DayDate != date:
		cur.DayDate, cur.DayStartUSD, cur.DayStartAt, cur.DayKey = date, cur.TotalUSD, at, key
	case prev.DayKey != key:
		cur.DayDate, cur.DayStartUSD, cur.DayStartAt, cur.DayKey = date, cur.TotalUSD, at, key
		cur.DayStartRebased = true
	default:
		cur.DayDate, cur.DayStartUSD, cur.DayStartAt, cur.DayKey = prev.DayDate, prev.DayStartUSD, prev.DayStartAt, prev.DayKey
		cur.DayStartRebased = prev.DayStartRebased
	}

	// The 30-day peak, from daily maxima on the same footing.
	if prev != nil && prev.PeakKey == key {
		cur.Peaks = append(cur.Peaks, prev.Peaks...)
	}
	cur.PeakKey = key
	cur.Peaks = bumpPeak(cur.Peaks, date, cur.TotalUSD, cutoff)

	s.pf = cur
	s.touchRow(rowKey{kind: rkPortfolio})
	return nil
}

// PortfolioSource and FuturesSource are the names under which the perception
// sources report their health.
const (
	PortfolioSource = "portfolio"
	FuturesSource   = "futures"
)

// bumpPeak records total under date, drops days before cutoff and keeps the days
// in order.
func bumpPeak(peaks []dayPeak, date string, total float64, cutoff string) []dayPeak {
	found := false
	for i := range peaks {
		if peaks[i].Date == date {
			peaks[i].Max = math.Max(peaks[i].Max, total)
			found = true
		}
	}
	if !found {
		peaks = append(peaks, dayPeak{Date: date, Max: total})
	}
	out := peaks[:0]
	for _, p := range peaks {
		if p.Date >= cutoff {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	if len(out) > 31 {
		out = out[len(out)-31:]
	}
	return out
}

// unpricedKey identifies a set of unpriced assets: a hash of the sorted list. A
// list the source had to cut is never comparable to anything (equal visible
// prefixes can hide different sets), so its key is unique to the snapshot.
func unpricedKey(list []string, truncated bool, at time.Time) string {
	if truncated {
		return "truncated@" + strconv.FormatInt(at.UnixNano(), 10)
	}
	if len(list) == 0 {
		return ""
	}
	sorted := append([]string(nil), list...)
	sort.Strings(sorted)
	h := fnv.New64a()
	for _, e := range sorted {
		_, _ = h.Write([]byte(e))
		_, _ = h.Write([]byte{'\n'})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func (s *Store) portfolioChanges(d *Diff, at time.Time, prev, cur *portfolioState, trust proactive.Trust) {
	if prev.TotalUSD != cur.TotalUSD {
		s.record(d, at, Change{Subject: "portfolio", Key: "total_usd", Old: prev.TotalUSD, New: cur.TotalUSD, Trust: trust})
	}
	names := make([]string, 0, len(cur.ByClass)+len(prev.ByClass))
	for n := range cur.ByClass {
		names = append(names, n)
	}
	for n := range prev.ByClass {
		if _, ok := cur.ByClass[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		ov, oldOK := prev.ByClass[n]
		nv, newOK := cur.ByClass[n]
		if oldOK == newOK && ov == nv {
			continue
		}
		c := Change{Subject: "portfolio", Key: "class:" + n, Trust: trust}
		if oldOK {
			c.Old = ov
		}
		if newOK {
			c.New = nv
		}
		s.record(d, at, c)
	}

	// Holdings that appeared or are gone. A snapshot that was cut short cannot say
	// a holding is gone, so neither is compared then. Quantity drift is not a
	// change worth a line (staking rewards, dust).
	if prev.Truncated || cur.Truncated {
		return
	}
	type pk struct{ venue, asset string }
	was := make(map[pk]PositionView, len(prev.Positions))
	for _, q := range prev.Positions {
		was[pk{q.Venue, q.Asset}] = q
	}
	now := make(map[pk]PositionView, len(cur.Positions))
	for _, q := range cur.Positions {
		now[pk{q.Venue, q.Asset}] = q
	}
	var moves []Change
	var values []float64
	for _, q := range cur.Positions { // by value, largest first
		if _, ok := was[pk{q.Venue, q.Asset}]; !ok && q.Qty > 0 {
			moves = append(moves, Change{Subject: "position:" + q.Venue + ":" + q.Asset, Key: "qty", New: q.Qty, Trust: trust})
			values = append(values, q.Value)
		}
	}
	for _, q := range prev.Positions {
		if _, ok := now[pk{q.Venue, q.Asset}]; !ok && q.Qty > 0 {
			moves = append(moves, Change{Subject: "position:" + q.Venue + ":" + q.Asset, Key: "qty", Old: q.Qty, Trust: trust})
			values = append(values, q.Value)
		}
	}
	idx := make([]int, len(moves))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return values[idx[a]] > values[idx[b]] })
	for n, i := range idx {
		if n >= maxPositionChanges {
			s.c.truncated.Add(1)
			break
		}
		s.record(d, at, moves[i])
	}
}

// resolveZone is the owner's time zone: the zone function's answer, else the last
// answer it gave, else the zone named by the last day_start event, else UTC. It
// runs on the pipeline goroutine with no lock held, and only the pipeline
// goroutine writes the fields it reads, so reading them here is safe.
func (s *Store) resolveZone() (loc *time.Location, fromFunc bool) {
	if s.zone != nil {
		if l := s.zone(); l != nil {
			return l, true
		}
	}
	if s.lastLoc != nil {
		return s.lastLoc, false
	}
	if z := s.day.Zone; z != "" {
		if l, err := time.LoadLocation(z); err == nil {
			return l, false
		}
	}
	return time.UTC, false
}

// ---- futures --------------------------------------------------------------

func (s *Store) applyFutures(ev events.Event, at time.Time, d *Diff) error {
	p, err := decode[FuturesPayload](ev.Payload)
	if err != nil {
		return err
	}
	if !validToken(p.Provider, MaxTokenLen) || !validToken(p.Account, MaxTokenLen) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	if len(p.Positions) > MaxFuturesPositions {
		s.c.truncated.Add(1)
		s.c.droppedInvalid.Add(1) // a partial position list would read as closed positions
		return nil
	}
	if !p.At.IsZero() && !p.At.After(at.Add(futureSkew)) {
		at = p.At
		d.At = at
	}
	acct := p.Provider + "/" + p.Account
	trust := ev.TrustLevel()
	fresh := make(map[string]FuturesView, len(p.Positions))
	for _, q := range p.Positions {
		v, ok := futuresView(acct, q)
		switch ok {
		case posBadName:
			s.c.droppedBadName.Add(1)
			return nil
		case posInvalid:
			s.c.droppedInvalid.Add(1)
			return nil
		case posFlat:
			continue
		}
		v.At, v.Trust = at, trust
		fresh[posKey(v.Symbol, v.Side)] = v
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)

	fa := s.futures[acct]
	if fa != nil && at.Before(fa.at) {
		s.c.droppedOutOfOrder.Add(1)
		return nil
	}
	s.recoverSource(d, at, FuturesSource, []string{acct}, trust)
	if fa == nil {
		if len(s.futures) >= MaxFuturesAccounts {
			s.evictFuturesLocked()
		}
		fa = &futuresAccount{positions: map[string]FuturesView{}}
		s.futures[acct] = fa
	}
	ct := proactive.MinTrust(fa.trust, trust)
	if len(fa.positions) == 0 && fa.at.IsZero() {
		ct = trust
	}

	keys := make([]string, 0, len(fresh)+len(fa.positions))
	for k := range fresh {
		keys = append(keys, k)
	}
	for k := range fa.positions {
		if _, ok := fresh[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		old, hadOld := fa.positions[k]
		nw, hasNew := fresh[k]
		if hadOld == hasNew && old.Contracts == nw.Contracts {
			continue
		}
		v := nw
		if !hasNew {
			v = old
		}
		c := Change{Subject: "futures:" + acct + ":" + v.Symbol, Key: sideKey(v.Side), Trust: ct}
		if hadOld {
			c.Old = old.Contracts
		}
		if hasNew {
			c.New = nw.Contracts
		}
		s.record(d, at, c)
	}
	fa.positions, fa.at, fa.trust, fa.src = fresh, at, trust, sourceTag(ev.Source.Name)
	s.touchRow(rowKey{kind: rkFutures, a: acct})
	return nil
}

func sideKey(side string) string {
	if side == "" {
		return "position"
	}
	return side
}

// posKey keys a position within an account: hedge mode holds a long and a short in
// one symbol.
func posKey(symbol, side string) string { return symbol + "\x00" + side }

type posResult int

const (
	posOK posResult = iota
	posFlat
	posBadName
	posInvalid
)

// futuresView validates one payload position. A number whose Known flag is false
// is zeroed: it is absent, and nothing may read it.
func futuresView(acct string, q FuturesPositionPayload) (FuturesView, posResult) {
	if !validSymbol(q.Symbol, MaxSymbolLen) {
		return FuturesView{}, posBadName
	}
	if q.Side != "" && q.Side != "long" && q.Side != "short" {
		return FuturesView{}, posInvalid
	}
	if !nonNegative(q.Contracts) {
		return FuturesView{}, posInvalid
	}
	if q.Contracts == 0 {
		return FuturesView{}, posFlat
	}
	v := FuturesView{Account: acct, Symbol: q.Symbol, Side: q.Side, Contracts: q.Contracts}
	type num struct {
		known bool
		val   float64
		neg   bool // may be negative
		dst   *float64
		flag  *bool
	}
	for _, n := range []num{
		{q.ContractSizeKnown, q.ContractSize, false, &v.ContractSize, &v.ContractSizeKnown},
		{q.MarkPriceKnown, q.MarkPrice, false, &v.MarkPrice, &v.MarkPriceKnown},
		{q.LiqPriceKnown, q.LiqPrice, false, &v.LiqPrice, &v.LiqPriceKnown},
		{q.MarginRatioKnown, q.MarginRatio, false, &v.MarginRatio, &v.MarginRatioKnown},
		{q.UnrealizedPnlKnown, q.UnrealizedPnl, true, &v.UnrealizedPnl, &v.UnrealizedKnown},
		{q.NotionalKnown, q.NotionalUSD, false, &v.NotionalUSD, &v.NotionalKnown},
	} {
		if !n.known {
			continue
		}
		if !finite(n.val) || (!n.neg && n.val < 0) {
			return FuturesView{}, posInvalid
		}
		*n.dst, *n.flag = n.val, true
	}
	return v, posOK
}

func (s *Store) evictFuturesLocked() {
	gone := evictOldest(s.futures, 1, func(a *futuresAccount) time.Time { return a.at },
		func(a, b string) bool { return a < b })
	for _, k := range gone {
		s.touchRow(rowKey{kind: rkFutures, a: k})
	}
	s.c.factsEvicted.Add(uint64(len(gone)))
}

// ---- plans ----------------------------------------------------------------

func (s *Store) applyPlan(ev events.Event, at time.Time, d *Diff) error {
	p, err := decode[PlanPayload](ev.Payload)
	if err != nil {
		return err
	}
	if !validToken(p.Kind, MaxTokenLen) || p.ID < 0 || (p.Status != "" && !validToken(p.Status, MaxTokenLen)) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	if len(p.Health) > MaxHealthEntries {
		s.c.truncated.Add(1)
		s.c.droppedInvalid.Add(1)
		return nil
	}
	for name, v := range p.Health {
		if !validToken(name, MaxTokenLen) {
			s.c.droppedBadName.Add(1)
			return nil
		}
		if !finite(v) {
			s.c.droppedInvalid.Add(1)
			return nil
		}
	}
	if !p.At.IsZero() && !p.At.After(at.Add(futureSkew)) {
		at = p.At
		d.At = at
	}
	trust := ev.TrustLevel()
	k := planKey{p.Kind, p.ID}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)

	ps := s.plans[k]
	if ps != nil && at.Before(ps.view.At) {
		s.c.droppedOutOfOrder.Add(1)
		return nil
	}
	old := PlanView{}
	if ps == nil {
		if len(s.plans) >= MaxPlans {
			gone := evictOldest(s.plans, max(MaxPlans/10, 1), func(x *planState) time.Time { return x.view.At },
				func(a, b planKey) bool {
					if a.kind != b.kind {
						return a.kind < b.kind
					}
					return a.id < b.id
				})
			for _, g := range gone {
				s.touchRow(rowKey{kind: rkPlan, a: g.kind, n: g.id})
			}
			s.c.factsEvicted.Add(uint64(len(gone)))
		}
		ps = &planState{view: PlanView{Kind: p.Kind, ID: p.ID}}
		s.plans[k] = ps
	} else {
		old = ps.view
	}
	v := &ps.view
	if p.Status != "" {
		v.Status = p.Status
	}
	if len(p.Health) > 0 {
		v.Health = make(map[string]float64, len(p.Health))
		for n, x := range p.Health {
			v.Health[n] = x
		}
	}
	if !p.NextDue.IsZero() {
		v.NextDue = p.NextDue
	}
	v.At = at
	ct := trust
	if !old.At.IsZero() {
		ct = proactive.MinTrust(old.Trust, trust)
	}
	v.Trust = trust
	ps.src = sourceTag(ev.Source.Name)

	subject := "plan:" + p.Kind + ":" + strconv.FormatInt(p.ID, 10)
	if v.Status != old.Status {
		c := Change{Subject: subject, Key: "status", New: v.Status, Trust: ct}
		if old.Status != "" {
			c.Old = old.Status
		}
		s.record(d, at, c)
	}
	if !v.NextDue.Equal(old.NextDue) {
		c := Change{Subject: subject, Key: "next_due", New: v.NextDue.UTC().Format(time.RFC3339), Trust: ct}
		if !old.NextDue.IsZero() {
			c.Old = old.NextDue.UTC().Format(time.RFC3339)
		}
		s.record(d, at, c)
	}
	s.touchRow(rowKey{kind: rkPlan, a: p.Kind, n: p.ID})
	return nil
}

// ---- source health --------------------------------------------------------

func (s *Store) applyHealth(ev events.Event, at time.Time, d *Diff) error {
	h, err := decode[HealthPayload](ev.Payload)
	if err != nil {
		return err
	}
	if !validToken(h.Source, MaxTokenLen) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	var failed []string
	for _, n := range h.FailedAccounts {
		if !validSymbol(n, MaxSymbolLen) {
			s.c.droppedBadName.Add(1)
			return nil
		}
		failed = append(failed, n)
	}
	if h.Healthy != nil && !validToken(h.Provider, MaxTokenLen) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	healthy := false
	switch {
	case h.Healthy != nil:
		healthy = *h.Healthy
	case h.State == "degraded":
	case h.State == "ok" || h.State == "healthy" || h.State == "recovered":
		healthy = true
	default:
		s.c.droppedInvalid.Add(1)
		return nil
	}
	trust := ev.TrustLevel()
	reason := cleanText(h.Reason, 80)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)
	st := s.noteSource(h.Source, at)
	if st == nil {
		s.c.factsEvicted.Add(1)
		return nil
	}
	was := st.degraded()
	switch {
	case healthy && h.Healthy != nil:
		delete(st.failed, h.Provider)
	case healthy:
		st.whole, st.failed = false, nil
	case h.Healthy != nil:
		st.addFailed(h.Provider)
	case len(failed) > 0:
		for _, n := range failed {
			st.addFailed(n)
		}
	default:
		st.whole = true
	}
	if !healthy {
		st.reason, st.lastErr = reason, at
		st.consecutive++
	} else if !st.degraded() {
		st.consecutive = 0
	}
	if now := st.degraded(); now != was {
		s.record(d, at, stateChange(h.Source, was, now, trust))
	}
	return nil
}

func (st *sourceState) addFailed(name string) {
	if st.failed == nil {
		st.failed = map[string]struct{}{}
	}
	if len(st.failed) < MaxFailedNames {
		st.failed[name] = struct{}{}
	}
}

func stateChange(source string, was, now bool, trust proactive.Trust) Change {
	label := func(b bool) string {
		if b {
			return "degraded"
		}
		return "ok"
	}
	return Change{Subject: "source:" + source, Key: "state", Old: label(was), New: label(now), Trust: trust}
}

// recoverSource marks accounts (or, with none named, the whole source) readable
// again after data that proves it. The caller holds the write lock.
func (s *Store) recoverSource(d *Diff, at time.Time, source string, accounts []string, trust proactive.Trust) {
	st := s.sources[source]
	if st == nil || !st.degraded() {
		return
	}
	if len(accounts) == 0 {
		st.whole, st.failed = false, nil
	} else {
		for _, a := range accounts {
			delete(st.failed, a)
		}
	}
	if !st.degraded() {
		st.consecutive = 0
		s.record(d, at, stateChange(source, true, false, trust))
	}
}

// cleanText keeps printable ASCII, collapses runs of space and cuts to max bytes.
func cleanText(s string, max int) string {
	var b strings.Builder
	space := false
	for i := 0; i < len(s) && b.Len() < max; i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			c = ' '
		}
		if c == ' ' {
			if space || b.Len() == 0 {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}

// ---- fills ----------------------------------------------------------------

func (s *Store) applyFill(ev events.Event, at time.Time, d *Diff) error {
	o, err := decodeOrder(ev.Payload)
	if err != nil {
		return err
	}
	if o.Paper {
		// A simulated order never reached an exchange. It must not become a fill,
		// whatever kind the event carries.
		s.c.droppedPaper.Add(1)
		return nil
	}
	if !validToken(o.Provider, MaxTokenLen) || !validToken(o.Account, MaxTokenLen) || !validSymbol(o.Symbol, MaxSymbolLen) ||
		!validToken(o.Side, 16) || (o.AmountUnit != "" && !validToken(o.AmountUnit, 24)) {
		s.c.droppedBadName.Add(1)
		return nil
	}
	if (o.FilledKnown && !nonNegative(o.Filled)) || (o.AvgPriceKnown && !nonNegative(o.AvgPrice)) {
		s.c.droppedInvalid.Add(1)
		return nil
	}
	if !o.At.IsZero() && !o.At.After(at.Add(futureSkew)) {
		at = o.At
		d.At = at
	}
	trust := ev.TrustLevel()
	fv := FillView{Side: strings.ToLower(o.Side), Unit: o.AmountUnit, At: at, Trust: trust}
	if o.FilledKnown {
		fv.Filled, fv.FilledKnown = o.Filled, true
	}
	if o.AvgPriceKnown {
		fv.AvgPrice, fv.AvgPriceKnown = o.AvgPrice, true
	}
	desc := fillText(fv)
	acct := o.Provider + "/" + o.Account
	k := fillKey{acct, o.Symbol}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteSource(ev.Source.Name, at)

	fs := s.fills[k]
	if fs != nil && at.Before(fs.view.At) {
		s.c.droppedOutOfOrder.Add(1)
		return nil
	}
	c := Change{Subject: "order:" + acct + ":" + o.Symbol, Key: "fill", New: desc, Trust: trust}
	if fs == nil {
		if len(s.fills) >= MaxFills {
			gone := evictOldest(s.fills, max(MaxFills/10, 1), func(x *fillState) time.Time { return x.view.At },
				func(a, b fillKey) bool {
					if a.account != b.account {
						return a.account < b.account
					}
					return a.symbol < b.symbol
				})
			for _, g := range gone {
				s.touchRow(rowKey{kind: rkFill, a: g.account, b: g.symbol})
			}
			s.c.factsEvicted.Add(uint64(len(gone)))
		}
		fs = &fillState{}
		s.fills[k] = fs
	} else {
		c.Old = fs.desc
		c.Trust = proactive.MinTrust(fs.view.Trust, trust)
	}
	fs.view, fs.desc, fs.src = fv, desc, sourceTag(ev.Source.Name)
	s.record(d, at, c)
	s.touchRow(rowKey{kind: rkFill, a: acct, b: o.Symbol})
	return nil
}

// fillText renders a fill from its numbers, by code. The time makes two otherwise
// identical fills different changes.
func fillText(f FillView) string {
	var b strings.Builder
	b.WriteString(f.Side)
	b.WriteByte(' ')
	if f.FilledKnown {
		b.WriteString(strconv.FormatFloat(f.Filled, 'f', -1, 64))
		if f.Unit != "" {
			b.WriteByte(' ')
			b.WriteString(f.Unit)
		}
	} else {
		b.WriteString("size unknown")
	}
	if f.AvgPriceKnown {
		b.WriteString(" @ ")
		b.WriteString(strconv.FormatFloat(f.AvgPrice, 'f', -1, 64))
	}
	b.WriteString(" at ")
	b.WriteString(f.At.UTC().Format(time.RFC3339))
	return b.String()
}

// ---- clock ----------------------------------------------------------------

// applyDayStart remembers the zone the owner's day started in. It moves no
// baseline: the baseline is the first complete snapshot of a local day, taken from
// the snapshot's own time, so a day_start that arrives late, or never, or twice
// changes nothing.
func (s *Store) applyDayStart(ev events.Event) error {
	p, err := decode[DayStartPayload](ev.Payload)
	if err != nil {
		return err
	}
	if len(p.Date) != len(dateLayout) || !validSymbol(p.Zone, MaxTokenLen) {
		s.c.droppedInvalid.Add(1)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.day.Date != p.Date || s.day.Zone != p.Zone {
		s.day = dayHint{Date: p.Date, Zone: p.Zone}
		s.touchRow(rowKey{kind: rkDay})
	}
	return nil
}
