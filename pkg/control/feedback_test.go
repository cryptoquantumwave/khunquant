package control_test

import (
	"context"
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/channels"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

const (
	thumbsUp   = "\U0001F44D"
	thumbsDown = "\U0001F44E"
	heart      = "❤"
)

// withFeedback gives the rig a real recorder over a fresh database and an alert "900"
// (message id) that is intervention I1.
func withFeedback(t *testing.T, r *rig) (*feedback.Service, *sql.DB) {
	t.Helper()
	db := sqlitextest.Open(t)
	fb, err := feedback.New(context.Background(), db, feedback.Options{})
	if err != nil {
		t.Fatal(err)
	}
	deps := r.d.Deps()
	deps.Feedback = fb
	r.d.SetDeps(deps)
	alertAt("900", "I1", "price:btc", "drawdown:portfolio")(r)
	return fb, db
}

func tap(channel, chat, sender, action, onMessage string) channels.ControlInput {
	return channels.ControlInput{
		Channel: channel, ChatID: chat, MessageID: onMessage, SenderID: channel + ":" + sender, PeerKind: "direct",
		Sender:           bus.SenderInfo{Platform: channel, PlatformID: sender, CanonicalID: channel + ":" + sender},
		ReplyToMessageID: onMessage, Callback: &channels.ControlCallback{Action: action, CallbackID: "cb"},
	}
}

type row struct{ kind, value, source string }

func rows(t *testing.T, fb *feedback.Service, id string) []row {
	t.Helper()
	got, err := fb.Responses(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []row
	for _, g := range got {
		out = append(out, row{g.Kind, g.Value, g.Source})
	}
	return out
}

func TestRecordButton(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)
	ctx := context.Background()

	for _, tc := range []struct {
		action string
		reply  string
		want   row
	}{
		{"useful", "Noted as useful.", row{"useful", "", "telegram:button"}},
		{"not_useful", "Noted as not useful.", row{"not_useful", "", "telegram:button"}},
		{"snooze_1h", "Snoozed for 1h", row{"snooze", "1h", "telegram:button"}},
		{"mute", "Muted this alert for 30 days", row{"mute", "30d", "telegram:button"}},
	} {
		rep, ok := r.d.TryHandle(ctx, tap("telegram", ownerChat, ownerUser, tc.action, "900"))
		if !ok || !strings.Contains(rep.Text, tc.reply) {
			t.Fatalf("%s: %+v, %v", tc.action, rep, ok)
		}
		if strings.Contains(rep.Text, "not stored") {
			t.Errorf("%s: the reply still says nothing is stored: %q", tc.action, rep.Text)
		}
		// A double tap on the same button is one response.
		if _, ok := r.d.TryHandle(ctx, tap("telegram", ownerChat, ownerUser, tc.action, "900")); !ok {
			t.Fatalf("%s: second tap not handled", tc.action)
		}
	}
	want := []row{
		{"useful", "", "telegram:button"}, {"not_useful", "", "telegram:button"},
		{"snooze", "1h", "telegram:button"}, {"mute", "30d", "telegram:button"},
	}
	got := rows(t, fb, "I1")
	if len(got) != 4 {
		t.Fatalf("rows = %v, want the four taps once each", got)
	}
	seen := map[row]bool{}
	for _, g := range got {
		seen[g] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("missing %v in %v", w, got)
		}
	}
	// The snooze and the mute are real: both silence exactly the alert's own keys.
	m := r.att.muted()
	for _, k := range []string{"exact:price:btc", "exact:drawdown:portfolio"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%s was not muted: %v", k, m)
		}
	}
}

func TestRecordButton_NotFromANonOwner(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)
	ctx := context.Background()

	for name, in := range map[string]channels.ControlInput{
		"allow-listed stranger": tap("telegram", otherUser, otherUser, "useful", "900"),
		"owner in a group": func() channels.ControlInput {
			in := tap("telegram", "-100777", ownerUser, "useful", "900")
			in.PeerKind = "group"
			return in
		}(),
	} {
		rep, ok := r.d.TryHandle(ctx, in)
		if !ok || strings.HasPrefix(rep.Text, "Noted") {
			t.Fatalf("%s: %+v, %v", name, rep, ok)
		}
	}
	if got := rows(t, fb, "I1"); len(got) != 0 {
		t.Fatalf("a non-owner's taps recorded %v", got)
	}
	if len(r.att.muted()) != 0 {
		t.Fatalf("a non-owner's taps muted %v", r.att.muted())
	}
}

