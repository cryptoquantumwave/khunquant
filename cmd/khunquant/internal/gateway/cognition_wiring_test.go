package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/cognition"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
)

func withCognition(cfg *config.Config) {
	withOwner("1001")(cfg)
	cfg.Proactive.Cognition.Enabled = true
}

func cognitionStats(f *assemblyFixture) map[string]any {
	f.t.Helper()
	body := f.expectStatus("/api/proactive/stats", testMgmtToken, http.StatusOK)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		f.t.Fatal(err)
	}
	c, _ := raw["cognition"].(map[string]any)
	return c
}

func submitJob(t *testing.T, s *cognition.Scheduler, j cognition.Job) <-chan cognition.Result {
	t.Helper()
	ch, err := s.Submit(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func receive(t *testing.T, ch <-chan cognition.Result, what string) cognition.Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no result", what)
		return cognition.Result{}
	}
}

// With cognition off - the default, and also proactive on with the switch off -
// nothing is built: no scheduler, none installed on the loop, nothing on the stats
// route, and /stop has no queue to drop.
func TestCognitionAssembly_OffBuildsNothing(t *testing.T) {
	for name, mutate := range map[string]func(*config.Config){
		"defaults":                 nil,
		"proactive on, switch off": withOwner("1001"),
		"switch on, proactive off": func(c *config.Config) { c.Proactive.Cognition.Enabled = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAssemblyFixture(t, mutate)
			if f.services.Cognition != nil || f.al.HasScheduler() {
				t.Fatalf("cognition built while off: runtime %v, installed %v", f.services.Cognition, f.al.HasScheduler())
			}
			if c := cognitionStats(f); c != nil {
				t.Errorf("stats carry a cognition section while off: %v", c)
			}
			if k := f.services.Control.stop.Load(); k != nil && k.Queue != nil {
				t.Error("/stop has a queue to drop while cognition is off")
			}
		})
	}
}

func TestCognitionAssembly_OnBuildsAndExposesStats(t *testing.T) {
	f := newAssemblyFixture(t, withCognition)
	rt := f.services.Cognition
	if rt == nil || !f.al.HasScheduler() {
		t.Fatalf("cognition not built: runtime %v installed %v", rt, f.al.HasScheduler())
	}
	if k := f.services.Control.stop.Load(); k == nil || k.Queue == nil {
		t.Error("/stop has no queue to drop while cognition is on")
	}
	c := cognitionStats(f)
	if c == nil {
		t.Fatal("no cognition section on the stats route")
	}
	if c["workers"] != float64(1) || c["tokens_per_day"] == nil || c["queued"] == nil {
		t.Errorf("cognition stats = %v", c)
	}
	// Cached: a burst of requests is one database read.
	if a, b := rt.stats(), rt.stats(); a != b {
		t.Error("two stats calls in a row were not served from the cache")
	}
}

// A reload rebuilds the scheduler: a job that is running when the services stop
// is cancelled once the drain window passes, a queued one is dropped with no
// fallback, and the new generation's scheduler takes over with the day's spend.
func TestCognitionAssembly_ReloadCancelsInFlightAndRebuilds(t *testing.T) {
	prev := cognitionDrainShare
	cognitionDrainShare = 300 // a 100ms drain window out of the 30s reload timeout
	t.Cleanup(func() { cognitionDrainShare = prev })

	f := newAssemblyFixture(t, withCognition)
	old := f.services.Cognition.sched

	// Spend some of the budget, so the reload has something to keep.
	res := submitJob(t, old, cognition.Job{
		Kind: proactive.TurnProactive, Priority: cognition.PriProactive, Budgeted: true,
		Run: func(context.Context) (cognition.Usage, error) {
			return cognition.Usage{InputTokens: 40, OutputTokens: 2}, nil
		},
	})
	receive(t, res, "the budgeted job")

	running := make(chan struct{})
	fellBack := make(chan string, 2)
	fb := func(_ context.Context, reason string) error { fellBack <- reason; return nil }
	runningRes := submitJob(t, old, cognition.Job{
		Kind: proactive.TurnUser, Priority: cognition.PriUser, SessionKey: "a", Fallback: fb,
		Run: func(ctx context.Context) (cognition.Usage, error) {
			close(running)
			<-ctx.Done()
			return cognition.Usage{}, ctx.Err()
		},
	})
	<-running
	ran := make(chan struct{}, 1)
	queuedRes := submitJob(t, old, cognition.Job{
		Kind: proactive.TurnUser, Priority: cognition.PriUser, SessionKey: "a", Fallback: fb,
		Run: func(context.Context) (cognition.Usage, error) { ran <- struct{}{}; return cognition.Usage{}, nil },
	})

	if err := f.reload(withCognition); err != nil {
		t.Fatal(err)
	}

	if r := receive(t, runningRes, "the running job"); !errors.Is(r.Err, cognition.ErrStopped) || r.Fallback {
		t.Errorf("running job at reload: %+v, want cancelled (ErrStopped) with no fallback", r)
	}
	if r := receive(t, queuedRes, "the queued job"); !errors.Is(r.Err, cognition.ErrStopped) || r.Fallback {
		t.Errorf("queued job at reload: %+v, want dropped (ErrStopped) with no fallback", r)
	}
	select {
	case <-ran:
		t.Error("a queued job ran after the services stopped")
	case reason := <-fellBack:
		t.Errorf("a fallback ran (%s) for a job the reload cancelled", reason)
	default:
	}

	next := f.services.Cognition
	if next == nil || next.sched == old || !f.al.HasScheduler() {
		t.Fatalf("the reload did not install a new scheduler (runtime %v)", next)
	}
	if _, err := old.Submit(context.Background(), cognition.Job{Run: func(context.Context) (cognition.Usage, error) { return cognition.Usage{}, nil }}); !errors.Is(err, cognition.ErrStopped) {
		t.Errorf("the old scheduler still accepts work: %v", err)
	}
	if u := next.sched.Stats().Budget["proactive"]; u.InputTokens != 40 || u.OutputTokens != 2 {
		t.Errorf("budget after the reload = %+v, want the 40/2 tokens already spent today", u)
	}
}

