package deltaneutral

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

// A leg's fill poll pauses on the engine's clock between two reads. In a lab whose clock
// is manual that clock moves only after a settle passes, and the settle waits for the
// journal engine: the pause must read idle, or the advance fails "not settled: journal"
// and the poll never reads again. Here a step polls through the real pause on a manual
// Sim, and an advance (settled on the engine, as the lab's journal quiescer does) brings
// the next read.
func TestPause_ManualAdvanceSettlesWhileALegPollWaits(t *testing.T) {
	sim := simclock.NewSim(time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC))
	if err := sim.SetResolution(0); err != nil {
		t.Fatal(err)
	}
	d := &Deps{Clock: sim} // PollInterval 0: the production 1 s
	reads := make(chan struct{}, 8)
	def := journal.Definition{Kind: "test.leg_poll", Run: func(c *journal.Ctx, _ json.RawMessage) error {
		return c.Step("poll", nil, func(ctx context.Context, _ int) (any, error) {
			for i := 0; i < 3; i++ {
				reads <- struct{}{} // a read of the order that says "not filled yet"
				d.pause(ctx)
			}
			return nil, nil
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

	if _, err := eng.Start(context.Background(), "test.leg_poll", nil); err != nil {
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
		t.Fatalf("a step pausing between two reads keeps the engine busy: %v", err)
	}
	if _, err := sim.Advance(context.Background(), sim.Now().Add(time.Second), settle); err != nil {
		t.Fatalf("a manual advance over the pause: %v", err)
	}
	nextRead("second")
}
