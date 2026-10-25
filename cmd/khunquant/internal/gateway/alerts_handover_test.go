package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// liveTriggersShadowAttention is the default attention mode with triggers asked to be
// live: a valid owner, perception on, nothing else.
func liveTriggersShadowAttention() func(*config.Config) {
	return compose(withOwner("1001"), withPerception, withTriggers(config.ProactiveModeLive),
		withAttentionMode(config.ProactiveModeShadow))
}

// Live triggers deliver through attention. With attention in shadow (the default) a
// live registry would switch the cron alerts off and then log every alert, so the
// effective triggers mode is shadow, nothing is promoted and the tools write cron jobs.
func TestAlerts_LiveTriggersWithShadowAttentionPromoteNothing(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, liveTriggersShadowAttention())
	price, ind, one := seedAlertJobs(t, f)
	f.build(f.initial)

	if e := f.initial.Proactive.Effective(); e.TriggersMode != config.ProactiveModeShadow {
		t.Fatalf("effective triggers = %s", e.TriggersMode)
	}
	rt := f.services.Perception
	if rt == nil || rt.triggers == nil {
		t.Fatal("no registry")
	}
	if got := rt.triggers.GlobalMode(); got != triggers.ModeShadow {
		t.Fatalf("registry mode = %s, want shadow", got)
	}
	if f.services.Alerts.resolver()().Live() {
		t.Fatal("the alert backend claims to be live")
	}
	modes := specModes(t, f)
	for _, id := range []string{price, ind, one} {
		if modes["migration:cron:"+id] != triggers.ModeShadow {
			t.Fatalf("spec for %s = %q, want shadow: nothing may be promoted", id, modes["migration:cron:"+id])
		}
		if exists, enabled, marked := jobState(f, id); !exists || !enabled || marked {
			t.Fatalf("job %s: %v %v %v, want it left delivering", id, exists, enabled, marked)
		}
	}
	jobs := f.cronJobCount()
	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(context.Background(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "SOL/USDT", "condition": "below", "threshold": 100.0})
	if res.IsError || f.cronJobCount() != jobs+1 || strings.Contains(res.ForLLM, "owner") {
		t.Fatalf("the tool did not write a cron job: %+v (jobs %d -> %d)", res, jobs, f.cronJobCount())
	}
}

// A job promoted while attention was live is handed back by a generation whose
// attention is not, though triggers.mode still says live.
func TestAlerts_AttentionLeavingLiveRollsPromotedJobsBack(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeLive))
	price, _, _ := seedAlertJobs(t, f)
	f.build(f.initial)
	if _, enabled, marked := jobState(f, price); enabled || !marked {
		t.Fatal("not promoted")
	}
	if err := f.reload(liveTriggersShadowAttention()); err != nil {
		t.Fatal(err)
	}
	if exists, enabled, marked := jobState(f, price); !exists || !enabled || marked {
		t.Fatalf("job %v %v %v: not handed back", exists, enabled, marked)
	}
	if got := specModes(t, f)["migration:cron:"+price]; got != triggers.ModeShadow {
		t.Fatalf("spec = %q", got)
	}
	if f.services.Alerts.resolver()().Live() {
		t.Fatal("live")
	}
}

type countLogs struct{ lockedBuf }

func (c *countLogs) count(sub string) int { return strings.Count(c.String(), sub) }

func standIns(f *assemblyFixture) []string {
	var out []string
	for _, j := range f.services.CronService.ListJobs(true) {
		if j.State.StandInFor != "" {
			out = append(out, j.State.StandInFor+"|"+j.Payload.Channel+"|"+j.Payload.To)
		}
	}
	return out
}

