package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// These tests drive the real, registered plan tools against a real gateway generation:
// a plan edit that makes the legacy job deliver again must be taken back by a debounced
// pass of the hand-over, not left for the hourly one.

const testDNDebounce = 150 * time.Millisecond

// linkedDNPlan builds the gateway, links the seeded plan to its job (the plan tools find
// the job by it) and returns both ids. The debounce is shortened before anything can
// request a pass.
func linkedDNPlan(t *testing.T, mutate func(*config.Config)) (f *assemblyFixture, planID int64, jobID string) {
	t.Helper()
	useSources(t, false, &countingSource{name: "fake"})
	enableTools := func(cfg *config.Config) {
		cfg.Tools.UpdateDeltaNeutralPlan.Enabled = true
		cfg.Tools.DeleteDeltaNeutralPlan.Enabled = true
		if mutate != nil {
			mutate(cfg)
		}
	}
	f = newAssemblyFixtureNoBuild(t, enableTools)
	planID, jobID = seedDNPlanAndJob(t, f)
	f.build(f.initial)
	f.services.Alerts.dn.debounce = testDNDebounce

	ctx := context.Background()
	p, err := f.services.DNStore.GetPlan(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	p.CronJobID = jobID
	if err := f.services.DNStore.UpdatePlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	return f, planID, jobID
}

func dnSpecs(t *testing.T, f *assemblyFixture) []triggers.Spec {
	t.Helper()
	specs, err := triggers.ReadSpecs(context.Background(), fixtureDB(t, f), triggers.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var out []triggers.Spec
	for _, s := range specs {
		if s.Kind == triggers.KindDNHealth {
			out = append(out, s)
		}
	}
	return out
}

func runDNTool(t *testing.T, f *assemblyFixture, name string, args map[string]any) {
	t.Helper()
	res := alertTool(t, f, name).Execute(tools.WithToolContext(context.Background(), "telegram", "1001"), args)
	if res.IsError {
		t.Fatalf("%s: %+v", name, res)
	}
}

func TestDNSync_APlanEditThatReEnablesTheJobIsTakenBackWithinTheDebounce(t *testing.T) {
	f, planID, job := linkedDNPlan(t, alertsMode(config.ProactiveModeLive))
	if _, enabled, marked := jobState(f, job); enabled || !marked {
		t.Fatal("the job was not promoted at start")
	}

	runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "enabled": true})
	if _, enabled, _ := jobState(f, job); !enabled {
		t.Fatal("the tool did not re-enable the job (the premise of the test)")
	}
	waitUntil(t, "the job off again", func() bool { _, enabled, marked := jobState(f, job); return !enabled && marked })
	if specs := dnSpecs(t, f); len(specs) != 1 || specs[0].Mode != triggers.ModeLive {
		t.Fatalf("specs = %+v, want exactly one live dn_health spec", specs)
	}
}

func TestDNSync_ChangingTheIntervalReplacesTheJobAndThePassPromotesTheNewOne(t *testing.T) {
	f, planID, oldJob := linkedDNPlan(t, alertsMode(config.ProactiveModeLive))

	runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "monitor_interval": "10m"})
	p, err := f.services.DNStore.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	newJob := p.CronJobID
	if newJob == "" || newJob == oldJob {
		t.Fatalf("the interval change made job %q (was %q)", newJob, oldJob)
	}
	waitUntil(t, "the new job promoted", func() bool { _, enabled, marked := jobState(f, newJob); return !enabled && marked })
	if exists, _, _ := jobState(f, oldJob); exists {
		t.Fatal("the old job is still there")
	}
	specs := dnSpecs(t, f)
	if len(specs) != 1 || specs[0].Mode != triggers.ModeLive || specs[0].Origin != "migration:cron:"+newJob {
		t.Fatalf("specs = %+v, want one live spec for the new job", specs)
	}
}

func TestDNSync_DeletingThePlanDropsItsSpec(t *testing.T) {
	f, planID, job := linkedDNPlan(t, alertsMode(config.ProactiveModeLive))
	if len(dnSpecs(t, f)) != 1 {
		t.Fatal("no spec at start")
	}

	// A plan can only be deleted once it is not active.
	p, err := f.services.DNStore.GetPlan(context.Background(), planID)
	if err != nil {
		t.Fatal(err)
	}
	p.Status = deltaneutral.PlanStatusClosed
	if err := f.services.DNStore.UpdatePlan(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	runDNTool(t, f, tools.NameDeleteDeltaNeutralPlan, map[string]any{"plan_id": float64(planID)})
	waitUntil(t, "the spec dropped", func() bool { return len(dnSpecs(t, f)) == 0 })
	if exists, _, _ := jobState(f, job); exists {
		t.Fatal("the job is still there")
	}
}

// Many edits in a row cost one pass.
func TestDNSync_ABurstOfEditsIsOnePass(t *testing.T) {
	f, planID, job := linkedDNPlan(t, alertsMode(config.ProactiveModeLive))
	f.services.Alerts.dn.debounce = 500 * time.Millisecond
	before := f.services.Alerts.dn.passes.Load()

	for i := 0; i < 6; i++ {
		runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "enabled": i%2 == 0})
	}
	runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "enabled": true})
	waitUntil(t, "the pass", func() bool { return f.services.Alerts.dn.passes.Load() > before })
	time.Sleep(700 * time.Millisecond) // a second pass would have come by now
	if n := f.services.Alerts.dn.passes.Load() - before; n != 1 {
		t.Fatalf("%d passes for a burst of edits, want 1", n)
	}
	if _, enabled, marked := jobState(f, job); enabled || !marked {
		t.Fatalf("job enabled=%v marked=%v after the pass", enabled, marked)
	}
}

// Without a registry the hook is a no-op: nothing is signalled and the job stays as
// the tool left it.
func TestDNSync_WithoutARegistryTheHookDoesNothing(t *testing.T) {
	f, planID, job := linkedDNPlan(t, nil)
	if f.services.Perception != nil && f.services.Perception.triggers != nil {
		t.Fatal("a registry was built")
	}
	f.services.CronService.EnableJob(job, false)

	runDNTool(t, f, tools.NameUpdateDeltaNeutralPlan, map[string]any{"plan_id": float64(planID), "enabled": true})
	time.Sleep(3 * testDNDebounce)
	if _, enabled, marked := jobState(f, job); !enabled || marked {
		t.Fatalf("job enabled=%v marked=%v, want enabled and untouched", enabled, marked)
	}
	if n := len(f.services.Alerts.dn.wake); n != 0 || f.services.Alerts.dn.passes.Load() != 0 {
		t.Fatalf("a request was queued (%d) or a pass ran (%d)", n, f.services.Alerts.dn.passes.Load())
	}
	// And the nil hook of a tool built without a gateway is as harmless.
	var nilHook tools.DNSyncFunc
	tools.NewDeleteDeltaNeutralPlanTool(f.services.DNStore, f.services.CronService).WithSync(nilHook)
}
