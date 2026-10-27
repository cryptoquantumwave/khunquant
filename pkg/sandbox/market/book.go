package market

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Options configure a Book.
type Options struct {
	// Now is the sim-time source, read on every call (the lab passes a closure over
	// lab.Lab.Now); nil is time.Now.
	Now func() time.Time
}

// Bar is one OHLC candle; V is a synthetic constant (1) that no scenario may assert on.
type Bar struct {
	Open       time.Time
	O, H, L, C float64
	V          float64
	Closed     bool
}

type segment struct {
	start time.Time
	m     Model
}

// timeline is a pair's segments in start order. A time before the first segment gets the
// first model (it extends backwards), so history is never empty once a pair has a model.
type timeline []segment

func (tl timeline) index(t time.Time) int {
	return max(sort.Search(len(tl), func(i int) bool { return tl[i].start.After(t) })-1, 0)
}

func (tl timeline) price(t time.Time) (float64, bool) { return tl[tl.index(t)].m.Price(t) }

// Book holds the models. Its lock is a leaf: the book never calls the simulator (the
// simulator calls it, holding its own lock), so the order is always sim.mu -> book.mu.
type Book struct {
	mu  sync.RWMutex
	now func() time.Time
	tls map[string]map[Pair]timeline // venue ("*" = wildcard) -> pair

	delisted map[string]map[Pair]bool    // venue ("*" = every venue) -> pair (delist.go)
	funding  map[string]map[Pair]float64 // venue ("*" = wildcard) -> rate per 8 h (render_funding.go)
}

// NewBook returns an empty book.
func NewBook(o Options) *Book {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Book{
		now: o.Now, tls: map[string]map[Pair]timeline{},
		delisted: map[string]map[Pair]bool{}, funding: map[string]map[Pair]float64{},
	}
}

// Now is the book's current time.
func (b *Book) Now() time.Time { return b.now() }

// lookup returns the timeline serving venue for p: its own, else the wildcard's.
func (b *Book) lookup(venue string, p Pair) timeline {
	if tl := b.tls[venue][p]; len(tl) > 0 {
		return tl
	}
	return b.tls[Wildcard][p]
}

// add appends m to the venue's timeline from now. A venue's first model inherits the
// wildcard's history, so bars before it stay continuous.
func (b *Book) add(venue string, p Pair, m Model) {
	if venue == "" {
		venue = Wildcard
	}
	if b.tls[venue] == nil {
		b.tls[venue] = map[Pair]timeline{}
	}
	tl := b.tls[venue][p]
	if len(tl) == 0 {
		tl = append(timeline(nil), b.tls[Wildcard][p]...)
	}
	start := b.now()
	if n := len(tl); n > 0 && start.Before(tl[n-1].start) {
		start = tl[n-1].start // the clock went back: keep the timeline ordered
	}
	b.tls[venue][p] = append(tl[:len(tl):len(tl)], segment{start, m})
}

// current is the price now, or an error naming what to do (an op never invents a start).
func (b *Book) current(venue string, p Pair) (float64, error) {
	if tl := b.lookup(venue, p); len(tl) > 0 {
		if v, ok := tl.price(b.now()); ok {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%s has no price on %s yet: set it first", p, venue)
}

func checkVenue(venue string) (string, error) {
	if venue == "" {
		venue = Wildcard
	}
	if !knownVenue(venue) {
		return "", fmt.Errorf("unknown venue %q (binance, okx or *)", venue)
	}
	return venue, nil
}

func positive(x float64) bool { return x > 0 && !math.IsInf(x, 0) }

// Set makes the price constant from now.
func (b *Book) Set(venue string, p Pair, price float64) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if !positive(price) {
		return fmt.Errorf("price %v is not a positive number", price)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.add(venue, p, setModel{price})
	return nil
}

// Path moves the price from its current value to `to` over `over`, then holds. Keyframes
// (offsets inside `over`) are passed on the way. Shape step holds each price until the
// next keyframe instead of interpolating.
func (b *Book) Path(venue string, p Pair, to float64, over time.Duration, shape Shape, kfs []Keyframe) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if !positive(to) || over < 0 || (shape != ShapeLinear && shape != ShapeStep) {
		return errors.New("path needs to > 0, over >= 0 and shape linear or step")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	from, err := b.current(venue, p)
	if err != nil {
		return err
	}
	pts := []Keyframe{{0, from}}
	for _, k := range kfs {
		if k.At <= 0 || k.At >= over || !positive(k.Price) {
			return fmt.Errorf("keyframe at %v price %v is outside (0, over) or not positive", k.At, k.Price)
		}
		pts = append(pts, k)
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].At < pts[j].At })
	pts = append(pts, Keyframe{over, to})
	b.add(venue, p, &pathModel{start: b.now(), pts: pts, step: shape == ShapeStep})
	return nil
}

