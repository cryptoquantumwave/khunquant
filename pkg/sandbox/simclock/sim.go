package simclock

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Mode says what moves a Sim.
type Mode string

const (
	ModeManual   Mode = "manual"   // moves only on Advance
	ModeScaled   Mode = "scaled"   // sim = anchor + wall elapsed x scale
	ModeRealtime Mode = "realtime" // scaled with scale 1
)

// DefaultResolution is the manual-mode quantum: timers due inside one quantum fire
// together at its boundary (aligned to the Unix epoch), at most one quantum late and
// never early. Zero means exact timer-by-timer stepping.
const DefaultResolution = time.Minute

// maxSteps stops a component that re-arms a 0-length timer forever.
const maxSteps = 200_000

var (
	ErrNotManual = errors.New("simclock: advance needs manual mode")
	ErrPast      = errors.New("simclock: target is before now")
	ErrBusy      = errors.New("simclock: another advance or mode change is running")
)

// Step is one move of the clock.
type Step struct {
	At          time.Time // where it moved to (a resolution boundary, or the target)
	TimersFired int
}

// AdvanceSummary is what an Advance did.
type AdvanceSummary struct {
	From        time.Time     `json:"from"`
	Target      time.Time     `json:"target"`
	Reached     time.Time     `json:"reached"`
	Done        bool          `json:"done"` // Reached == Target
	Steps       int           `json:"steps"`
	TimersFired int           `json:"timers_fired"`
	Wall        time.Duration `json:"wall"`
}

// Sim is a clock that moves only when told (manual), or follows the wall clock scaled.
// Timers fire as it moves. It satisfies Clock.
type Sim struct {
	mu     sync.Mutex
	now    time.Time // manual: the time; driven modes: the last stepped time (Now is live)
	timers timerHeap
	seq    uint64
	mode   Mode
	scale  float64
	res    time.Duration
	onStep func(Step)

	anchorSim, anchorWall time.Time // driven modes: sim = anchorSim + elapsed x scale
	drv                   *driver

	opMu sync.Mutex // one Advance or mode change at a time
	tick time.Duration
}

// NewSim returns a manual Sim at start with DefaultResolution.
func NewSim(start time.Time) *Sim {
	return &Sim{now: start, mode: ModeManual, res: DefaultResolution, tick: driverTick}
}

// nowLocked is the sim time: live in a driven mode, never behind the last step.
func (s *Sim) nowLocked() time.Time {
	if s.mode == ModeManual {
		return s.now
	}
	live := s.anchorSim.Add(time.Duration(float64(time.Since(s.anchorWall)) * s.scale))
	if live.After(s.now) {
		return live
	}
	return s.now
}

// Now is the sim time.
func (s *Sim) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowLocked()
}

// After is time.After on sim time.
func (s *Sim) After(d time.Duration) <-chan time.Time { return s.NewTimer(d).C() }

// NewTimer is time.NewTimer on sim time. A timer with d <= 0 fires at once.
func (s *Sim) NewTimer(d time.Duration) Timer {
	t := &simTimer{s: s, ch: make(chan time.Time, 1), idx: -1}
	s.mu.Lock()
	s.armLocked(t, d)
	s.mu.Unlock()
	return t
}

func (s *Sim) armLocked(t *simTimer, d time.Duration) {
	now := s.nowLocked()
	if d <= 0 {
		t.ch <- now // the channel is empty: Stop and Reset drain it
		return
	}
	s.seq++
	t.at, t.seq = now.Add(d), s.seq
	heap.Push(&s.timers, t)
}

type simTimer struct {
	s   *Sim
	at  time.Time
	seq uint64
	idx int // position in the heap; -1 when not armed
	ch  chan time.Time
}

func (t *simTimer) C() <-chan time.Time { return t.ch }

// Stop disarms the timer and reports whether it was armed. Like Go 1.23's timers, a
// value that was sent but not received is discarded.
func (t *simTimer) Stop() bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	return t.stopLocked()
}

func (t *simTimer) stopLocked() bool {
	active := t.idx >= 0
	if active {
		heap.Remove(&t.s.timers, t.idx)
	}
	select {
	case <-t.ch:
	default:
	}
	return active
}

// Reset re-arms the timer for d from now and reports whether it was armed.
func (t *simTimer) Reset(d time.Duration) bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	active := t.stopLocked()
	t.s.armLocked(t, d)
	return active
}

type timerHeap []*simTimer

func (h timerHeap) Len() int { return len(h) }
func (h timerHeap) Less(i, j int) bool {
	if h[i].at.Equal(h[j].at) {
		return h[i].seq < h[j].seq
	}
	return h[i].at.Before(h[j].at)
}
func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].idx, h[j].idx = i, j
}
func (h *timerHeap) Push(x any) { t := x.(*simTimer); t.idx = len(*h); *h = append(*h, t) }
func (h *timerHeap) Pop() any {
	old := *h
	t := old[len(old)-1]
	old[len(old)-1], t.idx = nil, -1
	*h = old[:len(old)-1]
	return t
}

// fireLocked moves the clock to at and fires every timer due by then, in deadline order.
func (s *Sim) fireLocked(at time.Time) int {
	if at.After(s.now) {
		s.now = at
	}
	n := 0
	for len(s.timers) > 0 && !s.timers[0].at.After(at) {
		t := heap.Pop(&s.timers).(*simTimer)
		t.ch <- at // buffered, empty: armLocked and Stop/Reset keep it so
		n++
	}
	return n
}