// Jobs that finish inside the drain window are not cancelled.
func TestCognitionAssembly_StopLetsQueuedJobsFinish(t *testing.T) {
	f := newAssemblyFixture(t, withCognition)
	s := f.services.Cognition.sched
	res := submitJob(t, s, cognition.Job{
		Kind: proactive.TurnUser, Priority: cognition.PriUser,
		Run: func(context.Context) (cognition.Usage, error) {
			time.Sleep(200 * time.Millisecond)
			return cognition.Usage{}, nil
		},
	})
	if err := f.reload(withCognition); err != nil {
		t.Fatal(err)
	}
	if r := receive(t, res, "the job"); r.Err != nil {
		t.Errorf("a job that fits in the drain window was cancelled: %v", r.Err)
	}
}

// A reload that switches cognition off removes the scheduler from the loop.
func TestCognitionAssembly_ReloadToOffUninstalls(t *testing.T) {
	f := newAssemblyFixture(t, withCognition)
	if !f.al.HasScheduler() {
		t.Fatal("not installed")
	}
	if err := f.reload(withOwner("1001")); err != nil {
		t.Fatal(err)
	}
	if f.services.Cognition != nil || f.al.HasScheduler() {
		t.Error("cognition survived a reload that turned it off")
	}
	if err := f.reload(withCognition); err != nil {
		t.Fatal(err)
	}
	if f.services.Cognition == nil || !f.al.HasScheduler() {
		t.Error("cognition did not come back on")
	}
}

// A legacy heartbeat tick over the cognition budget is skipped, silently: no model
// call (the fixture's provider would fail the tick) and no error result.
func TestCognitionAssembly_LegacyHeartbeatOverBudgetIsSkipped(t *testing.T) {
	f := newAssemblyFixture(t, func(c *config.Config) {
		withCognition(c)
		c.Proactive.Cognition.TokensPerDay = 0
	})
	res := heartbeatHandler(f.al)("check the list", "", "")
	if res == nil || res.IsError {
		t.Fatalf("a skipped tick returned %+v, want a silent result", res)
	}
	if st := f.services.Cognition.sched.Stats(); len(st.Budget) != 0 {
		t.Errorf("a skipped tick was charged: %+v", st.Budget)
	}
}

// A generation that has stopped leaves its scheduler on the loop, refusing work: the
// outbox's final flush composes nothing outside the budget.
func TestCognitionAssembly_AfterStopBudgetedTurnsAreSkipped(t *testing.T) {
	f := newAssemblyFixture(t, withCognition)
	f.services.Cognition.stop(time.Second)
	if !f.al.HasScheduler() {
		t.Fatal("the stopped scheduler was taken off the loop")
	}
	_, err := f.al.RunProactiveTurn(context.Background(), agent.ProactiveTurnRequest{
		Kind: proactive.TurnProactive, Prompt: "compose", Deadline: time.Second,
	})
	if !errors.Is(err, proactive.ErrTurnSkipped) {
		t.Errorf("composer turn after stop: err = %v, want ErrTurnSkipped", err)
	}
}
