// Package perceptiontest holds fakes for code that runs on pkg/perception.
package perceptiontest

import (
	"sort"
	"sync"
	"time"
)

// Clock is a settable clock whose timers fire only when the test advances it.
// It implements perception.Clock (and pkg/proactive/engine.Clock).
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*timer
	waiters []chan struct{}
}

type timer struct {
	at time.Time
	ch chan time.Time
}

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{now: start} }

// Now returns the fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives the fake time once the clock has been
// advanced by d. A d of zero or less fires at once.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, &timer{at: c.now.Add(d), ch: ch})
	for _, w := range c.waiters {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	return ch
}

// Advance moves the clock forward by d and fires every timer that became due, in
// order of their deadlines.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
	var due []*timer
	rest := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(now) {
			due = append(due, t)
		} else {
			rest = append(rest, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	for _, t := range due {
		t.ch <- now
	}
}

// Pending is the number of timers that have not fired.
func (c *Clock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// WaitForTimers blocks until at least n timers are pending, or until the real
// time limit passes. It reports whether they are. Tests use it to know a goroutine
// has reached its wait before they advance the clock.
func (c *Clock) WaitForTimers(n int, limit time.Duration) bool {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	w := make(chan struct{}, 1)
	c.mu.Lock()
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()
	for {
		if c.Pending() >= n {
			return true
		}
		select {
		case <-w:
		case <-deadline.C:
			return c.Pending() >= n
		}
	}
}