// An alert made through the tool while triggers were live exists only as a spec. In
// every generation without a live registry it is handed to the cron path, routed to the
// configured owner, and handed back to the SAME spec when the registry is live again.
func TestAlerts_ToolMadeAlertSurvivesTheRegistryGoingAway(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	res := alertTool(t, f, tools.NameSetPriceAlert).Execute(tools.WithToolContext(context.Background(), "telegram", "-100group"),
		map[string]any{"action": "create", "provider": "okx", "symbol": "SOL/USDT", "condition": "below", "threshold": 100.0, "recurring": true})
	if res.IsError {
		t.Fatal(res.ForLLM)
	}
	var specID string
	for _, s := range mustSpecs(t, f) {
		specID = s.ID
	}
	if specID == "" {
		t.Fatal("no spec")
	}

	for name, away := range map[string]func(*config.Config){
		"triggers shadow":                       alertsMode(config.ProactiveModeShadow),
		"attention shadow":                      liveTriggersShadowAttention(),
		"triggers off":                          compose(liveWithPerception(), withTriggers(config.ProactiveModeOff)),
		"perception off":                        compose(withOwner("1001"), withAttentionMode(config.ProactiveModeLive), withTriggers(config.ProactiveModeLive)),
		"proactive off, owner still configured": func(cfg *config.Config) { withOwner("1001")(cfg); cfg.Proactive.Enabled = false },
		"triggers shadow 2x":                    alertsMode(config.ProactiveModeShadow),
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.reload(away); err != nil {
				t.Fatal(err)
			}
			got := standIns(f)
			if len(got) != 1 || got[0] != specID+"|telegram|1001" {
				t.Fatalf("stand-ins = %v, want exactly one for the spec, routed to the configured owner", got)
			}
			if s := mustSpecs(t, f)[0]; s.Mode != triggers.ModeOff || s.ID != specID {
				t.Fatalf("spec = %+v: it should be dormant, not duplicated", s)
			}
			// The owner sees it, and can cancel it.
			list := alertTool(t, f, tools.NameSetPriceAlert).Execute(context.Background(), map[string]any{"action": "list"}).ForLLM
			if !strings.Contains(list, "price_alert:okx:SOL/USDT:below:100") || strings.Count(list, "SOL/USDT") != 1 {
				t.Fatalf("list = %s", list)
			}
		})
	}

	// The registry is live again: the same spec, the stand-in gone.
	if err := f.reload(alertsMode(config.ProactiveModeLive)); err != nil {
		t.Fatal(err)
	}
	if got := standIns(f); len(got) != 0 {
		t.Fatalf("stand-ins = %v after the registry came back", got)
	}
	specs := mustSpecs(t, f)
	if len(specs) != 1 || specs[0].ID != specID || specs[0].Mode != triggers.ModeLive {
		t.Fatalf("specs = %+v, want the same one, live", specs)
	}
	if !f.services.Alerts.resolver()().Live() {
		t.Fatal("not live")
	}
}

func TestAlerts_NoOwnerRouteIsNotGuessedAndTheAlertIsListedInactive(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixture(t, alertsMode(config.ProactiveModeLive))
	alertTool(t, f, tools.NameSetPriceAlert).Execute(context.Background(), map[string]any{
		"action": "create", "provider": "okx", "symbol": "SOL/USDT", "condition": "below", "threshold": 100.0})

	logs := &countLogs{}
	logger.SetAdditionalWriter(logs)
	t.Cleanup(func() { logger.SetAdditionalWriter(nil) })
	if err := f.reload(compose(enabled, withPerception, withTriggers(config.ProactiveModeLive))); err != nil { // no owner
		t.Fatal(err)
	}
	if got := standIns(f); len(got) != 0 {
		t.Fatalf("a stand-in was made without an owner route: %v", got)
	}
	if n := logs.count("alert(s) made while triggers were live are inactive"); n != 1 {
		t.Fatalf("%d warnings at start, want exactly 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "proactive.owner") {
		t.Fatal("the warning does not say how to fix it")
	}
	if s := mustSpecs(t, f)[0]; s.Mode != triggers.ModeLive {
		t.Fatalf("spec = %+v: it is left as it is", s)
	}
	list := alertTool(t, f, tools.NameSetPriceAlert).Execute(context.Background(), map[string]any{"action": "list"}).ForLLM
	if !strings.Contains(list, "inactive") || !strings.Contains(list, "SOL/USDT") {
		t.Fatalf("list = %s", list)
	}
}

func mustSpecs(t *testing.T, f *assemblyFixture) []triggers.Spec {
	t.Helper()
	specs, err := triggers.ReadSpecs(context.Background(), fixtureDB(t, f), triggers.ListFilter{Origin: "tool:"})
	if err != nil {
		t.Fatal(err)
	}
	return specs
}
