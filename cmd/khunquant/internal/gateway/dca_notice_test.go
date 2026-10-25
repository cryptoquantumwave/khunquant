package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
)

func notPlanOwned(planID int64, name string) error {
	return &dcaJobNotPlanOwnedError{PlanID: planID, PlanName: name, JobName: "dca:1:x", JobID: "j", RecordedJobID: "k"}
}

func forgetNotice(t *testing.T, jobID string) {
	t.Helper()
	dcaNotPlanOwnedNotified.Delete(jobID)
	t.Cleanup(func() { dcaNotPlanOwnedNotified.Delete(jobID) })
}

// The user is told once per job per process that the plan is not running, and
// only when there is somewhere to tell them.
func TestNotifyDCAJobNotPlanOwned(t *testing.T) {
	err := notPlanOwned(7, "btc weekly")
	if !errors.Is(err, errDCAJobNotPlanOwned) {
		t.Fatal("the error does not wrap the sentinel")
	}

	job := &cron.CronJob{ID: "notice-job-1", Name: "dca:7:btc weekly"}
	job.Payload.Channel, job.Payload.To = "telegram", "chat-1"
	forgetNotice(t, job.ID)
	pub := &recordingPublisher{}

	notifyDCAJobNotPlanOwned(pub, job, err)
	notifyDCAJobNotPlanOwned(pub, job, err) // the next tick
	want := []bus.OutboundMessage{{
		Channel: "telegram", ChatID: "chat-1",
		Content: "DCA plan 7 (btc weekly) is not running: its schedule is not linked to the plan. " +
			"Ask me to update the plan's schedule (the same schedule is fine) to re-link it.",
	}}
	if got := pub.messages(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sent %+v, want exactly %+v", got, want)
	}

	// Other errors, and jobs with no target, say nothing.
	other := &cron.CronJob{ID: "notice-job-2", Name: "dca:7:x"}
	other.Payload.Channel, other.Payload.To = "telegram", "chat-1"
	forgetNotice(t, other.ID)
	notifyDCAJobNotPlanOwned(pub, other, errors.New("provider down"))
	notifyDCAJobNotPlanOwned(pub, other, nil)
	untargeted := &cron.CronJob{ID: "notice-job-3", Name: "dca:7:x"}
	forgetNotice(t, untargeted.ID)
	notifyDCAJobNotPlanOwned(pub, untargeted, err)
	if n := len(pub.messages()); n != 1 {
		t.Fatalf("%d message(s) in total, want 1", n)
	}

	// A failed publish is not remembered, so the next tick tries again.
	retry := &cron.CronJob{ID: "notice-job-4", Name: "dca:7:x"}
	retry.Payload.Channel, retry.Payload.To = "telegram", "chat-2"
	forgetNotice(t, retry.ID)
	notifyDCAJobNotPlanOwned(&recordingPublisher{err: errors.New("bus is full")}, retry, err)
	notifyDCAJobNotPlanOwned(pub, retry, err)
	if n := len(pub.messages()); n != 2 {
		t.Fatalf("the retry sent %d message(s) in total, want 2", n)
	}
}

// The upgrade scenario end to end: a plan whose stored job is long gone, with a
// live dca: job still ticking. The tick is an error and tells the user once; the
// remedy in the message (update the schedule, the same one is fine) leaves one
// linked job, removes the stranded one, spares another plan's job, and the next
// tick runs.
func TestStalePlanLink_IsReportedAndTheRemedyWorks(t *testing.T) {
	store, cronService, cronTool, probe, cfg, planID, live := newDCAHandlerFixture(t)
	forgetNotice(t, live.ID)

	// Make the stored link stale, the way an old update_dca_plan could.
	plan, err := store.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	plan.CronJobID = "removed-long-ago"
	if err := store.UpdatePlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	neighbour, err := cronService.AddJob(fmt.Sprintf("dca:%d2:other plan", planID),
		cron.CronSchedule{Kind: "cron", Expr: "0 9 * * 1", TZ: "UTC"}, "[DCA-AUTO]", false, "telegram", "chat-9")
	if err != nil {
		t.Fatal(err)
	}

	pub := &recordingPublisher{}
	tick := func(job *cron.CronJob) error {
		_, err := handleDCAAutoJob(context.Background(), job, cfg, store, cronTool)
		notifyDCAJobNotPlanOwned(pub, job, err)
		return err
	}

	for i := 0; i < 2; i++ {
		if err := tick(live); !errors.Is(err, errDCAJobNotPlanOwned) {
			t.Fatalf("tick %d: %v, want the not-plan-owned error", i+1, err)
		}
	}
	if probe.origin != "" {
		t.Fatal("a turn started for an unlinked job")
	}
	msgs := pub.messages()
	if len(msgs) != 1 || msgs[0].Channel != "telegram" || msgs[0].ChatID != "chat-1" ||
		!strings.Contains(msgs[0].Content, fmt.Sprintf("DCA plan %d (btc weekly) is not running", planID)) {
		t.Fatalf("notice = %+v, want exactly one for plan %d to telegram/chat-1", msgs, planID)
	}

	// The remedy, with the plan's current schedule.
	res := tools.NewUpdateDCAPlanTool(store, cronService).Execute(ownerCtx(), map[string]any{
		"plan_id": float64(planID), "schedule": map[string]any{"cron": "0 9 * * 1"},
	})
	if res.IsError {
		t.Fatalf("update: %s", res.ForLLM)
	}
	if cronService.GetJob(live.ID) != nil {
		t.Error("the stranded live job is still scheduled")
	}
	if cronService.GetJob(neighbour.ID) == nil {
		t.Error("another plan's job was removed")
	}
	plan, _ = store.GetPlan(context.Background(), planID)
	var own []cron.CronJob
	for _, j := range cronService.ListJobs(true) {
		if strings.HasPrefix(j.Name, fmt.Sprintf("dca:%d:", planID)) {
			own = append(own, j)
		}
	}
	if len(own) != 1 || own[0].ID != plan.CronJobID {
		t.Fatalf("jobs for the plan = %+v, want exactly the recorded one (%s)", own, plan.CronJobID)
	}

	if err := tick(&own[0]); err != nil {
		t.Fatalf("the re-linked job was refused: %v", err)
	}
	if probe.scope.DCAPlanID != planID {
		t.Errorf("the re-linked job ran with scope %+v", probe.scope)
	}
	if n := len(pub.messages()); n != 1 {
		t.Errorf("%d notices after the fix, want still 1", n)
	}
}
