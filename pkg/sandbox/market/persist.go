package market

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// The book outlives a gateway process (lab.Persistent): a venue's prices do not reset
// because the instance behind them was killed. Every model the ops build is saved by its
// parameters; a model the book cannot name (one only a test installs) is an error, never
// a segment silently dropped.

// StateKey is the file name of the book's saved state (<home>/lab/world/market.json).
func (b *Book) StateKey() string { return "market" }

type segDump struct {
	Start  time.Time   `json:"start"`
	Set    *float64    `json:"set,omitempty"`
	Path   *pathDump   `json:"path,omitempty"`
	Walk   *walkDump   `json:"walk,omitempty"`
	Replay *replayDump `json:"replay,omitempty"`
}

type pathDump struct {
	Start time.Time `json:"start"`
	Pts   []kfDump  `json:"pts"`
	Step  bool      `json:"step,omitempty"`
}

type kfDump struct {
	At    time.Duration `json:"at"`
	Price float64       `json:"price"`
}

type walkDump struct {
	Start time.Time  `json:"start"`
	P0    float64    `json:"p0"`
	W     WalkParams `json:"w"`
}

type replayDump struct {
	TF     time.Duration `json:"tf"`
	Offset time.Duration `json:"offset"`
	Bars   []Bar         `json:"bars"`
}

type termsDump struct {
	At       time.Time     `json:"at"`
	Rate     float64       `json:"rate"`
	Maint    float64       `json:"maint"`
	Interval time.Duration `json:"interval"`
}

type bookDump struct {
	Timelines map[string]map[string][]segDump   `json:"timelines,omitempty"` // venue -> pair
	Delisted  map[string][]string               `json:"delisted,omitempty"`
	Terms     map[string]map[string][]termsDump `json:"terms,omitempty"`
}

func dumpModel(m Model) (d segDump, err error) {
	switch v := m.(type) {
	case setModel:
		d.Set = &v.p
	case *pathModel:
		pd := &pathDump{Start: v.start, Step: v.step}
		for _, k := range v.pts {
			pd.Pts = append(pd.Pts, kfDump{k.At, k.Price})
		}
		d.Path = pd
	case *walkModel:
		d.Walk = &walkDump{Start: v.start, P0: v.p0, W: v.w}
	case *replayModel:
		d.Replay = &replayDump{TF: v.tf, Offset: v.offset, Bars: v.bars}
	default:
		err = fmt.Errorf("market: a %T model cannot be saved", m)
	}
	return d, err
}

func (d segDump) model() (Model, error) {
	switch {
	case d.Set != nil:
		return setModel{*d.Set}, nil
	case d.Path != nil && len(d.Path.Pts) > 0:
		m := &pathModel{start: d.Path.Start, step: d.Path.Step}
		for _, k := range d.Path.Pts {
			m.pts = append(m.pts, Keyframe{k.At, k.Price})
		}
		return m, nil
	case d.Walk != nil:
		return &walkModel{start: d.Walk.Start, p0: d.Walk.P0, w: d.Walk.W}, nil
	case d.Replay != nil && len(d.Replay.Bars) > 0:
		return &replayModel{tf: d.Replay.TF, offset: d.Replay.Offset, bars: d.Replay.Bars}, nil
	}
	return nil, fmt.Errorf("market: a saved segment at %s has no usable model", d.Start)
}

// MarshalState saves the timelines, the delisted pairs and the contract terms.
func (b *Book) MarshalState() ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := bookDump{Timelines: map[string]map[string][]segDump{}, Delisted: map[string][]string{}, Terms: map[string]map[string][]termsDump{}}
	for venue, ps := range b.tls {
		out.Timelines[venue] = map[string][]segDump{}
		for p, tl := range ps {
			for _, s := range tl {
				d, err := dumpModel(s.m)
				if err != nil {
					return nil, fmt.Errorf("%s on %s: %w", p, venue, err)
				}
				d.Start = s.start
				out.Timelines[venue][p.String()] = append(out.Timelines[venue][p.String()], d)
			}
		}
	}
	for venue, ps := range b.delisted {
		for p, on := range ps {
			if on {
				out.Delisted[venue] = append(out.Delisted[venue], p.String())
			}
		}
		sort.Strings(out.Delisted[venue])
	}
	for venue, ps := range b.terms {
		out.Terms[venue] = map[string][]termsDump{}
		for p, ts := range ps {
			for _, t := range ts {
				out.Terms[venue][p.String()] = append(out.Terms[venue][p.String()], termsDump{t.at, t.rate, t.maint, t.sched.Interval})
			}
		}
	}
	return json.Marshal(out)
}

// RestoreState replaces the book with the saved one. It changes nothing on an error.
func (b *Book) RestoreState(data []byte) error {
	var in bookDump
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	tls, delisted, tm := map[string]map[Pair]timeline{}, map[string]map[Pair]bool{}, map[string]map[Pair][]terms{}
	for venue, ps := range in.Timelines {
		tls[venue] = map[Pair]timeline{}
		for sym, segs := range ps {
			p, err := ParsePair(sym)
			if err != nil {
				return err
			}
			for _, d := range segs {
				m, err := d.model()
				if err != nil {
					return fmt.Errorf("%s on %s: %w", sym, venue, err)
				}
				tls[venue][p] = append(tls[venue][p], segment{d.Start, m})
			}
		}
	}
	for venue, syms := range in.Delisted {
		delisted[venue] = map[Pair]bool{}
		for _, sym := range syms {
			p, err := ParsePair(sym)
			if err != nil {
				return err
			}
			delisted[venue][p] = true
		}
	}
	for venue, ps := range in.Terms {
		tm[venue] = map[Pair][]terms{}
		for sym, ts := range ps {
			p, err := ParsePair(sym)
			if err != nil {
				return err
			}
			for _, t := range ts {
				tm[venue][p] = append(tm[venue][p], terms{at: t.At, rate: t.Rate, maint: t.Maint, sched: FundingSchedule{t.Interval}})
			}
		}
	}
	b.mu.Lock()
	b.tls, b.delisted, b.terms = tls, delisted, tm
	b.mu.Unlock()
	return nil
}
