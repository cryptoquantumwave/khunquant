package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/approval"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/journal"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

// F-DN-3: the close card is lost while the chat channel is down, and no close proposal
// existed until the next round, 30 minutes later. dn.recover now retries the proposal inside
// the round (1, 3, 7 and 15 minutes in) when, and only when, the card could not be delivered.

// proposalRows counts the stored proposals by status (cancelled ones are the undelivered cards).
func proposalRows(t *testing.T, n *dnNode) map[string]int {
	t.Helper()
	rows, err := n.db.Query(`SELECT status, COUNT(*) FROM proposals GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var s string
		var c int
		if err := rows.Scan(&s, &c); err != nil {
			t.Fatal(err)
		}
		got[s] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// advanceTo moves the clock to d after t0 (never back).
func advanceTo(r *dnRig, t0 time.Time, d time.Duration) {
	if wait := t0.Add(d).Sub(r.clock.Now()); wait > 0 {
		r.clock.Advance(wait)
	}
}

func followUps(r *dnRig) (out []deltaneutral.RecoveryAlert) {
	for _, a := range alertDetails(r) {
		if a.FollowUp {
			out = append(out, a)
		}
	}
	return out
}

// The channel is down at the first try and back a minute later: the second try, inside the
// same round, creates ONE proposal, and only then does the owner hear it is waiting.
func TestFDN3_CardNotDeliveredThenSecondAttemptProposes(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	if w := r.drive(n, rec, false); w.Status != journal.StatusWaiting {
		t.Fatalf("dn.recover: %+v, want it to wait for the retry", w)
	}
	if r.tg.count() != 0 || len(followUps(r)) != 0 {
		t.Fatalf("%d cards, %d follow-ups before the retry", r.tg.count(), len(followUps(r)))
	}
	if txt := alertsText(r); strings.Contains(txt, claimsWaiting) {
		t.Fatalf("an alert claims a proposal is waiting before one exists:\n%s", txt)
	}

	// Not yet: the retry is due a minute in.
	advanceTo(r, t0, 59*time.Second)
	r.drive(n, rec, false)
	if r.tg.count() != 0 {
		t.Fatal("retried before its time")
	}

	r.tg.down = false
	advanceTo(r, t0, time.Minute)
	r.drive(n, rec, false)
	if r.tg.count() != 1 {
		t.Fatalf("%d cards after the retry, want 1", r.tg.count())
	}
	p, op := lastProposal(t, r, n)
	if !op.ReduceOnly || op.Amount != dnFirstContracts || p.Status != approval.StatusPending {
		t.Errorf("proposed %+v %+v", p, op)
	}
	if rows := proposalRows(t, n); rows["pending"] != 1 || rows["cancelled"] != 1 || len(rows) != 2 {
		t.Errorf("proposals %v, want the undelivered one cancelled and ONE pending", rows)
	}
	fu := followUps(r)
	if len(fu) != 1 || !strings.Contains(fu[0].Detail, claimsWaiting+" (code "+p.Code+").") {
		t.Fatalf("follow-ups %+v, want one that says the proposal %s is waiting", fu, p.Code)
	}
	if strings.Contains(alertDetails(r)[0].Detail, claimsWaiting) {
		t.Errorf("the first alert claims a proposal: %q", alertDetails(r)[0].Detail)
	}
	if r.world.placed("futures") != 1 {
		t.Error("dn.recover placed an order of its own")
	}
	// the round goes on as before: it waits for the decision, and no more cards come
	r.drive(n, rec, false)
	if r.tg.count() != 1 || proposalRows(t, n)["pending"] != 1 {
		t.Errorf("a second proposal was made: %d cards, %v", r.tg.count(), proposalRows(t, n))
	}
}

// The channel never comes back inside the round: the retries stop at the schedule (5 tries
// in all, every one before the 30-minute window ends), the owner is told nothing was
// created and when the next round is, and that round still runs and proposes.
func TestFDN3_RetriesStopAtTheWindowAndTheNextRoundStillRuns(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	r.drive(n, rec, false)
	for _, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute} {
		advanceTo(r, t0, at)
		r.drive(n, rec, false)
		if len(followUps(r)) != 0 {
			t.Fatalf("an outcome was sent at +%s, while retries remain", at)
		}
	}
	advanceTo(r, t0, 15*time.Minute)
	r.drive(n, rec, false)
	if rows := proposalRows(t, n); rows["cancelled"] != 5 || len(rows) != 1 {
		t.Fatalf("proposals %v, want 5 undelivered (the first try and 4 retries)", rows)
	}
	fu := followUps(r)
	if len(fu) != 1 || !strings.Contains(fu[0].Detail, "No close proposal could be created (the approval card could not be delivered") ||
		!strings.Contains(fu[0].Detail, "Recovery will try again in about 15 minutes.") || strings.Contains(alertsText(r), claimsWaiting) {
		t.Fatalf("outcome alerts:\n%s", alertsText(r))
	}

	// No sixth try inside the round.
	advanceTo(r, t0, 29*time.Minute)
	r.drive(n, rec, false)
	if rows := proposalRows(t, n); rows["cancelled"] != 5 {
		t.Fatalf("proposals %v after the retries ended", rows)
	}

	// The channel is back for the next round, which proposes.
	r.tg.down = false
	advanceTo(r, t0, deltaneutral.RecoverOfferEvery)
	r.drive(n, rec, false)
	if r.tg.count() != 1 {
		t.Fatalf("%d cards: the next round did not propose", r.tg.count())
	}
	if p, _ := lastProposal(t, r, n); p.Status != approval.StatusPending {
		t.Errorf("proposal %+v", p)
	}
}

// A refusal that is not an undelivered card is not retried in the round: it is the next
// round's business, as before.
func TestFDN3_PendingBoundRefusalIsNotRetried(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	for i := 0; i < approval.MaxPendingProtective; i++ {
		_, err := n.svc.Propose(context.Background(), approval.ProposeRequest{
			Order: approval.OrderRequest{
				Provider: r.world.id, Market: approval.MarketFutures, Symbol: "BTC/USDT:USDT", Side: "buy",
				Contracts: 0.5 + float64(i)/10, ReduceOnly: true,
			},
			Origin: approval.OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "x", ProtectiveKey: "protect:dn_recover:9",
		})
		if err != nil {
			t.Fatalf("protective close %d: %v", i, err)
		}
	}
	before := proposalRows(t, n)
	cards := r.tg.count()
	t0 := r.clock.Now()
	r.drive(n, rec, false)
	if fu := followUps(r); len(fu) != 1 || !strings.Contains(fu[0].Detail, "too many exit proposals are already waiting") ||
		!strings.Contains(fu[0].Detail, "Recovery will try again in about 30 minutes.") {
		t.Fatalf("the outcome came at once with the full 30 minutes; alerts:\n%s", alertsText(r))
	}
	advanceTo(r, t0, 9*time.Minute) // the owner's three expire at 10 min: they stay pending now
	r.drive(n, rec, false)
	if got := proposalRows(t, n); got["pending"] != before["pending"] || got["cancelled"] != 0 || r.tg.count() != cards {
		t.Errorf("a retry ran: proposals %v (was %v), cards %d (was %d)", got, before, r.tg.count(), cards)
	}
}

// The plan is closed (an unwind) while the retry waits: the retry proposes nothing.
func TestFDN3_PlanClosedBetweenRetries(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	r.drive(n, rec, false)
	if err := n.store.UpdatePlanStatus(context.Background(), r.planID, deltaneutral.PlanStatusClosed); err != nil {
		t.Fatal(err)
	}
	r.tg.down = false
	advanceTo(r, t0, time.Minute)
	r.drive(n, rec, false)
	if r.tg.count() != 0 {
		t.Fatalf("%d cards for a closed plan", r.tg.count())
	}
	if rows := proposalRows(t, n); rows["pending"] != 0 || len(rows) != 1 {
		t.Errorf("proposals %v: only the undelivered first try may exist", rows)
	}
	fu := followUps(r)
	if len(fu) != 1 || !strings.Contains(fu[0].Detail, "Nothing is left to close: the plan was closed") || strings.Contains(alertsText(r), claimsWaiting) {
		t.Fatalf("alerts:\n%s", alertsText(r))
	}
}

// The orphan is gone (the owner closed it by hand) while the retry waits: nothing to propose.
func TestFDN3_OrphanGoneBetweenRetries(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	r.drive(n, rec, false)
	r.world.mu.Lock()
	r.world.position = 0
	r.world.mu.Unlock()
	r.tg.down = false
	advanceTo(r, t0, time.Minute)
	r.drive(n, rec, false)
	if r.tg.count() != 0 || proposalRows(t, n)["pending"] != 0 {
		t.Fatalf("a proposal for an orphan that is gone: %d cards, %v", r.tg.count(), proposalRows(t, n))
	}
	if fu := followUps(r); len(fu) != 1 || !strings.Contains(fu[0].Detail, "Nothing is left to close") {
		t.Fatalf("alerts:\n%s", alertsText(r))
	}
}

// A restart while the retry waits: another process resumes the workflow and makes ONE
// proposal; the first process, driven again, makes none, and nothing is proposed twice.
func TestFDN3_RestartDuringTheRetryWaitProposesOnce(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	r.drive(n, rec, false)
	if w := wfStatus(t, n, rec); w != journal.StatusWaiting {
		t.Fatalf("dn.recover is %s", w)
	}
	n2 := r.node() // the process after a restart: same databases, new engine
	r.tg.down = false
	advanceTo(r, t0, time.Minute)
	r.drive(n2, rec, false)
	r.drive(n, rec, false) // the old process wakes up too: the journal's lease and steps allow one run
	r.drive(n2, rec, false)
	if r.tg.count() != 1 {
		t.Fatalf("%d cards, want exactly 1", r.tg.count())
	}
	if rows := proposalRows(t, n2); rows["pending"] != 1 || rows["cancelled"] != 1 || len(rows) != 2 {
		t.Fatalf("proposals %v, want one pending and the undelivered first try", rows)
	}
	if fu := followUps(r); len(fu) != 1 || !strings.Contains(fu[0].Detail, claimsWaiting) {
		t.Fatalf("alerts:\n%s", alertsText(r))
	}
}

// The undelivered tries do not use up the protective class's hour: after a long outage (more
// tries than the bound) the close still goes through, and a delivered close counts as before.
func TestFDN3_UndeliveredRetriesDoNotExhaustTheHourlyBound(t *testing.T) {
	r := newDNRig(t, true)
	n, rec, _ := failedSecondLeg(t, r)
	t0 := r.clock.Now()
	r.tg.down = true
	r.drive(n, rec, false)
	// A protective close that WAS delivered (and rejected) counts toward the hour, as before.
	counted := func() {
		r.tg.down = false
		_, err := n.svc.Propose(context.Background(), approval.ProposeRequest{
			Order: approval.OrderRequest{
				Provider: r.world.id, Market: approval.MarketFutures, Symbol: "BTC/USDT:USDT", Side: "buy",
				Contracts: 0.5, ReduceOnly: true,
			},
			Origin: approval.OriginProactive, Trust: proactive.TrustSystem, DeliverNow: true, CandidateID: "x", ProtectiveKey: "protect:dn_recover:9",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.tg.tap(t, n.svc, "reject"); err != nil {
			t.Fatal(err)
		}
		r.tg.down = true
	}
	// Two rounds of outage inside the hour: 2 x 5 undelivered tries, with the delivered one
	// that is 11 proposals created in the hour, over the bound of MaxProtectivePerHour (10).
	for i, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute, 20 * time.Minute, deltaneutral.RecoverOfferEvery,
		deltaneutral.RecoverOfferEvery + time.Minute, deltaneutral.RecoverOfferEvery + 3*time.Minute,
		deltaneutral.RecoverOfferEvery + 7*time.Minute, deltaneutral.RecoverOfferEvery + 15*time.Minute} {
		advanceTo(r, t0, at)
		if i == 4 {
			counted()
		}
		r.drive(n, rec, false)
	}
	if rows := proposalRows(t, n); rows["cancelled"] != 2*(1+len(retryOffsets())) || rows["rejected"] != 1 || len(rows) != 2 {
		t.Fatalf("proposals %v, want %d undelivered and the rejected one", rows, 2*(1+len(retryOffsets())))
	}
	if 2*(1+len(retryOffsets()))+1 <= approval.MaxProtectivePerHour {
		t.Fatalf("the test no longer reaches the bound (%d)", approval.MaxProtectivePerHour)
	}
	// the channel is back for the third round (in the same hour)
	r.tg.down = false
	advanceTo(r, t0, 2*deltaneutral.RecoverOfferEvery)
	r.drive(n, rec, false)
	if r.tg.count() != 2 || proposalRows(t, n)["pending"] != 1 { // the rejected one's card and the close's
		t.Fatalf("%d cards, %v: the hourly bound was used up by the undelivered tries; alerts:\n%s", r.tg.count(), proposalRows(t, n), alertsText(r))
	}
}

// "The card was not delivered" is typed, not read off the text: only an error that wraps
// approval.ErrCardNotDelivered is retried, and it is still an ErrUnavailable.
func TestFDN3_CardNotDeliveredIsTyped(t *testing.T) {
	if !errors.Is(approval.ErrCardNotDelivered, approval.ErrUnavailable) {
		t.Fatal("ErrCardNotDelivered must be an ErrUnavailable")
	}
	wrapped := fmt.Errorf("%w (chat channel down)", approval.ErrCardNotDelivered)
	if got := closeRefusalReason(wrapped); !strings.Contains(got, "chat channel may be down") {
		t.Errorf("reason for the typed error: %q", got)
	}
	lookalike := fmt.Errorf("%w: the card could not be delivered to the owner", approval.ErrUnavailable)
	if got := closeRefusalReason(lookalike); got != "approvals are not available" {
		t.Errorf("reason for a text that only looks like it: %q", got)
	}
}

func retryOffsets() []time.Duration {
	return []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute}
}
