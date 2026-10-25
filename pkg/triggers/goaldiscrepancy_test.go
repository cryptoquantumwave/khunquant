package triggers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/deltaneutral/planhealth"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

const goalMarker = "ZXQ-GOAL-MARKER-7731"

// goalRig replays events through a real world model and a real registry, with a fake
// clock, and commits every candidate the way the engine does after attention decided.
type goalRig struct {
	t     *testing.T
	e     *env
	store *worldmodel.Store
	clk   *perceptiontest.Clock
	stats *GoalStats
}

func newGoalRig(t *testing.T) *goalRig {
	t.Helper()
	g := &goalRig{t: t, clk: perceptiontest.NewClock(noon.UTC()), stats: &GoalStats{}}
	var err error
	g.store, err = worldmodel.NewStore(context.Background(), nil, g.clk, worldmodel.WithZone(func() *time.Location { return bkk }))
	if err != nil {
		t.Fatal(err)
	}
	cfg := GoalConfig{Stats: g.stats, Providers: func() []string { return []string{"okx"} }, Location: func() *time.Location { return bkk }}
	g.e = newEnv(t, map[string]Factory{KindGoalDiscrepancy: GoalDiscrepancyFactory(cfg)})
	return g
}

func (g *goalRig) addGoal(id string, p GoalParams) {
	g.t.Helper()
	if _, err := g.e.r.Add(context.Background(), Spec{
		ID: id, Kind: KindGoalDiscrepancy, Mode: ModeLive, Origin: IntentOrigin(id), IntentID: id, Params: p.JSON(),
	}); err != nil {
		g.t.Fatalf("Add: %v", err)
	}
}

func planFunding(apy float64, dataOK bool) map[string]float64 {
	return planhealth.Health{FundingAPYPct: apy, Score: 80, IntervalSeconds: 600, DataOK: dataOK}.Encode()
}

// feed applies an event, evaluates it and, unless commit is false, commits.
func (g *goalRig) feed(ev events.Event, commit bool) []proactive.Candidate {
	g.t.Helper()
	if d := ev.Time.Sub(g.clk.Now()); d > 0 {
		g.clk.Advance(d)
	}
	diff, err := g.store.Apply(context.Background(), ev)
	if err != nil {
		g.t.Fatalf("Apply: %v", err)
	}
	cs, err := g.e.r.Evaluate(context.Background(), g.store.View(), diff)
	if err != nil {
		g.t.Fatalf("Evaluate: %v", err)
	}
	if commit {
		g.e.r.Committed(context.Background(), idsOf(cs))
	}
	return cs
}

func planEv(at time.Time, health map[string]float64) events.Event {
	return events.Event{
		ID: "plan", Kind: events.KindPerceptionPlanHealth, Time: at,
		Source:  events.Source{Component: "perception", Name: "plans"},
		Payload: worldmodel.PlanPayload{Kind: planhealth.Kind, ID: 12, Status: "active", Health: health, At: at},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}
}

// step moves the clock forward one window (ten minutes) and reports a funding APY.
func (g *goalRig) step(apy float64) []proactive.Candidate {
	g.t.Helper()
	return g.stepWith(apy, true, true)
}

func (g *goalRig) stepWith(apy float64, dataOK, commit bool) []proactive.Candidate {
	g.t.Helper()
	return g.feed(planEv(g.clk.Now().Add(10*time.Minute), planFunding(apy, dataOK)), commit)
}

const fundingAbove10 = `plan_funding_apy(12) > 10`

