package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/cron"
	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// seedDNPlanAndJob stores an active plan in the workspace the fixture's gateway will
// open, and a legacy "dn:" monitor job for it in the cron store, before the gateway is
// built. The providers are names no factory is registered for: a check of this plan
// reaches no exchange.
func seedDNPlanAndJob(t *testing.T, f *assemblyFixture) (planID int64, jobID string) {
	t.Helper()
	ws := f.initial.WorkspacePath()
	store, err := deltaneutral.NewStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	planID, err = store.SavePlan(context.Background(), &deltaneutral.Plan{
		Name: "hedge", Asset: "CHZ", Status: deltaneutral.PlanStatusActive, Mode: deltaneutral.ExecutionModeApproval,
		SpotProvider: "dn-test-spot", SpotSymbol: "CHZ/USDT", SpotSide: "buy",
		FuturesProvider: "dn-test-fut", FuturesSymbol: "CHZ/USDT:USDT", FuturesSide: "short",
		FuturesLeverage: 2, FuturesMarginMode: "cross", CapitalUSDT: 100, SpotNotionalUSDT: 44, FuturesNotionalUSDT: 44,
		MonitorInterval: "5m", Enabled: true, RiskPolicy: deltaneutral.DefaultRiskPolicy(), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cs := cron.NewCronService(filepath.Join(ws, "cron", "jobs.json"), nil)
	every := int64(5 * 60 * 1000)
	j, err := cs.AddJob("dn:"+itoa(planID)+":hedge", cron.CronSchedule{Kind: "every", EveryMS: &every},
		"Check the plan.", false, "telegram", "1001")
	if err != nil {
		t.Fatal(err)
	}
	return planID, j.ID
}

func TestDN_DefaultConfigRunsNothingNew(t *testing.T) {
	f := newAssemblyFixtureNoBuild(t, nil)
	_, job := seedDNPlanAndJob(t, f)
	f.build(f.initial)

	if f.services.Perception != nil || triggersTable(t, f) {
		t.Fatal("perception or the triggers table exists")
	}
	if exists, enabled, marked := jobState(f, job); !exists || !enabled || marked {
		t.Fatalf("the legacy job was touched: %v %v %v", exists, enabled, marked)
	}
	if n := goroutinesIn("DNPlans"); n != 0 {
		t.Fatalf("%d plan-health goroutines", n)
	}
}

func TestDN_ShadowImportsAndTheLegacyJobKeepsDelivering(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeShadow))
	_, job := seedDNPlanAndJob(t, f)
	f.build(f.initial)

	if got := specModes(t, f)["migration:cron:"+job]; got != triggers.ModeShadow {
		t.Fatalf("spec = %q, want shadow", got)
	}
	if exists, enabled, marked := jobState(f, job); !exists || !enabled || marked {
		t.Fatalf("shadow must leave the job delivering: %v %v %v", exists, enabled, marked)
	}
	rt := f.services.Perception
	if rt == nil || rt.dnPlans == nil {
		t.Fatal("the plan-health source is not running although a dn_health spec exists")
	}
	// The registry demands the plan, so the source reads it and nothing else.
	var demanded bool
	for _, d := range rt.triggers.Demands() {
		demanded = demanded || (d.Provider == "dn" && d.Symbol == itoa(1) && d.Kind == perception.DemandPlanHealth)
	}
	if !demanded {
		t.Fatalf("demands = %+v", rt.triggers.Demands())
	}
}

func TestDN_LivePromotesAndRollsBackOnReload_AndWithoutARegistry(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeLive))
	_, job := seedDNPlanAndJob(t, f)
	f.build(f.initial)

	if got := specModes(t, f)["migration:cron:"+job]; got != triggers.ModeLive {
		t.Fatalf("spec = %q, want live", got)
	}
	if exists, enabled, marked := jobState(f, job); !exists || enabled || !marked {
		t.Fatalf("promoted job: %v %v %v, want exists, disabled, marked", exists, enabled, marked)
	}

	if err := f.reload(alertsMode(config.ProactiveModeShadow)); err != nil {
		t.Fatal(err)
	}
	if exists, enabled, marked := jobState(f, job); !exists || !enabled || marked {
		t.Fatalf("after the rollback: %v %v %v, want enabled and unmarked", exists, enabled, marked)
	}
	if got := specModes(t, f)["migration:cron:"+job]; got != triggers.ModeShadow {
		t.Fatalf("after the rollback the spec = %q", got)
	}

	if err := f.reload(alertsMode(config.ProactiveModeLive)); err != nil {
		t.Fatal(err)
	}
	if _, enabled, marked := jobState(f, job); enabled || !marked {
		t.Fatal("not promoted again")
	}
	// A generation with no registry at all hands the job back: nothing else would watch the plan.
	if err := f.reload(compose(liveWithPerception(), withTriggers(config.ProactiveModeOff))); err != nil {
		t.Fatal(err)
	}
	if f.services.Perception != nil && f.services.Perception.triggers != nil {
		t.Fatal("a registry was built")
	}
	if exists, enabled, marked := jobState(f, job); !exists || !enabled || marked {
		t.Fatalf("the job was left off with nothing to watch the plan: %v %v %v", exists, enabled, marked)
	}
}

// Live is not enough to switch the legacy job off: the registry must be live NOW. With
// attention in shadow the configuration is downgraded, the registry is shadow, and the
// job keeps delivering.
func TestDN_ALiveConfigDowngradedToShadowNeverSwitchesTheJobOff(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, compose(liveWithPerception(), withAttentionMode(config.ProactiveModeShadow),
		withTriggers(config.ProactiveModeLive)))
	_, job := seedDNPlanAndJob(t, f)
	f.build(f.initial)

	if f.services.Perception == nil || f.services.Perception.triggers == nil {
		t.Skip("no registry in this generation")
	}
	if got := f.services.Perception.triggers.GlobalMode(); got == triggers.ModeLive {
		t.Fatalf("the registry is live although attention is not: %s", got)
	}
	if exists, enabled, marked := jobState(f, job); !exists || !enabled || marked {
		t.Fatalf("the job was switched off while nothing delivers: %v %v %v", exists, enabled, marked)
	}
}