func TestUsefulAndNotUsefulCommandsRecord(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)

	r.ch.say(ownerUser, ownerChat, "/useful", replyMeta("900"))
	r.ch.say(ownerUser, ownerChat, "/notuseful", replyMeta("900"))
	reps := r.ch.replies()
	if len(reps) != 2 || reps[0].Content != "Noted as useful." || reps[1].Content != "Noted as not useful." {
		t.Fatalf("replies = %+v", reps)
	}
	got := rows(t, fb, "I1")
	if len(got) != 2 || got[0].source != "telegram:command" {
		t.Fatalf("rows = %v, want useful and not_useful from the command path", got)
	}
	// A stranger's /useful is refused and records nothing.
	r.ch.say(otherUser, otherUser, "/useful", replyMeta("900"))
	if len(rows(t, fb, "I1")) != 2 {
		t.Fatal("a non-owner's /useful recorded")
	}
	// Not an alert: nothing.
	r.ch.say(ownerUser, ownerChat, "/useful", replyMeta("777"))
	if len(rows(t, fb, "I1")) != 2 {
		t.Fatal("/useful on a message that is not an alert recorded")
	}
}

func TestLearningOff_RepliesAreUnchangedAndNothingIsRecorded(t *testing.T) {
	r := newRig(t, "telegram", nil)
	alertAt("900", "I1", "price:btc")(r)
	r.ch.say(ownerUser, ownerChat, "/useful", replyMeta("900"))
	mustContain(t, "reply", r.ch.onlyReply(t).Content, "Noted as useful", "only written to the log")

	// No recorder: a reaction and a reply are inert.
	r.d.ObserveReaction(context.Background(), tap("telegram", ownerChat, ownerUser, "", "900"), thumbsUp)
	r.ch.say(ownerUser, ownerChat, "yes thanks", replyMeta("900"))
	if got := r.inbound(); len(got) != 1 || got[0].Content != "yes thanks" {
		t.Fatalf("inbound = %+v", got)
	}
}

func TestReactionMapsToIntervention(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)
	ctx := context.Background()
	react := func(sender, chat, msg, emoji string) {
		in := tap("telegram", chat, sender, "", msg)
		in.Callback = nil
		r.d.ObserveReaction(ctx, in, emoji)
	}

	react(ownerUser, ownerChat, "900", thumbsUp)
	react(ownerUser, ownerChat, "900", thumbsUp) // a replayed update
	react(ownerUser, ownerChat, "900", thumbsDown)
	react(ownerUser, ownerChat, "900", heart)      // any other emoji: ignored
	react(ownerUser, ownerChat, "777", thumbsUp)   // not an alert
	react(otherUser, otherUser, "900", thumbsDown) // not the owner
	react(ownerUser, "-100777", "900", thumbsUp)   // the owner, but not in the owner chat

	want := []row{{"useful", "", "telegram:reaction"}, {"not_useful", "", "telegram:reaction"}}
	got := rows(t, fb, "I1")
	if len(got) != 2 || !reflect.DeepEqual(map[row]bool{got[0]: true, got[1]: true}, map[row]bool{want[0]: true, want[1]: true}) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestReplyClassifiedAndStillReachesAgent(t *testing.T) {
	const marker = "ZZMARKERZZ"
	// The same message through a dispatcher with no recorder is the golden: whatever the
	// recorder does, the agent gets this and nothing else.
	golden := newRig(t, "telegram", nil)
	golden.ch.say(ownerUser, ownerChat, marker+" yes please", replyMeta("900"))
	want := golden.inbound()
	if len(want) != 1 {
		t.Fatalf("golden inbound = %+v", want)
	}

	r := newRig(t, "telegram", nil)
	fb, db := withFeedback(t, r)
	r.ch.say(ownerUser, ownerChat, marker+" yes please", replyMeta("900"))
	fb.Wait()

	got := r.inbound()
	if len(got) != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("inbound = %+v, want exactly %+v", got, want)
	}
	if rs := r.ch.replies(); len(rs) != 0 {
		t.Fatalf("a reply to an alert was answered by the control path: %+v", rs)
	}
	rr := rows(t, fb, "I1")
	if len(rr) != 1 || rr[0] != (row{"reply", "affirmative", "telegram:reply"}) {
		t.Fatalf("rows = %v, want one affirmative reply", rr)
	}

	// The text is nowhere in the database.
	var all sql.NullString
	if err := db.QueryRow(`SELECT group_concat(id || '|' || intervention_id || '|' || kind || '|' || value || '|' || source, ';')
		FROM feedback_responses`).Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(all.String), strings.ToLower(marker)) || strings.Contains(all.String, "please") {
		t.Fatalf("the reply text reached the database: %q", all.String)
	}

	// Other classes, in Thai too.
	n := 0
	for text, class := range map[string]string{"no, stop": "negative", "why did it move?": "question", "ขอบคุณ": "affirmative", "hmm": "other"} {
		n++
		r.ch.HandleMessage(context.Background(), bus.Peer{Kind: "direct", ID: ownerUser}, "r"+strconv.Itoa(n), ownerUser, ownerChat,
			text, nil, replyMeta("900"), bus.SenderInfo{Platform: "telegram", PlatformID: ownerUser, CanonicalID: "telegram:" + ownerUser})
		fb.Wait()
		r.inbound()
		found := false
		for _, g := range rows(t, fb, "I1") {
			found = found || (g.kind == "reply" && g.value == class)
		}
		if !found {
			t.Errorf("%q: no reply row of class %s in %v", text, class, rows(t, fb, "I1"))
		}
	}
}

