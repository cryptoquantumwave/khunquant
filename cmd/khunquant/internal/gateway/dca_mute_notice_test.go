package gateway

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/dca"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// A mute that skips a mandated buy tells the owner once a day per plan; the day
// survives a restart; attention off/shadow tells nobody; a run that is not muted is
// started exactly as before; and a skipped buy is never started.
func TestDispatcher_MutedSkipTellsOnceADay(t *testing.T) {
	r := newMandateRig(t)
	r.approve(r.ask())
	m, ok, err := r.store.LatestMandateForPlan(context.Background(), r.plan.ID)
	if err != nil || !ok {
		t.Fatalf("no mandate: %v", err)
	}
	planID := r.plan.ID
	// mandateT0 is Mon 2026-10-12 09:00 in Bangkok (02:00 UTC).
	slot := func(days int) int64 { return mandateT0.AddDate(0, 0, days).UnixMilli() }
	var now time.Time
	tells := &tellRec{}
	shared := &sharedResources{}
	st := &recStarter{}
	shared.setDCAStarter(st)
	shared.dcaPromoted.Store(planID, m.ID)
	arm := func(store *dca.Store) {
		shared.dcaMute.Store(&dcaMuteNotifier{store: store, tell: tells.tell, thai: func() bool { return false }, now: func() time.Time { return now }})
	}
	arm(r.store)
	dispatch := func(reason string, dueMS int64) {
		dec := attention.Decision{Reason: reason, Candidate: proactive.Candidate{
			TriggerID: "dca_due:plan-" + strconv.FormatInt(planID, 10),
			Evidence:  map[string]any{"action": "execute_dca", "plan_id": planID, "due_at_ms": dueMS, "mandate_id": m.ID},
		}}
		if err := (dcaDispatcher{next: &recNext{}, shared: shared}).Dispatch(context.Background(), []attention.Decision{dec}); err != nil {
			t.Fatal(err)
		}
	}

	now = mandateT0.Add(time.Minute)
	dispatch(attention.ReasonMuted, slot(0))
	if tells.count() != 1 {
		t.Fatalf("first muted slot: %d notices, want 1", tells.count())
	}
	n := tells.sent[0]
	if !strings.Contains(n.Body, "2026-10-12 09:00 (Asia/Bangkok)") || !strings.Contains(n.Body, "BTC/THB") || !strings.Contains(n.Body, "/unmute dca_due") {
		t.Errorf("notice lacks slot, symbol or the undo: %+v", n)
	}
	// A second muted slot the same day is counted, not sent; the same slot again changes nothing.
	now = now.Add(2 * time.Hour)
	dispatch(attention.ReasonMuted, slot(0)+3600_000)
	dispatch(attention.ReasonMuted, slot(0))
	if tells.count() != 1 {
		t.Fatalf("second muted slot the same day: %d notices, want 1", tells.count())
	}
	// A restart: a new handle on the same file and a new notifier; the day is remembered.
	reopened, err := dca.NewStore(r.cfg.WorkspacePath())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	arm(reopened)
	dispatch(attention.ReasonMuted, slot(0)+2*3600_000)
	if tells.count() != 1 {
		t.Fatalf("after a restart: %d notices, want still 1", tells.count())
	}
	// The next owner-local day: one notice, which counts the 2 slots it did not report.
	now = mandateT0.AddDate(0, 0, 1).Add(time.Minute)
	dispatch(attention.ReasonMuted, slot(1))
	if tells.count() != 2 || !strings.Contains(tells.sent[1].Body, "2 earlier slot(s)") {
		t.Fatalf("next day: %+v, want a second notice counting 2 earlier slots", tells.sent)
	}
	dispatch(attention.ReasonMuted, slot(1)+1)
	if tells.count() != 2 {
		t.Fatal("a second notice on the second day")
	}
	// The day is the mandate's zone's: 17:30 UTC is already the next day in Bangkok.
	now = mandateT0.AddDate(0, 0, 1).Add(15*time.Hour + 30*time.Minute)
	dispatch(attention.ReasonMuted, slot(2))
	if tells.count() != 3 {
		t.Fatalf("new Bangkok day at 00:30 local: %d notices, want 3", tells.count())
	}

	// Attention off or shadow is configuration, not a mute: nobody is told.
	now = now.AddDate(0, 0, 3)
	dispatch(attention.ReasonOff, slot(3))
	dispatch(attention.ReasonShadow, slot(3))
	if tells.count() != 3 {
		t.Fatalf("off/shadow told the owner: %d notices", tells.count())
	}
	if len(st.ids) != 0 {
		t.Fatalf("a skipped run was started: %v", st.ids)
	}
	// A run that is not muted starts and says nothing.
	dispatch(attention.ReasonInfoToDigest, slot(3))
	if len(st.ids) != 1 || tells.count() != 3 {
		t.Fatalf("unmuted run: started %v, %d notices", st.ids, tells.count())
	}
}
