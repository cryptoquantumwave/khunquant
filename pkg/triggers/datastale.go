package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const (
	maxStaleSources = 8
	// staleUnreadableAfter is how many degraded reports in a row make a source
	// "unreadable" even though it keeps talking (a source that only reports failures
	// bumps its last-event time, so silence alone would call it healthy).
	staleUnreadableAfter = 3
	staleWindow          = 24 * time.Hour
	staleRecoveredWindow = time.Hour
	maxOutageCounter     = 9999
)

type staleState struct {
	S map[string]staleSrc `json:"s,omitempty"`
}

type staleSrc struct {
	Stale bool `json:"x,omitempty"`
	N     int  `json:"n"`
}

type dataStale struct {
	id  string
	p   StaleParams
	cfg PortfolioConfig
	// t0 is the time of the first evaluation of this build of the trigger. A source
	// is never called quiet for time before it, whatever the restored world model
	// says: after a restart every "last event" is as old as the downtime.
	t0 atomic.Int64
}

func staleFactory(cfg PortfolioConfig) Factory {
	return func(spec Spec) (Trigger, error) {
		p, err := decodeParams(spec.Params, DefaultStaleParams())
		if err != nil {
			return nil, err
		}
		return &dataStale{id: spec.Kind + ":" + spec.ID, p: p, cfg: cfg}, nil
	}
}

func (t *dataStale) ID() string { return t.id }

// Kinds: the events that arrive on their own rhythm and so give the trigger a chance
// to notice a source that did not. A pipeline that receives no event at all cannot be
// asked anything: total silence is outside what an event-driven evaluator can see.
func (t *dataStale) Kinds() []events.Kind {
	return []events.Kind{
		events.KindPerceptionPortfolioSnapshot, events.KindPerceptionFuturesPositions,
		events.KindPerceptionSourceHealth, events.KindPerceptionMarketTick,
		events.KindPerceptionClockSession, events.KindPerceptionClockDayStart,
	}
}

func (t *dataStale) Demands() []perception.Demand { return nil }

// Evaluate reports a source that has said nothing for factor times its expected
// period, or that keeps reporting it cannot read what it should, while its market is
// open and something depends on it. It fires once per outage (the outage number is in
// the key) and once, as Info, on recovery.
func (t *dataStale) Evaluate(_ context.Context, v worldmodel.View, _ worldmodel.Diff, st *State) ([]proactive.Candidate, error) {
	if t.cfg.StaleSources == nil {
		return nil, nil
	}
	now := v.Now()
	t.t0.CompareAndSwap(0, now.UnixNano())
	t0 := time.Unix(0, t.t0.Load())
	srcs := slices.Clone(t.cfg.StaleSources())
	if len(srcs) > maxStaleSources {
		srcs = srcs[:maxStaleSources]
	}
	slices.SortFunc(srcs, func(a, b StaleSource) int {
		switch {
		case a.Name < b.Name:
			return -1
		case a.Name > b.Name:
			return 1
		}
		return 0
	})
	var s staleState
	if len(st.Raw) > 0 && json.Unmarshal(st.Raw, &s) != nil {
		s = staleState{}
	}
	if s.S == nil {
		s.S = map[string]staleSrc{}
	}
	before := fmt.Sprint(s.S)
	var out []proactive.Candidate
	for _, src := range srcs {
		if src.ExpectedEvery <= 0 || src.Name == "" {
			continue
		}
		cur := s.S[src.Name]
		if src.Active != nil && !src.Active(v) {
			cur.Stale = false // nothing depends on it now: no outage, and nothing to announce
			s.S[src.Name] = cur
			continue
		}
		m := src.Market
		if m == "" {
			m = calendar.Crypto
		}
		if !t.cfg.open(m, now) {
			continue // a closed market is not an outage; the state stays as it was
		}
		quiet, unreadable := false, false
		silent := now.Sub(t0)
		if h, ok := v.Source(src.Name); ok {
			ref := h.LastEventAt
			if ref.Before(t0) {
				ref = t0
			}
			silent = now.Sub(ref)
		}
		limit := time.Duration(float64(src.ExpectedEvery) * t.p.Factor)
		quiet = silent > limit
		if ss, ok := v.SourceState(src.Name); ok && ss.Degraded && ss.ConsecutiveDegraded >= staleUnreadableAfter {
			unreadable = true
		}
		bad := quiet || unreadable
		switch {
		case bad && !cur.Stale:
			cur.Stale = true
			if cur.N < maxOutageCounter {
				cur.N++
			}
			out = append(out, t.staleCandidate(src, cur.N, silent, unreadable, now))
			t.cfg.stats().StaleFired.Add(1)
		case !bad && cur.Stale:
			cur.Stale = false
			out = append(out, t.recoveredCandidate(src, cur.N, now))
			t.cfg.stats().StaleRecovered.Add(1)
		}
		s.S[src.Name] = cur
	}
	if fmt.Sprint(s.S) != before {
		marshalState(st, s)
	}
	return out, nil
}

