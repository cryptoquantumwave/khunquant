package events

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Pins the arithmetic the sandbox lab's feedback.Service.Idle relies on: every event a
// subscription received ends as exactly one of handled, failed, panicked or dropped.
// TimedOut is NOT a fifth outcome: a timed-out call is counted in TimedOut and also in
// Failed (or Handled, when the handler returned nil after the deadline), so adding it
// would count the event twice.
func TestSubscriptionStats_EveryReceivedEventEndsInOneOutcome(t *testing.T) {
	bus := NewBus()
	defer closeBus(t, bus)

	gate := make(chan struct{})
	sub, err := bus.Channel().Subscribe(context.Background(),
		SubscribeOptions{Name: "acct", Buffer: 2, Timeout: 30 * time.Millisecond},
		func(ctx context.Context, e Event) error {
			switch e.ID {
			case "gate":
				<-gate
			case "err":
				return errors.New("boom")
			case "panic":
				panic("boom")
			case "slow":
				time.Sleep(150 * time.Millisecond) // ignores its context: a timeout
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}

	pub := func(id string) { bus.Publish(context.Background(), Event{Kind: KindAgentTurnStart, ID: id}) }
	pub("gate") // held in the handler
	waitCond(t, "the gate event to be taken", func() bool { return len(sub.(*eventSubscription).ch) == 0 && sub.Stats().Received == 1 })
	for _, id := range []string{"ok", "err", "panic", "slow", "ok2", "ok3", "ok4"} {
		pub(id) // two fit the queue; the rest are dropped
	}
	st := sub.Stats()
	if st.Dropped == 0 {
		t.Fatalf("no event was dropped: %+v", st)
	}
	if st.Received <= st.Handled+st.Failed+st.Panicked+st.Dropped {
		t.Fatalf("a held handler plus queued events must read as pending: %+v", st)
	}
	close(gate)

	// Later, with the queue empty, every kind of outcome once more.
	waitCond(t, "the backlog to drain", func() bool { return len(sub.(*eventSubscription).ch) == 0 })
	for _, id := range []string{"err", "panic", "slow"} {
		pub(id)
		waitCond(t, "the event to finish", func() bool {
			s := sub.Stats()
			return s.Received == s.Handled+s.Failed+s.Panicked+s.Dropped
		})
	}

	final := sub.Stats()
	if final.Received != final.Handled+final.Failed+final.Panicked+final.Dropped {
		t.Fatalf("received %d != handled %d + failed %d + panicked %d + dropped %d", final.Received, final.Handled, final.Failed, final.Panicked, final.Dropped)
	}
	if final.TimedOut == 0 || final.Panicked == 0 || final.Failed == 0 || final.Dropped == 0 {
		t.Fatalf("the scenario did not exercise every outcome: %+v", final)
	}
	if final.Received >= final.Handled+final.Failed+final.Panicked+final.Dropped+final.TimedOut {
		t.Fatalf("TimedOut is a separate outcome (%+v): it is counted in Failed or Handled as well", final)
	}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
