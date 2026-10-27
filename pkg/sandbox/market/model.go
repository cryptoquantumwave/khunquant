// Package market is SimMarket: one deterministic price per symbol at the current time,
// served in each venue's wire format (ticker, klines) and pulled by the stateful
// simulator for its fills. Ops (set, path, shock, walk) append segments to a timeline
// per (venue, pair); Price(t) evaluates the last segment that started at or before t.
package market

import (
	"math"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Model is the price of one symbol over time. ok=false means no price.
type Model interface {
	Price(t time.Time) (float64, bool)
}

// BarModel is optional: a model that carries recorded bars (replay CSV, recordings).
type BarModel interface {
	Model
	Bars(tf time.Duration, from, to time.Time) ([]Bar, bool)
}

// Shape is how a path moves between its keyframes.
type Shape string

const (
	ShapeLinear Shape = "linear"
	ShapeStep   Shape = "step"
)

// Keyframe is a price the path passes at an offset from its start.
type Keyframe struct {
	At    time.Duration
	Price float64
}

// WalkParams configure a seeded geometric random walk. Vol and Drift are per Step.
type WalkParams struct {
	Vol, Drift float64
	Seed       uint64
	Step       time.Duration
}

type setModel struct{ p float64 }

func (m setModel) Price(time.Time) (float64, bool) { return m.p, true }

// pathModel moves through pts (sorted by At, pts[0].At == 0) from start, then holds the last.
type pathModel struct {
	start time.Time
	pts   []Keyframe
	step  bool
}

func (m *pathModel) Price(t time.Time) (float64, bool) {
	el := t.Sub(m.start)
	i := sort.Search(len(m.pts), func(i int) bool { return m.pts[i].At > el })
	switch {
	case i == 0:
		return m.pts[0].Price, true
	case i == len(m.pts):
		return m.pts[i-1].Price, true
	case m.step:
		return m.pts[i-1].Price, true
	}
	a, b := m.pts[i-1], m.pts[i]
	return a.Price + (b.Price-a.Price)*float64(el-a.At)/float64(b.At-a.At), true
}

const maxWalkSteps = 1 << 20

// walkModel is P_k = P_0 * exp(sum_{i<=k} (drift - vol^2/2 + vol*Z_i)) with Z_i drawn from
// a PCG stream seeded by (seed, i), so the same seed gives the same path whatever order
// it is read in. The prefix sums are memoised; mu is a leaf lock (Price mutates them).
type walkModel struct {
	start  time.Time
	p0     float64
	w      WalkParams
	mu     sync.Mutex
	prefix []float64
}

func (m *walkModel) Price(t time.Time) (float64, bool) {
	k := int(min(max(t.Sub(m.start), 0)/m.w.Step, maxWalkSteps))
	m.mu.Lock()
	if len(m.prefix) == 0 {
		m.prefix = append(m.prefix, 0)
	}
	for i := len(m.prefix); i <= k; i++ {
		z := rand.New(rand.NewPCG(m.w.Seed, uint64(i))).NormFloat64()
		m.prefix = append(m.prefix, m.prefix[i-1]+m.w.Drift-m.w.Vol*m.w.Vol/2+m.w.Vol*z)
	}
	s := m.prefix[k]
	m.mu.Unlock()
	p := m.p0 * math.Exp(s)
	return p, p > 0 && !math.IsInf(p, 0)
}

// kind names a model for the get op.
func kind(m Model) string {
	switch m.(type) {
	case setModel:
		return "set"
	case *pathModel:
		return "path"
	case *walkModel:
		return "walk"
	case *replayModel:
		return "replay"
	}
	return "custom"
}
