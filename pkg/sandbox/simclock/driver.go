package simclock

import (
	"context"
	"time"
)

// driverTick is how often a driven (scaled or realtime) Sim fires its due timers.
const driverTick = 100 * time.Millisecond

type driver struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startDriverLocked starts the wall ticker that moves a driven Sim. The caller holds
// s.mu and has set the mode and anchor.
func (s *Sim) startDriverLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	d := &driver{cancel: cancel, done: make(chan struct{})}
	s.drv = d
	go func() {
		defer close(d.done)
		tk := time.NewTicker(s.tick)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			// Always exact (resolution 0) and never settled; no manual-mode check.
			_, _ = s.step(ctx, s.Now(), nil, 0)
		}
	}()
}

// stopDriver stops the ticker and waits for it; it must not be called with s.mu held.
func (s *Sim) stopDriver() {
	s.mu.Lock()
	d := s.drv
	s.drv = nil
	s.mu.Unlock()
	if d != nil {
		d.cancel()
		<-d.done
	}
}

// Close stops the driver goroutine of a scaled or realtime Sim. The clock itself
// stays readable.
func (s *Sim) Close() error {
	s.stopDriver()
	return nil
}