// ceilTo rounds t up to a multiple of res since the Unix epoch.
func ceilTo(t time.Time, res time.Duration) time.Time {
	n, r := t.UnixNano(), int64(res)
	m := n % r
	if m == 0 {
		return t
	}
	if m < 0 {
		m += r
	}
	return time.Unix(0, n-m+r).In(t.Location())
}

// Advance moves manual time to target, firing timers on the way: one step per
// resolution quantum that holds a timer, then a last step to target. After every step
// it calls the OnStep hook and then settle (when non-nil), so the components can react
// before time moves on. It returns Done:false, with no error, when ctx ends first (the
// wall budget); a settle error ends it with that error. Refused outside manual mode.
func (s *Sim) Advance(ctx context.Context, target time.Time, settle func(context.Context) error) (AdvanceSummary, error) {
	if !s.opMu.TryLock() {
		return AdvanceSummary{}, ErrBusy
	}
	defer s.opMu.Unlock()
	s.mu.Lock()
	mode, res, now := s.mode, s.res, s.now
	s.mu.Unlock()
	sum := AdvanceSummary{From: now, Target: target, Reached: now}
	if mode != ModeManual {
		return sum, ErrNotManual
	}
	if target.Before(now) {
		return sum, ErrPast
	}
	return s.step(ctx, target, settle, res)
}

// step is the loop behind Advance and the driver: it has no mode check.
func (s *Sim) step(ctx context.Context, target time.Time, settle func(context.Context) error, res time.Duration) (sum AdvanceSummary, err error) {
	t0 := time.Now()
	s.mu.Lock()
	sum.From, sum.Target = s.now, target
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		sum.Reached = s.now
		s.mu.Unlock()
		sum.Done = !sum.Reached.Before(target)
		sum.Wall = time.Since(t0)
	}()
	for ctx.Err() == nil {
		s.mu.Lock()
		var st Step
		last := false
		if len(s.timers) > 0 && !s.timers[0].at.After(target) {
			if sum.Steps >= maxSteps {
				first := s.timers[0].at
				s.mu.Unlock()
				return sum, fmt.Errorf("simclock: more than %d steps in one advance: a timer due %s keeps re-arming", maxSteps, first.Format(time.RFC3339Nano))
			}
			at := s.timers[0].at
			if res > 0 {
				at = ceilTo(at, res)
				if at.After(target) {
					at = target
				}
			}
			st = Step{At: at, TimersFired: s.fireLocked(at)}
		} else if s.now.Before(target) {
			s.now, st, last = target, Step{At: target}, true
		} else {
			s.mu.Unlock()
			return sum, nil
		}
		fn := s.onStep
		s.mu.Unlock()
		sum.Steps++
		sum.TimersFired += st.TimersFired
		if fn != nil {
			fn(st)
		}
		if settle != nil {
			if err := settle(ctx); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return sum, nil // the wall budget ran out inside the settle
				}
				return sum, fmt.Errorf("simclock: settle at %s: %w", st.At.Format(time.RFC3339), err)
			}
		}
		if last {
			return sum, nil
		}
	}
	return sum, nil
}

// SetMode switches the mode. Leaving a driven mode freezes the clock at its live time;
// entering one anchors it at the current sim time.
func (s *Sim) SetMode(m Mode) error {
	if m != ModeManual && m != ModeScaled && m != ModeRealtime {
		return fmt.Errorf("simclock: unknown mode %q", m)
	}
	if !s.opMu.TryLock() {
		return ErrBusy
	}
	defer s.opMu.Unlock()
	s.stopDriver()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.nowLocked()
	s.mode = ModeManual
	switch m {
	case ModeRealtime:
		s.scale = 1
	case ModeScaled:
		if s.scale <= 0 {
			s.scale = 1
		}
	}
	if m != ModeManual {
		s.mode, s.anchorSim, s.anchorWall = m, s.now, time.Now()
		s.startDriverLocked()
	}
	return nil
}

// SetScale sets the sim seconds per wall second. In scaled mode it re-anchors at the
// current sim time; in realtime the scale is fixed at 1.
func (s *Sim) SetScale(factor float64) error {
	if !(factor > 0) || factor > 1e9 {
		return fmt.Errorf("simclock: scale %v must be > 0", factor)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == ModeRealtime {
		return errors.New("simclock: realtime runs at scale 1; switch to scaled")
	}
	if s.mode == ModeScaled {
		s.now = s.nowLocked()
		s.anchorSim, s.anchorWall = s.now, time.Now()
	}
	s.scale = factor
	return nil
}

// SetResolution sets the manual-mode quantum (0 = exact). Driven modes always step exactly.
func (s *Sim) SetResolution(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("simclock: resolution %v must be >= 0", d)
	}
	s.mu.Lock()
	s.res = d
	s.mu.Unlock()
	return nil
}

// OnStep sets the hook that runs after every step (the lab journals and saves it).
func (s *Sim) OnStep(fn func(Step)) {
	s.mu.Lock()
	s.onStep = fn
	s.mu.Unlock()
}

// Pending is the number of armed timers.
func (s *Sim) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers)
}

// NextAt is the earliest armed timer's deadline.
func (s *Sim) NextAt() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.timers) == 0 {
		return time.Time{}, false
	}
	return s.timers[0].at, true
}