func counts(g *goalRig, series ...float64) (fired []int) {
	for i, apy := range series {
		if len(g.step(apy)) > 0 {
			fired = append(fired, i)
		}
	}
	return fired
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGoalDiscrepancy_RequiresNConsecutive(t *testing.T) {
	for _, c := range []struct {
		name   string
		n      int
		series []float64
		want   []int
	}{
		{"default is two", 0, []float64{12, 3, 3, 3, 3}, []int{2}},
		{"one false check is not enough", 0, []float64{12, 3, 12, 3, 12}, nil},
		{"a true check resets the run", 0, []float64{12, 3, 12, 3, 3}, []int{4}},
		{"three", 3, []float64{12, 3, 3, 3}, []int{3}},
		{"three, two are not enough", 3, []float64{12, 3, 3}, nil},
		{"one fires at once", 1, []float64{12, 3}, []int{1}},
		{"never seen to hold: nothing to break", 0, []float64{3, 3, 3, 3}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newGoalRig(t)
			g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10, Consecutive: c.n})
			if got := counts(g, c.series...); !eq(got, c.want) {
				t.Fatalf("fired at steps %v, want %v", got, c.want)
			}
		})
	}
}

func TestGoalDiscrepancy_RearmsWhenTrue(t *testing.T) {
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	// fires once, stays quiet while false, and fires again only after it held again
	got := counts(g, 12, 3, 3, 3, 3, 3, 12, 3, 3, 3)
	if want := []int{2, 8}; !eq(got, want) {
		t.Fatalf("fired at steps %v, want %v", got, want)
	}
	if g.stats.Rearmed.Load() != 1 {
		t.Fatalf("rearmed = %d", g.stats.Rearmed.Load())
	}
}

// The acceptance criterion: a replayed funding series that drops under the plan's
// assumption for two evaluation windows gives exactly one candidate.
func TestGoalDiscrepancy_ReplayedFundingSeriesGivesOneCandidate(t *testing.T) {
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10, Description: "dn plan assumed funding above 10"})
	series := []float64{14, 13, 12, 11, 3.9, 3.8, 3.7, 3.6}
	var all []proactive.Candidate
	for _, apy := range series {
		all = append(all, g.step(apy)...)
	}
	if len(all) != 1 {
		t.Fatalf("got %d candidates, want exactly one: %v", len(all), keys(all))
	}
	c := all[0]
	if c.IntentID != "01HGOAL" || c.DedupKey != "goal_discrepancy:01HGOAL:goal" || c.DedupWindow != 24*time.Hour {
		t.Fatalf("candidate: intent %q key %q window %s", c.IntentID, c.DedupKey, c.DedupWindow)
	}
	ev, _ := json.Marshal(c.Evidence)
	for _, want := range []string{`"intent_id":"01HGOAL"`, `"kind":"goal_discrepancy"`, `"consecutive_false":2`, `"plan_funding_apy(12)":3.8`} {
		if !strings.Contains(string(ev), want) {
			t.Errorf("evidence %s lacks %s", ev, want)
		}
	}
}

func TestGoalDiscrepancy_UnknownNeitherCountsNorRearms(t *testing.T) {
	// an unknown check in the middle of a run does not break it
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	steps := []struct {
		apy   float64
		ok    bool
		fires bool
	}{{12, true, false}, {3, true, false}, {0, false, false}, {3, true, true}}
	for i, s := range steps {
		if got := len(g.stepWith(s.apy, s.ok, true)) > 0; got != s.fires {
			t.Fatalf("step %d: fired = %v, want %v", i, got, s.fires)
		}
	}

	// and an unknown check after a firing does not re-arm it
	g = newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	for _, apy := range []float64{12, 3, 3} {
		g.step(apy)
	}
	for i := 0; i < 3; i++ {
		if cs := g.stepWith(3, false, true); len(cs) != 0 {
			t.Fatalf("an unknown check fired")
		}
	}
	if cs := g.step(3); len(cs) != 0 {
		t.Fatal("it fired again without having held")
	}
	if g.stats.Unknown.Load() != 3 {
		t.Fatalf("unknown = %d, want 3", g.stats.Unknown.Load())
	}

	// a goal that starts out unknown never arms
	g = newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	for i := 0; i < 4; i++ {
		if cs := g.stepWith(3, false, true); len(cs) != 0 {
			t.Fatal("fired on unknown input")
		}
	}
}

