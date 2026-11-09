package dca

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/quiesce"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/simclock"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// countingFetch is fakeEx with every read of an order reported on reads.
type countingFetch struct {
	*fakeEx
	reads chan struct{}
}

func (c countingFetch) Fetch(ctx context.Context, p *Plan, orderID string) (Fill, error) {
	c.reads <- struct{}{}
	return c.fakeEx.Fetch(ctx, p, orderID)
}

// The verify step of a mandated buy polls the order on the engine's clock until it is
// done. In a lab whose clock is manual that clock moves only after a settle passes, and
// the settle waits for the journal engine: the wait between two reads must read idle, or
// the advance fails "not settled: journal" and the order is never read again (an
// accept_no_fill fault on the simulator is enough). Here the real verify runs in a step
// on a manual Sim, and an advance (settled on the engine, as the lab's journal quiescer
// does) brings the next read.
func TestVerify_ManualAdvanceSettlesWhileAFillPollWaits(t *testing.T) {
	w := newXWorld(t, true, monday)
	w.ex.fetch = &Fill{OrderID: "ord-1", Status: "open", Known: true} // resting: not done
	sim := simclock.NewSim(monday)
	if err := sim.SetResolution(0); err != nil {
		t.Fatal(err)
	}
	reads := make(chan struct{}, 2*verifyPolls)
	d := &Deps{Store: w.openStore(), Exchange: countingFetch{fakeEx: w.ex, reads: reads}, Clock: sim} // PollInterval 0: the production 1 s
	def := journal.Definition{Kind: "test.verify", Run: func(c *journal.Ctx, _ json.RawMessage) error {
		return c.Step("verify", nil, func(ctx context.Context, _ int) (any, error) {
			return d.verify(ctx, ExecInput{PlanID: w.planID}, Fill{OrderID: "ord-1"}), nil
		})
	}}
	eng, err := journal.New(sqlitextest.Open(t), []journal.Definition{def}, sim, journal.WithShutdownWait(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	reg := quiesce.NewRegistry()
	reg.Register(quiesce.Func("journal", eng.Idle))
	settle := func(ctx context.Context) error { return reg.Settle(ctx, 2*time.Second) }

	if _, err := eng.Start(context.Background(), "test.verify", nil); err != nil {
		t.Fatal(err)
	}
	nextRead := func(what string) {
		t.Helper()
		select {
		case <-reads:
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s read of the order", what)
		}
	}
	nextRead("first")
	if err := settle(context.Background()); err != nil {
		t.Fatalf("a verify step waiting between two reads keeps the engine busy: %v", err)
	}
	if _, err := sim.Advance(context.Background(), sim.Now().Add(time.Second), settle); err != nil {
		t.Fatalf("a manual advance over the wait: %v", err)
	}
	nextRead("second")
}
