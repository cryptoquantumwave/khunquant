package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/bus"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

const legacyWarning = "legacy job ran while triggers live"

func job(id, name string) *cron.CronJob { return &cron.CronJob{ID: id, Name: name} }

func newWarner(day *time.Time) *legacyJobWarner {
	return &legacyJobWarner{now: func() time.Time { return *day }}
}

// A job of a family the triggers replaced warns when it runs while they are live,
// at most once per job per day; nothing else warns, and dca: never does.
func TestLegacyJobWhileLive_Warns(t *testing.T) {
	day := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	w := newWarner(&day)
	live, notLive := func() bool { return true }, func() bool { return false }
	logs := captureLogs(t)

	for _, name := range []string{"price_alert:BTC/USDT above 70000", "indicator_alert:ETH rsi", "dn:12:basis"} {
		if !w.check(job("id-"+name, name), live) {
			t.Errorf("%s: no warning while triggers are live", name)
		}
	}
	if n := strings.Count(logs.String(), legacyWarning); n != 3 {
		t.Fatalf("%d warnings, want 3:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "family=price_alert") || !strings.Contains(logs.String(), "family=dn") {
		t.Errorf("the warning does not name the family:\n%s", logs.String())
	}

	// Not live: nothing to say. Not a legacy family: nothing to say.
	before := logs.String()
	for _, tc := range []struct {
		j    *cron.CronJob
		live func() bool
	}{
		{job("n1", "price_alert:BTC/USDT"), notLive},
		{job("n2", "price_alert:BTC/USDT"), nil},
		{job("n3", "dca:7:weekly"), live},
		{job("n4", "morning brief"), live},
		{nil, live},
	} {
		if w.check(tc.j, tc.live) {
			t.Errorf("warned for %+v", tc.j)
		}
	}
	if logs.String() != before {
		t.Errorf("a non-warning logged:\n%s", strings.TrimPrefix(logs.String(), before))
	}

	// Once per job per day.
	if w.check(job("id-dn:12:basis", "dn:12:basis"), live) {
		t.Error("the same job warned twice on one day")
	}
	if !w.check(job("other", "dn:12:basis"), live) {
		t.Error("another job did not warn")
	}
	day = day.Add(24 * time.Hour)
	if !w.check(job("id-dn:12:basis", "dn:12:basis"), live) {
		t.Error("the job did not warn again the next day")
	}
}

// The warner's memory is bounded.
func TestLegacyJobWarner_IsBounded(t *testing.T) {
	day := time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)
	w := newWarner(&day)
	for i := range 3 * maxLegacyWarned {
		w.check(job(strings.Repeat("x", 3)+string(rune('a'+i%26))+time.Duration(i).String(), "dn:1:x"), func() bool { return true })
		if len(w.warned) > maxLegacyWarned {
			t.Fatalf("%d jobs remembered", len(w.warned))
		}
	}
}

// liveAlerts is a tools.AlertTriggersFunc over a registry whose global mode is as
// given.
func liveAlerts(t *testing.T, mode string) tools.AlertTriggersFunc {
	t.Helper()
	reg, err := triggers.NewRegistry(context.Background(), sqlitextest.Open(t), func() string { return mode }, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := &alertRuntime{}
	rt.reg.Store(reg)
	return rt.resolver()
}

// runLegacyJob runs a price-alert job whose stored payload cannot be decoded, so the
// legacy handler fails at once without touching an exchange, and returns what the
// job recorded.
func runLegacyJob(t *testing.T, name string, hooks cronHooks) cron.CronJobState {
	t.Helper()
	workspace := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = workspace
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()
	al := agent.NewAgentLoop(cfg, msgBus, noopProvider{})
	defer al.Close()

	svc, _ := setupCronTool(al, msgBus, workspace, true, 0, cfg, hooks)
	j, err := svc.AddJob(name, cron.CronSchedule{Kind: "every", EveryMS: ptr(int64(3600_000))}, "not json", false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RunJobNow(j.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := svc.GetJob(j.ID)
		if got != nil && got.State.LastStatus != "" && !got.State.Running {
			return got.State
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func ptr[T any](v T) *T { return &v }

// Through the real cron dispatch: with the registry live the legacy job still runs
// (rollback safety) and says so; without it, the job runs and says nothing.
func TestLegacyJobWhileLive_StillRunsAndWarnsThroughTheDispatch(t *testing.T) {
	saved := legacyJobs
	t.Cleanup(func() { legacyJobs = saved })
	legacyJobs = &legacyJobWarner{now: time.Now}

	logs := captureLogs(t)
	st := runLegacyJob(t, "price_alert:BTC/USDT above 70000", cronHooks{Alerts: liveAlerts(t, triggers.ModeLive)})
	if !strings.Contains(st.LastError, "decode payload") {
		t.Fatalf("the legacy handler did not run: %+v", st)
	}
	if !strings.Contains(logs.String(), legacyWarning) {
		t.Fatalf("no warning for a legacy job that ran while triggers were live:\n%s", logs.String())
	}

	legacyJobs = &legacyJobWarner{now: time.Now}
	logs = captureLogs(t)
	st = runLegacyJob(t, "price_alert:BTC/USDT above 70000", cronHooks{Alerts: liveAlerts(t, triggers.ModeShadow)})
	if !strings.Contains(st.LastError, "decode payload") {
		t.Fatalf("the legacy handler did not run: %+v", st)
	}
	if strings.Contains(logs.String(), legacyWarning) {
		t.Fatalf("warned while the registry was only in shadow:\n%s", logs.String())
	}

	logs = captureLogs(t)
	runLegacyJob(t, "price_alert:BTC/USDT above 70000", cronHooks{})
	if strings.Contains(logs.String(), legacyWarning) {
		t.Fatalf("warned with no registry at all:\n%s", logs.String())
	}
}