func TestDN_AJobForAPlanThatDoesNotExistIsLeftAlone(t *testing.T) {
	useSources(t, false, &countingSource{name: "fake"})
	f := newAssemblyFixtureNoBuild(t, alertsMode(config.ProactiveModeLive))
	ws := f.initial.WorkspacePath()
	cs := cron.NewCronService(filepath.Join(ws, "cron", "jobs.json"), nil)
	every := int64(5 * 60 * 1000)
	j, err := cs.AddJob("dn:4242:ghost", cron.CronSchedule{Kind: "every", EveryMS: &every}, "x", false, "telegram", "1001")
	if err != nil {
		t.Fatal(err)
	}
	f.build(f.initial)
	if exists, enabled, marked := jobState(f, j.ID); !exists || !enabled || marked {
		t.Fatalf("%v %v %v", exists, enabled, marked)
	}
	for origin := range specModes(t, f) {
		if strings.HasPrefix(origin, "migration:cron:") {
			t.Fatalf("a spec was made for a plan that does not exist: %v", specModes(t, f))
		}
	}
}

// ---- the audit rows --------------------------------------------------------------

type auditStore struct {
	mu     sync.Mutex
	alerts []deltaneutral.Alert
	snapID int64
	err    error
}

func (s *auditStore) SaveAlert(_ context.Context, a *deltaneutral.Alert) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.alerts = append(s.alerts, *a)
	return int64(len(s.alerts)), nil
}
func (s *auditStore) LatestSnapshot(context.Context, int64) (*deltaneutral.MonitorSnapshot, error) {
	if s.snapID == 0 {
		return nil, nil
	}
	return &deltaneutral.MonitorSnapshot{ID: s.snapID}, nil
}

func dnDecision(key, title string) attention.Decision {
	return attention.Decision{ID: "d-" + key, At: time.Unix(1_700_000_000, 0).UTC(),
		Candidate: proactive.Candidate{DedupKey: key, Title: title, Severity: proactive.SeverityCritical}}
}

func TestDNAudit_WritesOneRowPerDeliveredDecisionWithTheRealChannelAndAgentFlag(t *testing.T) {
	store := &auditStore{snapID: 77}
	obs := dnAuditObserver(store, func() (string, string) { return "telegram", "1001" }, func() time.Time { return time.Unix(1_700_000_100, 0) })

	// A composed group of a Warning and a Notice, and one price alert that is not ours.
	obs(attention.Outcome{Delivered: true, Composed: true, Message: attention.Message{Decisions: []attention.Decision{
		dnDecision("dn_health:12:delta_drift_high", "Delta-neutral plan 12: delta drift high"),
		dnDecision("dn_health:12:profit_target_met", "Delta-neutral plan 12: profit target met"),
		dnDecision("price_cross:okx:BTC/USDT:above:60000", "BTC crossed"),
	}}})
	// A template Critical, and one that was not delivered.
	obs(attention.Outcome{Delivered: true, Message: attention.Message{Decisions: []attention.Decision{
		dnDecision("dn_health:12:margin_critical", "Delta-neutral plan 12: margin critical"),
	}}})
	obs(attention.Outcome{Delivered: false, Message: attention.Message{Decisions: []attention.Decision{
		dnDecision("dn_health:12:liquidation_distance_low", "never sent"),
	}}})

	if len(store.alerts) != 3 {
		t.Fatalf("%d rows, want 3: %+v", len(store.alerts), store.alerts)
	}
	byCode := map[string]deltaneutral.Alert{}
	for _, a := range store.alerts {
		byCode[a.Code] = a
	}
	d := byCode["delta_drift_high"]
	if d.PlanID != 12 || d.Severity != "danger" || !d.AgentInvoked || d.DeliveredChannel != "telegram" || d.DeliveredChatID != "1001" ||
		d.SnapshotID == nil || *d.SnapshotID != 77 || d.Message != "Delta-neutral plan 12: delta drift high" || d.RecommendedAction == "" {
		t.Fatalf("row = %+v", d)
	}
	if p := byCode["profit_target_met"]; p.Severity != "warn" || !p.AgentInvoked {
		t.Fatalf("row = %+v", p)
	}
	if c := byCode["margin_critical"]; c.Severity != "critical" || c.AgentInvoked {
		t.Fatalf("a template Critical must record AgentInvoked=false: %+v", c)
	}
}

func TestDNAudit_AFailingStoreOrAJunkKeyIsHarmless(t *testing.T) {
	store := &auditStore{err: errors.New("disk full")}
	obs := dnAuditObserver(store, func() (string, string) { return "", "" }, time.Now)
	obs(attention.Outcome{Delivered: true, Message: attention.Message{Decisions: []attention.Decision{
		dnDecision("dn_health:12:margin_critical", "x"),
		dnDecision("dn_health:abc:margin_critical", "x"),
		dnDecision("dn_health:12:not_a_code", "x"),
		dnDecision("dn_health:12", "x"),
		dnDecision("dn_health:-3:margin_critical", "x"),
	}}})
	if len(store.alerts) != 0 {
		t.Fatalf("rows = %+v", store.alerts)
	}
	if id, code, ok := parseDNKey("dn_health:5:funding_reversal"); !ok || id != 5 || code != "funding_reversal" {
		t.Fatalf("parse = %v %v %v", id, code, ok)
	}
}
