package gateway

import (
	"context"
	"database/sql"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

// processClockSetters are the process-global clock hooks: state that lives in a package,
// not in a component, so it is bound once per process (bindProcessClock), never per
// generation. A var so a test can see what each one is given.
var processClockSetters = []func(now func() time.Time){
	broker.SetClock,
	broker.GlobalLossTracker.SetClock,
	orderfeed.SetClock,
	tools.SetClock,
	func(now func() time.Time) { alertNow = now },
	func(now func() time.Time) { legacyJobs.now = now },
	func(now func() time.Time) { processNow = now },
}

// processNow is the clock of the gateway's free functions that have no deps to carry one
// (the legacy cron handlers, the cron routes): time.Now until bindProcessClock.
//
//simclock:wall the default when no clock is injected
var processNow = time.Now

// processAfter is processNow's timer: nil (the callers keep their wall-clock ticker) until
// bindProcessClock is given a simulated clock.
var processAfter func(d time.Duration) <-chan time.Time

// bindProcessClock points every process-global hook at c. gatewayCmd calls it once, after
// shared.Clock is final: under the lock after the lab is built, otherwise after
// newSharedResources. With simclock.Real() each hook reads time.Now, as it does unbound.
func bindProcessClock(c simclock.Clock) {
	if c == nil {
		c = simclock.Real()
	}
	processAfter = nil
	if c != simclock.Real() {
		processAfter = c.After
	}
	now := simclock.NowFunc(c)
	for _, set := range processClockSetters {
		set(now)
	}
}

// nowFn is the shared clock as a func() time.Time, for the hooks shaped that way.
func (d assemblyDeps) nowFn() func() time.Time { return simclock.NowFunc(d.Shared.clk()) }

// clockTick fires every d: on the wall clock a plain time.Ticker, exactly the loop it
// replaced; on a simulated clock a timer re-armed after each receive. Stop it when the
// loop ends.
type clockTick struct {
	tk *time.Ticker
	t  simclock.Timer
	d  time.Duration
}

func newClockTick(c simclock.Clock, d time.Duration) *clockTick {
	if c == nil || c == simclock.Real() {
		//simclock:wall the wall clock keeps its ticker
		return &clockTick{tk: time.NewTicker(d), d: d}
	}
	return &clockTick{t: c.NewTimer(d), d: d}
}

func (k *clockTick) C() <-chan time.Time {
	if k.tk != nil {
		return k.tk.C
	}
	return k.t.C()
}

// Rearm re-arms a simulated tick; a ticker needs nothing.
func (k *clockTick) Rearm() {
	if k.t != nil {
		k.t.Reset(k.d)
	}
}

func (k *clockTick) Stop() {
	if k.tk != nil {
		k.tk.Stop()
		return
	}
	k.t.Stop()
}

// openIntentStore opens the intent store on db with the gateway clock, so its stamps and
// expiries follow sim time under the lab. Every gateway site opens it through here.
func openIntentStore(ctx context.Context, d assemblyDeps, db *sql.DB) (*intent.Store, error) {
	st, err := intent.NewStore(ctx, db)
	if err != nil {
		return nil, err
	}
	st.SetClock(d.nowFn())
	return st, nil
}
