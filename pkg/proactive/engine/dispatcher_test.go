package engine

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// recordingHandover is an outbox that remembers how it was called.
type recordingHandover struct {
	calls [][]attention.Decision
	reply []attention.Handoff
}

func (r *recordingHandover) AcceptAll(ds []attention.Decision) []attention.Handoff {
	r.calls = append(r.calls, append([]attention.Decision(nil), ds...))
	return r.reply
}

// The decisions of one event reach the outbox in ONE call, in order: a hand-over
// one decision at a time is what let the protective lane deliver the first code of a
// breach before the second arrived, which made one breach two messages.
func TestDefaultDispatcher_HandsAnEventsDecisionsOverTogether(t *testing.T) {
	rec := &recordingHandover{reply: []attention.Handoff{
		attention.HandoffQueued, attention.HandoffQueued, attention.HandoffBuffered, attention.HandoffIgnored,
	}}
	d := NewDefaultDispatcher(rec)
	ds := []attention.Decision{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	if err := d.Dispatch(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || len(rec.calls[0]) != 4 {
		t.Fatalf("the outbox was called %d times: %+v", len(rec.calls), rec.calls)
	}
	for i, id := range []string{"a", "b", "c", "d"} {
		if rec.calls[0][i].ID != id {
			t.Fatalf("decision %d is %s, want %s", i, rec.calls[0][i].ID, id)
		}
	}
	if st := d.Stats(); st.Queued != 2 || st.Buffered != 1 || st.Ignored != 1 || st.Rejected != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestDefaultDispatcher_ReportsWhatTheStoppedOutboxRefused(t *testing.T) {
	rec := &recordingHandover{reply: []attention.Handoff{attention.HandoffClosed, attention.HandoffIgnored, attention.HandoffClosed}}
	d := NewDefaultDispatcher(rec)
	err := d.Dispatch(context.Background(), make([]attention.Decision, 3))
	if err == nil {
		t.Fatal("Dispatch to a stopped outbox returned no error")
	}
	if st := d.Stats(); st.Rejected != 2 || st.Ignored != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// settlingOutbox is the real outbox as seen by a protective worker that always wins
// the race: after every call into the outbox it waits until everything handed over so
// far has been delivered, so the worker has taken whatever it was given before the
// next call can add to it. Whatever the dispatcher hands over in separate calls
// therefore reaches the phone as separate messages, every run and without a sleep.
type settlingOutbox struct {
	t      *testing.T
	ob     *attention.Outbox
	del    *fakeDeliverer
	handed int
}

func (s *settlingOutbox) settle(hs []attention.Handoff) {
	s.t.Helper()
	for _, h := range hs {
		if h == attention.HandoffQueued {
			s.handed++
		}
	}
	waitFor(s.t, "the protective worker to take what it was handed", func() bool {
		n := 0
		for _, m := range s.del.messages() {
			n += len(m.Decisions)
		}
		return n >= s.handed && s.ob.Stats().ProtectivePending == 0
	})
}

func (s *settlingOutbox) AcceptAll(ds []attention.Decision) []attention.Handoff {
	hs := s.ob.AcceptAll(ds)
	s.settle(hs)
	return hs
}

// One event with two protective Criticals (the two breach codes of a delta-neutral
// plan) is ONE message even when the protective worker is idle and wins every race
// against the dispatcher: the dispatcher must not let it see the first code alone.
func TestDefaultDispatcher_TwoCriticalsOfOneEventAreOneMessage(t *testing.T) {
	g := newGov(t, config.ProactiveModeLive, liveCfg())
	del := &fakeDeliverer{}
	ob := attention.NewOutbox(g.gov, del, 0, g.clock.Now)
	runOutbox(t, ob)

	var ds []attention.Decision
	for _, key := range []string{"margin_critical", "liquidation_distance_low"} {
		d, err := ob.Decide(context.Background(), protective(key))
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	disp := NewDefaultDispatcher(&settlingOutbox{t: t, ob: ob, del: del})
	if err := disp.Dispatch(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	msgs := del.messages()
	if len(msgs) != 1 || len(msgs[0].Decisions) != 2 {
		t.Fatalf("%d messages for one event, want one carrying both: %+v", len(msgs), msgs)
	}
	if st := disp.Stats(); st.Queued != 2 {
		t.Fatalf("stats = %+v", st)
	}
}