// Shock gaps the price by pct percent at now; history keeps the old price.
func (b *Book) Shock(venue string, p Pair, pct float64) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, err := b.current(venue, p)
	if err != nil {
		return err
	}
	if next := cur * (1 + pct/100); next > 0 && !math.IsInf(next, 0) {
		b.add(venue, p, setModel{next})
		return nil
	}
	return fmt.Errorf("a shock of %v%% would not leave a positive price", pct)
}

// Walk starts a seeded geometric random walk from the current price.
func (b *Book) Walk(venue string, p Pair, w WalkParams) error {
	venue, err := checkVenue(venue)
	if err != nil {
		return err
	}
	if w.Step <= 0 || w.Vol < 0 || math.IsNaN(w.Vol) || math.IsNaN(w.Drift) {
		return errors.New("walk needs step > 0 and vol >= 0")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, err := b.current(venue, p)
	if err != nil {
		return err
	}
	b.add(venue, p, &walkModel{start: b.now(), p0: cur, w: w})
	return nil
}

// SetModel sets a model for a symbol on every venue without one of its own.
func (b *Book) SetModel(symbol string, m Model) error { return b.SetVenueModel(Wildcard, symbol, m) }

// SetVenueModel sets a model for one venue.
func (b *Book) SetVenueModel(venue, symbol string, m Model) error {
	p, err := ParsePair(symbol)
	if err != nil {
		return err
	}
	if venue, err = checkVenue(venue); err != nil {
		return err
	}
	if m == nil {
		return errors.New("nil model")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.add(venue, p, m)
	return nil
}

// Price is the price of p on venue at t.
func (b *Book) Price(venue string, p Pair, t time.Time) (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if tl := b.lookup(venue, p); len(tl) > 0 {
		return tl.price(t)
	}
	return 0, false
}

// MarkPrice is the simulator's PriceSource: the price now of a venue's wire symbol.
func (b *Book) MarkPrice(venue, wireSymbol string) (float64, bool) {
	p, _, ok := ParseWire(venue, wireSymbol)
	if !ok {
		return 0, false
	}
	return b.Price(venue, p, b.now())
}

// Entry is one row of Snapshot.
type Entry struct {
	Venue  string  `json:"venue"`
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	Model  string  `json:"model"`

	Delisted bool `json:"delisted"`
}

// Snapshot lists the price now of every modelled pair on the venues (all when empty),
// optionally for one pair only.
func (b *Book) Snapshot(venue string, only *Pair) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []Entry
	now := b.now()
	for v, ps := range b.tls {
		if venue != "" && v != venue {
			continue
		}
		for p, tl := range ps {
			if only != nil && *only != p {
				continue
			}
			if px, ok := tl.price(now); ok {
				out = append(out, Entry{v, p.String(), px, kind(tl[tl.index(now)].m), b.delistedLocked(v, p)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Venue+out[i].Symbol < out[j].Venue+out[j].Symbol
	})
	return out
}