func TestGoalDiscrepancy_ChecksAreSpacedByTheWindow(t *testing.T) {
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	g.step(12)
	// five reports a minute apart are one check: the window has not passed
	at := g.clk.Now()
	for i := 1; i <= 5; i++ {
		if cs := g.feed(planEv(at.Add(time.Duration(i)*time.Minute), planFunding(3, true)), true); len(cs) != 0 {
			t.Fatalf("report %d inside the window fired", i)
		}
	}
	if n := g.stats.Checked.Load(); n != 1 {
		t.Fatalf("checked %d times, want 1: the reports inside the window were judged", n)
	}
	// a goal with a longer window waits that long
	g = newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10, EveryMinutes: 60})
	if got := counts(g, 12, 3, 3, 3, 3, 3, 3, 3); len(got) != 0 {
		t.Fatalf("ten-minute steps fired a one-hour goal at %v", got)
	}
}

// What a firing changes in the trigger's state is applied only once attention has
// decided: a candidate that was not committed is made again at the next check.
func TestGoalDiscrepancy_StateIsCommittedAtDecision(t *testing.T) {
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	g.step(12)
	g.step(3)
	if cs := g.stepWith(3, true, false); len(cs) != 1 {
		t.Fatalf("second false check gave %d candidates", len(cs))
	}
	if cs := g.stepWith(3, true, true); len(cs) != 1 {
		t.Fatalf("the uncommitted candidate was not made again: %d", len(cs))
	}
	if cs := g.step(3); len(cs) != 0 {
		t.Fatal("it fired after its firing was committed")
	}
}

func TestGoalDiscrepancy_StateSurvivesARestart(t *testing.T) {
	g := newGoalRig(t)
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	g.step(12)
	g.step(3)
	// a new registry over the same database, as a restart makes
	cfg := GoalConfig{Stats: g.stats}
	g.e.r = g.e.registry(map[string]Factory{KindGoalDiscrepancy: GoalDiscrepancyFactory(cfg)})
	if cs := g.step(3); len(cs) != 1 {
		t.Fatalf("the run was forgotten by the restart: %d candidates", len(cs))
	}
}

func TestGoalDiscrepancy_CandidateShape(t *testing.T) {
	for _, sev := range []struct {
		in   string
		want proactive.Severity
	}{{"", proactive.SeverityNotice}, {"info", proactive.SeverityInfo}, {"warning", proactive.SeverityWarning}} {
		g := newGoalRig(t)
		g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10, Severity: sev.in, Title: "dn plan", Description: "assumed above 10"})
		var c proactive.Candidate
		for _, apy := range []float64{12, 3, 3} {
			if cs := g.step(apy); len(cs) == 1 {
				c = cs[0]
			}
		}
		if c.DedupKey == "" {
			t.Fatalf("severity %q: nothing fired", sev.in)
		}
		if c.Severity != sev.want || c.Protective || c.OwnerRequested || c.Compose != proactive.ComposeLLM {
			t.Errorf("severity %q: %+v", sev.in, c)
		}
		if c.Quote != "dn plan\nassumed above 10" {
			t.Errorf("Quote = %q", c.Quote)
		}
		if err := checkCandidate(KindGoalDiscrepancy, c); err != nil {
			t.Errorf("malformed: %v", err)
		}
	}
	// the registry's rule is why the key is not goal:<id>
	if err := checkCandidate(KindGoalDiscrepancy, proactive.Candidate{DedupKey: "goal:01H", Severity: proactive.SeverityNotice, Title: "t"}); err == nil {
		t.Error("goal:<id> passes the registry's key rule")
	}
}

func TestGoalDiscrepancy_TextFromTheOwnerIsOnlyInTheQuote(t *testing.T) {
	g := newGoalRig(t)
	// the marker is in the title, the description and a string literal of the expression
	g.addGoal("01HGOAL", GoalParams{
		Expr:  `"` + goalMarker + `" == "` + goalMarker + `" and plan_funding_apy(12) > 10`,
		Title: "title " + goalMarker, Description: "description " + goalMarker,
	})
	var c proactive.Candidate
	for _, apy := range []float64{12, 3, 3} {
		if cs := g.step(apy); len(cs) == 1 {
			c = cs[0]
		}
	}
	if !strings.Contains(c.Quote, goalMarker) {
		t.Fatalf("the Quote lost the goal's words: %q", c.Quote)
	}
	whole := c
	whole.Quote = ""
	b, err := json.Marshal(whole)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), goalMarker) {
		t.Fatalf("the marker is outside the Quote: %s", b)
	}
	for name, s := range map[string]string{"Title": c.Title, "Subject": c.Subject, "Detail": c.Detail, "DedupKey": c.DedupKey, "TriggerID": c.TriggerID} {
		if strings.Contains(s, goalMarker) {
			t.Errorf("%s holds the marker: %q", name, s)
		}
	}
}