func TestReply_NotFromANonOwnerAndNotToAnAlert(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)

	r.ch.say(otherUser, otherUser, "yes", replyMeta("900"))                               // a stranger replies to the alert
	r.ch.sayAs("group", ownerUser, "-100777", "yes", replyMeta("900"))                    // the owner, in a group
	r.ch.say(ownerUser, ownerChat, "yes", replyMeta("777"))                               // a reply to something else
	r.ch.say(ownerUser, ownerChat, "yes", nil)                                            // not a reply
	r.ch.say(ownerUser, ownerChat, "yes", map[string]string{"control_forwarded": "true"}) // forwarded
	fb.Wait()
	if got := rows(t, fb, "I1"); len(got) != 0 {
		t.Fatalf("recorded %v", got)
	}
	if got := r.inbound(); len(got) != 5 {
		t.Fatalf("%d messages reached the agent, want all 5", len(got))
	}
}

func TestWebOwner_ResponsesRecordWithSourceWeb(t *testing.T) {
	r, _ := webRig(t, true, nil)
	fb, _ := withFeedback(t, r)
	ctx := context.Background()
	web := func(authenticated bool) bus.SenderInfo {
		return bus.SenderInfo{Platform: "pico", PlatformID: "pico-user", CanonicalID: "pico:pico-user", WebAuthenticated: authenticated}
	}
	in := func(authenticated bool) channels.ControlInput {
		return channels.ControlInput{
			Channel: "pico", ChatID: "pico:s1", MessageID: "900", ReplyToMessageID: "900", SenderID: "pico:pico-user",
			PeerKind: "direct", Sender: web(authenticated),
		}
	}

	tapIn := in(true)
	tapIn.Callback = &channels.ControlCallback{Action: "useful"}
	if rep, ok := r.d.TryHandle(ctx, tapIn); !ok || !strings.HasPrefix(rep.Text, "Noted") {
		t.Fatalf("web tap: %+v, %v", rep, ok)
	}
	r.d.ObserveReaction(ctx, in(true), thumbsDown)
	replyIn := in(true)
	replyIn.Text = "no"
	r.d.ObserveReply(ctx, replyIn)
	fb.Wait()

	got := map[row]bool{}
	for _, g := range rows(t, fb, "I1") {
		got[g] = true
	}
	for _, w := range []row{{"useful", "", "web:button"}, {"not_useful", "", "web:reaction"}, {"reply", "negative", "web:reply"}} {
		if !got[w] {
			t.Errorf("missing %v in %v", w, got)
		}
	}

	// The same from a connection that did not authenticate records nothing.
	before := len(got)
	badTap := in(false)
	badTap.Callback = &channels.ControlCallback{Action: "not_useful"}
	r.d.TryHandle(ctx, badTap)
	r.d.ObserveReaction(ctx, in(false), thumbsUp)
	badReply := in(false)
	badReply.Text = "yes"
	r.d.ObserveReply(ctx, badReply)
	fb.Wait()
	if n := len(rows(t, fb, "I1")); n != before {
		t.Fatalf("an unauthenticated web connection recorded: %d rows, was %d", n, before)
	}
}

func TestSnoozeCommandRecordsWithItsLength(t *testing.T) {
	r := newRig(t, "telegram", nil)
	fb, _ := withFeedback(t, r)
	r.ch.say(ownerUser, ownerChat, "/snooze 4h", replyMeta("900"))
	got := rows(t, fb, "I1")
	if len(got) != 1 || got[0] != (row{"snooze", "4h", "telegram:command"}) {
		t.Fatalf("rows = %v", got)
	}
	if time.Until(r.att.muted()["exact:price:btc"]) < 3*time.Hour {
		t.Fatalf("mutes = %v", r.att.muted())
	}
}
