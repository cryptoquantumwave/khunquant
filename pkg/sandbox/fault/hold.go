package fault

import (
	"context"
	"errors"
	"time"
)

var errClosed = errors.New("fault: injector closed")

// Hold waits d of wall time (at most MaxHold), or until ctx ends or the injector is
// closed. Every hold counts toward Idle, so a settle does not pass over a held request or
// model call. It returns nil when the time passed.
func (i *Injector) Hold(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	i.held.Add(1)
	defer i.held.Add(-1)
	t := time.NewTimer(min(d, i.maxHold)) //simclock:wall a hold is wall time by design (§5.3)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-i.stop:
		return errClosed
	}
}

// Idle is true when no hold is in progress. It changes nothing.
func (i *Injector) Idle() bool { return i.held.Load() == 0 }

// Close ends every hold now and makes later holds return at once. Rules stay.
func (i *Injector) Close() error {
	i.once.Do(func() { close(i.stop) })
	return nil
}
