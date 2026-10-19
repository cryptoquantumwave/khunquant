package gateway

import (
	"context"
	"database/sql"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func withLearning(cfg *config.Config) { cfg.Proactive.Learning.Enabled = true }

func hasFeedbackTable(t *testing.T, f *assemblyFixture) bool {
	t.Helper()
	db, err := proactiveDB(assemblyDeps{Cfg: f.initial, Shared: f.shared})
	if err != nil {
		t.Fatal(err)
	}
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'feedback_responses'`).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestFeedback_OffBuildsNothing(t *testing.T) {
	// A default install, and an owner with proactive on but learning off: no recorder, no
	// table, no buttons.
	for name, mut := range map[string]func(*config.Config){
		"default":            nil,
		"owner, no learning": compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive)),
		"learning, no owner": compose(enabled, withLearning), // downgraded by Effective
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mut)
			if f.services.Feedback != nil || feedbackButtons.Load() {
				t.Fatal("the recorder or its buttons exist while learning is off")
			}
			if hasFeedbackTable(t, f) {
				t.Fatal("feedback_responses was created while learning is off")
			}
			db, err := proactiveDB(assemblyDeps{Cfg: f.initial, Shared: f.shared})
			if err != nil {
				t.Fatal(err)
			}
			if hasTable(t, db, "feedback_hindsight") || hasTable(t, db, "feedback_fills") {
				t.Fatal("the hindsight tables were created while learning is off")
			}
			if f.services.Control.disp.Deps().Feedback != nil {
				t.Fatal("the control path holds a recorder while learning is off")
			}
		})
	}
}

func TestFeedback_OnBuildsAttachesAndStops(t *testing.T) {
	f := newAssemblyFixture(t, compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withLearning))
	if f.services.Feedback == nil || !feedbackButtons.Load() || !hasFeedbackTable(t, f) {
		t.Fatalf("recorder %v, buttons %v: want the recorder running", f.services.Feedback, feedbackButtons.Load())
	}
	if f.services.Control.disp.Deps().Feedback == nil {
		t.Fatal("the control path was not given the recorder")
	}
	db, err := proactiveDB(assemblyDeps{Cfg: f.initial, Shared: f.shared})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTable(t, db, "feedback_hindsight") {
		t.Fatal("feedback_hindsight was not created while learning is on")
	}

	// A reload that turns learning off takes the buttons and the recorder away.
	if err := f.reload(func(c *config.Config) { c.Proactive.Learning.Enabled = false }); err != nil {
		t.Fatal(err)
	}
	if f.services.Feedback != nil || feedbackButtons.Load() || f.services.Control.disp.Deps().Feedback != nil {
		t.Fatal("learning was switched off by the reload but the recorder remains")
	}
}

func fbMessage(mut ...func(*attention.Message)) attention.Message {
	m := attention.Message{
		InterventionID: "01HZXXXXXXXXXXXXXXXXXXXXXX", Title: "BTC moved", Body: "evidence",
		Decisions: []attention.Decision{{Candidate: proactive.Candidate{Subject: "BTC/USDT"}}},
	}
	for _, f := range mut {
		f(&m)
	}
	return m
}

func ids(as []bus.Action) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func TestFeedbackActions(t *testing.T) {
	feedbackButtons.Store(true)
	t.Cleanup(func() { feedbackButtons.Store(false) })

	got := feedbackActions(fbMessage(), 0)
	if want := []string{"useful", "not_useful", "snooze_1h", "mute"}; len(got) != 4 || ids(got)[0] != want[0] || ids(got)[1] != want[1] ||
		ids(got)[2] != want[2] || ids(got)[3] != want[3] {
		t.Fatalf("buttons = %v, want %v", ids(got), want)
	}
	for _, a := range got {
		if _, ok := channels.CallbackCode(a.ID); !ok {
			t.Errorf("%s has no callback code, so it could not be sent", a.ID)
		}
		if a.Payload != "" || a.TextCommand != "" {
			t.Errorf("%s carries a payload or a footer command: %+v", a.ID, a)
		}
		if _, err := channels.EncodeCallbackData(a, 64); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}

	// A protective candidate: the two ratings, never a snooze or a mute.
	prot := fbMessage(func(m *attention.Message) { m.Decisions[0].Candidate.Protective = true })
	if got := ids(feedbackActions(prot, 0)); len(got) != 2 || got[0] != "useful" || got[1] != "not_useful" {
		t.Errorf("protective buttons = %v", got)
	}
	// Not when the message already has buttons, not when they would not fit under
	// bus.MaxActions, not without an intervention.
	if feedbackActions(fbMessage(), 2) != nil || feedbackActions(prot, bus.MaxActions) != nil {
		t.Error("buttons added to a message that already carries some")
	}
	if feedbackActions(fbMessage(func(m *attention.Message) { m.InterventionID = "" }), 0) != nil {
		t.Error("buttons on a message with no intervention")
	}
	// Off: nothing, whatever the message.
	feedbackButtons.Store(false)
	if feedbackActions(fbMessage(), 0) != nil {
		t.Error("buttons while no recorder runs")
	}
}

// The deliverer adds the buttons to a delivered message only while a recorder runs, and
// leaves a digest's memory and reflection cards with their own buttons alone.
func TestAttentionDeliverer_FeedbackButtons(t *testing.T) {
	deliver := func(m attention.Message) []bus.Action {
		n := &capturingNotifier{}
		if ok, err := (attentionDeliverer{svc: n}).Deliver(context.Background(), m); err != nil || !ok {
			t.Fatalf("Deliver = %v, %v", ok, err)
		}
		return n.got[0].Actions
	}

	if got := deliver(fbMessage()); got != nil {
		t.Fatalf("learning off: actions = %v, want none (the message is as it was)", ids(got))
	}

	feedbackButtons.Store(true)
	t.Cleanup(func() { feedbackButtons.Store(false) })
	if got := ids(deliver(fbMessage())); len(got) != 4 {
		t.Fatalf("learning on: actions = %v", got)
	}

	b := &stubButtons{}
	setDigestButtons(b)
	t.Cleanup(func() { setDigestButtons(nil) })
	m := memoryDigestMessage()
	m.InterventionID = "01HZXXXXXXXXXXXXXXXXXXXXXX"
	if got := deliver(m); len(got) != 2 || got[0].Label != "Save AB3K" {
		t.Fatalf("a digest with a memory suggestion: actions = %+v, want its own pair only", got)
	}
}
