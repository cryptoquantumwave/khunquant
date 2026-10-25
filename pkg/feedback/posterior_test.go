package feedback

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

var bangkok = func() *time.Location {
	l, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic(err)
	}
	return l
}()

// bkkFixture is a fixture whose posteriors are filed under Bangkok hour buckets, the zone
// of the default profile that attention reads. The clock (12:00 UTC) is 19:00 there:
// bucket 4.
func bkkFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.fb.post.zone = func(context.Context) *time.Location { return bangkok }
	// A budget that the tests below do not run into.
	ps, err := intent.NewStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	prof := intent.DefaultProfile()
	prof.NotificationBudgetPerDay = intent.MaxBudgetPerDay
	if _, err := ps.PutProfile(context.Background(), prof, "test", proactive.TrustUserAuthored); err != nil {
		t.Fatal(err)
	}
	return f
}

const driftKind = "allocation_drift"
const driftBucket = 4

func asNotice(c *proactive.Candidate) {
	c.Severity = proactive.SeverityNotice
	c.TriggerID = driftKind + ":1"
}

func (f *fixture) respond(id string, k Kind, event string) {
	f.t.Helper()
	if _, err := f.fb.Record(context.Background(), Response{InterventionID: id, Kind: k, Source: "telegram:button", Event: event}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) posterior() Posterior {
	f.t.Helper()
	p, err := f.fb.Posteriors().Posterior(context.Background(), driftKind, driftBucket)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPosterior_UpdatesAndDecay(t *testing.T) {
	f := bkkFixture(t)
	if p := f.posterior(); p.Alpha != 2 || p.Beta != 2 || p.Mean() != 0.5 || p.Responses != 0 {
		t.Fatalf("prior = %+v", p)
	}

	for i, tc := range []struct {
		kind   Kind
		da, db float64
	}{
		{KindUseful, 1, 0}, {KindActedOn, 1, 0}, {KindApproved, 1, 0},
		{KindNotUseful, 0, 1}, {KindMute, 0, 1}, {KindRejected, 0, 1},
		{KindSnooze, 0, 0.5}, {KindNone, 0, 0.2},
		{KindReply, 0, 0}, {KindReaction, 0, 0}, {KindExpired, 0, 0},
	} {
		id, _ := f.deliver("BTC/USDT", fmt.Sprintf("allocation_drift:k%d", i), asNotice)
		before := f.posterior()
		f.respond(id, tc.kind, "e")
		after := f.posterior()
		if !near(after.Alpha-before.Alpha, tc.da) || !near(after.Beta-before.Beta, tc.db) {
			t.Errorf("%s: alpha %+v beta %+v, want %+v %+v", tc.kind, after.Alpha-before.Alpha, after.Beta-before.Beta, tc.da, tc.db)
		}
		wantN := before.Responses
		if tc.da != 0 || tc.db != 0 {
			wantN++
		}
		if after.Responses != wantN {
			t.Errorf("%s: responses %d, want %d", tc.kind, after.Responses, wantN)
		}
	}
	// 3 positives, 3 negatives, a snooze and a none: alpha 5, beta 5.7.
	p := f.posterior()
	if !near(p.Alpha, 5) || !near(p.Beta, 5.7) {
		t.Fatalf("posterior = %+v, want alpha 5 beta 5.7", p)
	}

	// Decay toward the prior with a 30-day half-life, from the injected clock.
	f.clock.Add(HalfLife)
	d := f.posterior()
	if !near(d.Alpha, 2+(5.0-2)/2) || !near(d.Beta, 2+(5.7-2)/2) {
		t.Errorf("after one half-life = %+v", d)
	}
	f.clock.Add(2 * HalfLife)
	d = f.posterior()
	if !near(d.Alpha, 2+(5.0-2)/8.0) || !near(d.Beta, 2+(5.7-2)/8.0) {
		t.Errorf("after three half-lives = %+v", d)
	}
	// A new response decays what is there first, then adds.
	id, _ := f.deliver("BTC/USDT", "allocation_drift:late", asNotice)
	f.respond(id, KindUseful, "late")
	if got := f.posterior(); !near(got.Alpha, 2+(5.0-2)/8.0+1) {
		t.Errorf("alpha after a late response = %v", got.Alpha)
	}
}

func TestPosterior_UpdateIsIdempotent(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	id, _ := f.deliver("BTC/USDT", "allocation_drift:a", asNotice)
	f.respond(id, KindNotUseful, "e1")
	want := f.posterior()
	for i := 0; i < 3; i++ {
		if err := f.fb.Posteriors().Update(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	// The same event recorded again is the same row.
	f.respond(id, KindNotUseful, "e1")
	if got := f.posterior(); got != want {
		t.Errorf("posterior moved on a replay: %+v -> %+v", want, got)
	}
	// A different event is a second response and counts.
	f.respond(id, KindNotUseful, "e2")
	if got := f.posterior(); !near(got.Beta, want.Beta+1) {
		t.Errorf("a second response did not count: %+v", got)
	}
	// Concurrent updates apply each response once.
	f.respond(id, KindMute, "e3")
	want = f.posterior()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.fb.Posteriors().Update(ctx, id) }()
	}
	wg.Wait()
	if got := f.posterior(); got != want {
		t.Errorf("concurrent updates moved the posterior: %+v -> %+v", want, got)
	}
	var markers int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_posterior_applied`).Scan(&markers); err != nil || markers != 3 {
		t.Errorf("markers = %d, %v; want 3", markers, err)
	}
	if err := f.fb.Posteriors().Update(ctx, " "); err == nil {
		t.Error("Update accepted an empty intervention")
	}
}

// Updates come from messages that interrupted the owner; a digest teaches nothing.
func TestPosterior_OnlyInterruptingMessagesCount(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	// An Info candidate goes to the digest; a response to an intervention that has no
	// notify or escalate decision moves nothing.
	d, err := f.ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{
		TriggerID: driftKind + ":1", DedupKey: "allocation_drift:i", Severity: proactive.SeverityInfo, Subject: "x", Title: "t",
	}})
	if err != nil || d.Disposition != proactive.DispDigest {
		t.Fatalf("%v %s", err, d.Disposition)
	}
	f.respond(d.ID, KindNotUseful, "e")
	f.respond("no-such-intervention", KindNotUseful, "e")
	if p := f.posterior(); p.Responses != 0 || p.Beta != 2 {
		t.Errorf("posterior moved on a digest: %+v", p)
	}
	// ...but the response was recorded and marked, so nothing is replayed later.
	if f.count() != 2 {
		t.Errorf("responses = %d", f.count())
	}
}

func TestPosterior_ReadThroughTheCallersTransaction(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	id, _ := f.deliver("BTC/USDT", "allocation_drift:a", asNotice)
	f.respond(id, KindNotUseful, "e1")
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	mean, n, err := f.fb.Posteriors().Acceptance(ctx, tx, driftKind, driftBucket, f.clock.Now())
	if err != nil || n != 1 || !near(mean, 2.0/5) {
		t.Errorf("Acceptance = %v, %d, %v", mean, n, err)
	}
	// A kind nobody has answered about is the prior.
	mean, n, err = f.fb.Posteriors().Acceptance(ctx, tx, "drawdown", 0, f.clock.Now())
	if err != nil || n != 0 || mean != 0.5 {
		t.Errorf("unknown = %v, %d, %v", mean, n, err)
	}
	var nilStore *Store
	if mean, n, err := nilStore.Acceptance(ctx, tx, "x", 0, time.Now()); mean != 0.5 || n != 0 || err != nil {
		t.Error("a nil store must answer with the prior")
	}
}

func TestPosterior_PublishesTheRoundedMean(t *testing.T) {
	f := bkkFixture(t)
	_, ch, err := f.bus.Channel().OfKind(events.KindFeedbackPosterior).SubscribeChan(context.Background(), events.SubscribeOptions{Name: "t"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := f.deliver("BTC/USDT", "allocation_drift:a", asNotice)
	f.respond(id, KindNotUseful, "e1") // 2/5 = 0.4
	f.respond(id, KindNotUseful, "e2") // 2/6 = 0.333...
	for _, want := range []float64{0.4, 0.33} {
		select {
		case ev := <-ch:
			p := ev.Payload.(map[string]any)
			if p["kind"] != driftKind || p["bucket"] != driftBucket || p["mean"] != want || len(p) != 3 {
				t.Errorf("payload = %v, want mean %v", p, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("feedback.posterior was not published")
		}
	}
}

// A grouped message teaches each of its kinds once.
func TestPosterior_GroupedMessageFilesEachKind(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	for _, key := range []string{"allocation_drift:g1", "drawdown:portfolio:g2"} {
		kind := attention.KindOf("", key)
		if _, err := f.ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{
			TriggerID: kind + ":1", DedupKey: key, Severity: proactive.SeverityNotice, Subject: key, Title: key,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.ob.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sent := f.sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("%d messages, want one group", len(sent))
	}
	f.respond(sent[0].InterventionID, KindNotUseful, "e")
	for _, k := range []string{"allocation_drift", "drawdown"} {
		if p, _ := f.fb.Posteriors().Posterior(ctx, k, driftBucket); !near(p.Beta, 3) || p.Responses != 1 {
			t.Errorf("%s = %+v", k, p)
		}
	}
}

// Markers are dropped with the responses they mark, never before.
func TestPosterior_MarkersOutliveTheirResponses(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	id, _ := f.deliver("BTC/USDT", "allocation_drift:a", asNotice)
	f.respond(id, KindNotUseful, "e1")
	if err := dropOrphanMarkers(ctx, f.db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_posterior_applied`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a live response lost its marker: %d, %v", n, err)
	}
	f.clock.Add(Retention + time.Hour)
	if _, err := f.fb.Housekeep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM feedback_posterior_applied`).Scan(&n); err != nil || n != 0 {
		t.Errorf("markers after the response was pruned: %d, %v", n, err)
	}
}

// The acceptance criterion: an owner who rejects every allocation_drift Notice drives the
// posterior mean under 0.2, after which further items go to the digest; and the user
// is heard again once the evidence has faded.
func TestAcceptance_RejectingEveryDriftNoticeEndsInTheDigest(t *testing.T) {
	f := bkkFixture(t)
	ctx := context.Background()
	f.ob.SetLearning(attention.Learning{Enabled: true, Adapt: true, HoldoutRate: 0, Source: f.fb.Posteriors(), Rand: attention.SeededRand(1)})

	submit := func(i int) (proactive.Disposition, string) {
		t.Helper()
		d, err := f.ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{
			TriggerID: driftKind + ":1", DedupKey: fmt.Sprintf("allocation_drift:equity:over%d", i),
			Severity: proactive.SeverityNotice, Subject: "equity", Title: fmt.Sprintf("equity is over target (%d)", i),
		}})
		if err != nil {
			t.Fatal(err)
		}
		if d.Disposition != proactive.DispNotify {
			return d.Disposition, d.Reason
		}
		if err := f.ob.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		sent := f.sender.Sent()
		f.respond(sent[len(sent)-1].InterventionID, KindNotUseful, fmt.Sprintf("tap%d", i))
		f.clock.Add(2 * time.Minute) // the same hour bucket throughout
		return d.Disposition, d.Reason
	}

	interactions := 0
	var means []float64
	for i := 0; i < 20; i++ {
		disp, reason := submit(i)
		if disp != proactive.DispNotify {
			if disp != proactive.DispDigest || reason != attention.ReasonLowAcceptance {
				t.Fatalf("item %d: %s/%s", i, disp, reason)
			}
			break
		}
		interactions++
		means = append(means, f.posterior().Mean())
	}
	// Mean after n rejections is 2/(4+n): 0.2 after six (not below), 0.18 after seven.
	if interactions != 7 {
		t.Fatalf("the digest began after %d rejections (means %v), want 7", interactions, means)
	}
	if last := means[len(means)-1]; last >= 0.2 || means[len(means)-2] < 0.2 {
		t.Errorf("means = %v: the crossing is not at the seventh", means)
	}
	// Further items go to the digest, and nothing new is learned from them.
	for i := 100; i < 104; i++ {
		if disp, reason := submit(i); disp != proactive.DispDigest || reason != attention.ReasonLowAcceptance {
			t.Fatalf("later item %d: %s/%s", i, disp, reason)
		}
	}
	if p := f.posterior(); p.Responses != 7 {
		t.Errorf("responses = %d after demotion, want 7", p.Responses)
	}
	// Other kinds are unaffected, and so is a Warning of this kind.
	d, err := f.ob.Submit(ctx, attention.Item{Candidate: proactive.Candidate{
		TriggerID: driftKind + ":1", DedupKey: "allocation_drift:equity:warn", Severity: proactive.SeverityWarning, Subject: "equity", Title: "w",
	}})
	if err != nil || d.Disposition != proactive.DispNotify {
		t.Errorf("warning of the demoted kind: %v %s/%s", err, d.Disposition, d.Reason)
	}
	// With no new evidence the mean decays back over the threshold (about a week), and
	// the owner hears Notices of the kind again.
	f.clock.Add(10 * 24 * time.Hour)
	if disp, _ := submit(200); disp != proactive.DispNotify {
		t.Errorf("after ten days the kind is still demoted (mean %v)", f.posterior().Mean())
	}
}