// The expression is judged at its own value: the Thai wording, and the figures, are code.
func TestGoalDiscrepancy_Thai(t *testing.T) {
	g := newGoalRig(t)
	cfg := GoalConfig{Language: func() string { return "th" }, Providers: func() []string { return nil }}
	g.e.r = g.e.registry(map[string]Factory{KindGoalDiscrepancy: GoalDiscrepancyFactory(cfg)})
	g.addGoal("01HGOAL", GoalParams{Expr: fundingAbove10})
	var c proactive.Candidate
	for _, apy := range []float64{12, 3, 3} {
		if cs := g.step(apy); len(cs) == 1 {
			c = cs[0]
		}
	}
	if !strings.Contains(c.Title, "เป้าหมาย") || !strings.Contains(c.Detail, "plan_funding_apy(12) = 3") {
		t.Fatalf("title %q detail %q", c.Title, c.Detail)
	}
}

func TestGoalParams_Validate(t *testing.T) {
	bad := map[string]GoalParams{
		"empty":            {},
		"not a test":       {Expr: `price("BTC/USDT") + 1`},
		"unknown function": {Expr: `fetch("x") > 1`},
		"critical":         {Expr: fundingAbove10, Severity: "critical"},
		"nonsense":         {Expr: fundingAbove10, Severity: "loud"},
		"too many":         {Expr: fundingAbove10, Consecutive: 11},
		"negative":         {Expr: fundingAbove10, Consecutive: -1},
		"too slow":         {Expr: fundingAbove10, EveryMinutes: 1441},
		"too long":         {Expr: strings.Repeat("now() > 0 and ", 50) + "true"},
	}
	for name, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := GoalDiscrepancyFactory(GoalConfig{})(Spec{ID: "x", Kind: KindGoalDiscrepancy, Params: p.JSON()}); err == nil {
			t.Errorf("%s: the factory built it", name)
		}
	}
	if err := (GoalParams{Expr: fundingAbove10, Severity: "Warning", Consecutive: 10, EveryMinutes: 1440}).Validate(); err != nil {
		t.Errorf("the bounds were refused: %v", err)
	}
}

// The world model carries a plan's health as numbers under the keys planhealth
// encodes; the expectation language reads them under its own copies of the names.
func TestExprPlanKeysMatchPlanHealth(t *testing.T) {
	m := planhealth.Health{FundingAPYPct: 7.25, Score: 66, IntervalSeconds: 300, DataOK: true}.Encode()
	for key, want := range map[string]float64{
		worldmodel.ExprPlanKeyAPY: 7.25, worldmodel.ExprPlanKeyScore: 66, worldmodel.ExprPlanKeyInterval: 300, worldmodel.ExprPlanKeyDataOK: 1,
	} {
		if got, ok := m[key]; !ok || got != want {
			t.Errorf("planhealth encodes %q as %v (present %v), the expectation language reads %v", key, got, ok, want)
		}
	}
	if worldmodel.ExprPlanKind != planhealth.Kind {
		t.Errorf("plan kind %q != %q", worldmodel.ExprPlanKind, planhealth.Kind)
	}
}

func TestGoalDiscrepancy_IsNotAnOwnerAlert(t *testing.T) {
	if OwnerAlertSpec(KindGoalDiscrepancy, IntentOrigin("01HGOAL")) || OwnerAlertSpec(KindGoalDiscrepancy, IntentOrderOrigin("01HGOAL")) {
		t.Fatal("a goal's discrepancy may break quiet hours")
	}
}