func minutes(d time.Duration) string { return fmtNumber(float64(int(d / time.Minute))) }

func (t *dataStale) staleCandidate(src StaleSource, n int, silent time.Duration, unreadable bool, at time.Time) proactive.Candidate {
	thai := t.cfg.isThai()
	reason := "quiet"
	if unreadable {
		reason = "unreadable"
	}
	var title, detail string
	switch {
	case thai && unreadable:
		title = "แหล่งข้อมูลอ่านไม่ได้: " + src.Name
		detail = "รายงานว่าอ่านข้อมูลไม่ได้ต่อเนื่อง · เงียบมา " + minutes(silent) + " นาที · ปกติทุก " + minutes(src.ExpectedEvery) + " นาที"
	case thai:
		title = "แหล่งข้อมูลเงียบผิดปกติ: " + src.Name
		detail = "ไม่มีข้อมูลมา " + minutes(silent) + " นาที · ปกติทุก " + minutes(src.ExpectedEvery) + " นาที"
	case unreadable:
		title = "Data source unreadable: " + src.Name
		detail = "It keeps reporting that it cannot read its data · silent " + minutes(silent) + " min · expected every " + minutes(src.ExpectedEvery) + " min"
	default:
		title = "Data source quiet: " + src.Name
		detail = "Nothing received for " + minutes(silent) + " min · expected every " + minutes(src.ExpectedEvery) + " min"
	}
	if thai {
		detail += "\nการแจ้งเตือนที่ใช้ข้อมูลนี้จะไม่ทำงานจนกว่าจะกลับมา"
	} else {
		detail += "\nAlerts that rely on this data are not being judged until it returns."
	}
	return proactive.Candidate{
		DedupKey: StaleKey(src.Name, n),
		Severity: proactive.SeverityWarning, Salience: 0.6, Subject: "source:" + cleanKeyToken(src.Name),
		Title: title, Detail: detail,
		Evidence: map[string]any{
			"source": src.Name, "reason": reason, "silent_seconds": int64(silent / time.Second),
			"expected_seconds": int64(src.ExpectedEvery / time.Second), "outage": n,
		},
		Trust: proactive.TrustSystem, Compose: proactive.ComposeTemplate, DedupWindow: staleWindow, At: at,
	}
}

func (t *dataStale) recoveredCandidate(src StaleSource, n int, at time.Time) proactive.Candidate {
	title := "Data source back: " + src.Name
	if t.cfg.isThai() {
		title = "แหล่งข้อมูลกลับมาแล้ว: " + src.Name
	}
	return proactive.Candidate{
		DedupKey: StaleClearKey(src.Name, n),
		Severity: proactive.SeverityInfo, Salience: 0.2, Subject: "source:" + cleanKeyToken(src.Name),
		Title:    title,
		Evidence: map[string]any{"source": src.Name, "outage": n},
		Trust:    proactive.TrustSystem, Compose: proactive.ComposeTemplate, DedupWindow: staleRecoveredWindow, At: at,
	}
}
