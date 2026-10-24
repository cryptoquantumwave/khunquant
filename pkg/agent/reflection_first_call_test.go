package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/intent"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/reflection"
	"github.com/cryptoquantumwave/khunquant/pkg/tools"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
)

// The nightly reflection turn is a background turn like the heartbeat, so its first
// model call is held to the same 40,000-byte budget (about 10K tokens at four bytes
// a token), measured the way proactive_first_call_test.go measures it: the JSON of
// the first call's messages plus the JSON of its tool definitions, on the repo's
// default workspace, with the system prompt counted as the request carries it.
//
// pkg/reflection's own test bounds the INPUT (MaxInputBytes). This one bounds the
// whole call: the system prompt as built for this turn, the contract, the input at
// its caps, and the three tool definitions.
const reflectionFirstCallBudget = governedHeartbeatFirstCallBudget

// reflectionDecisionLog is the decision log of a day with far more decisions and
// mutes than the input can show.
type reflectionDecisionLog struct {
	decisions []attention.DayDecision
	mutes     []attention.MuteEvent
}

func (l reflectionDecisionLog) DecisionsBetween(context.Context, time.Time, time.Time, int) ([]attention.DayDecision, error) {
	return l.decisions, nil
}

func (l reflectionDecisionLog) MuteEventsBetween(context.Context, time.Time, time.Time) ([]attention.MuteEvent, error) {
	return l.mutes, nil
}

type reflectionSpecs struct{ specs []triggers.Spec }

func (s reflectionSpecs) List(context.Context, triggers.ListFilter) ([]triggers.Spec, error) {
	return s.specs, nil
}
func (reflectionSpecs) Update(context.Context, triggers.Spec) error { return nil }

type reflectionIntents struct{ its []intent.Intent }

func (i reflectionIntents) ListIntents(context.Context, intent.IntentFilter) ([]intent.Intent, error) {
	return i.its, nil
}

func (reflectionIntents) GetIntent(context.Context, string) (intent.Intent, error) {
	return intent.Intent{}, nil
}

func (reflectionIntents) Transition(context.Context, string, intent.Status, intent.Provenance, string) error {
	return nil
}

type reflectionProfile struct{ p intent.Profile }

func (r reflectionProfile) Profile(context.Context) (intent.Profile, error) { return r.p, nil }

type reflectionSession struct{ msgs []reflection.Message }

func (r reflectionSession) Tail(context.Context, int) ([]reflection.Message, string, error) {
	return r.msgs, "40:x", nil
}

// worstCaseReflectionInput builds the input of a day that fills every section of
// reflection.BuildInput to its cap: many trigger kinds and decisions with the longest
// ids and reasons, mutes, tunable specs, open intents with titles at the limit, and an
// owner conversation of Thai text (three bytes a rune) at the per-message limit.
func worstCaseReflectionInput(t *testing.T) reflection.Input {
	t.Helper()
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	long := func(s string, n int) string { return strings.Repeat(s, n) }

	var log reflectionDecisionLog
	for i := 0; i < attention.MaxRecentDecisions && i < 400; i++ {
		kind := fmt.Sprintf("trigger_kind_%02d", i%24)
		disp := []proactive.Disposition{proactive.DispNotify, proactive.DispDrop, proactive.DispDigest}[i%3]
		log.decisions = append(log.decisions, attention.DayDecision{
			ID: fmt.Sprintf("01JABCDEFGHJKMNPQRSTV%05d", i), At: day.Add(time.Duration(i) * time.Minute),
			TriggerID: kind + ":" + long("x", 64), DedupKey: kind + ":SET:tier1",
			Severity: proactive.SeverityNotice, Disposition: disp, Reason: long("r", 40), Delivered: i%2 == 0,
		})
	}
	for i := 0; i < 30; i++ {
		at := day.Add(time.Duration(i) * time.Minute)
		log.mutes = append(log.mutes, attention.MuteEvent{Key: fmt.Sprintf("move_vs_vol:ASSET%d", i), Created: at, Until: at.Add(time.Hour), Reason: long("r", 100)})
	}

	var specs []triggers.Spec
	for i := 0; i < 40; i++ {
		specs = append(specs, triggers.Spec{ID: fmt.Sprintf("pf-move_vs_vol-%s-%02d", long("s", 30), i), Kind: triggers.KindMoveVsVol,
			Mode: triggers.ModeLive, Origin: triggers.PortfolioSpecOrigin})
	}

	var its []intent.Intent
	for i := 0; i < 15; i++ {
		its = append(its, intent.Intent{ID: fmt.Sprintf("01JABCDEFGHJKMNPQRSTV%05d", i), Type: intent.TypeStandingOrder,
			Status: intent.StatusProposed, Source: intent.SourceInferred, CreatedAt: day.AddDate(0, 0, -100),
			Title: long("ก", 100), FiresUsed: 12, FireBudget: 99})
	}

	prof := intent.DefaultProfile()
	prof.Language = "th"
	prof.RiskTolerance = "conservative"
	prof.AllocationTargets = map[string]float64{"crypto": 30.5, "th_equity": 25.5, "us_equity": 30.5, "cash": 13.5}

	var msgs []reflection.Message
	for i := 0; i < reflection.OwnerMessages; i++ {
		msgs = append(msgs, reflection.Message{Role: []string{"user", "assistant"}[i%2], Text: long("สวัสดีเรื่องเงินลงทุน ", 80)})
	}

	in, err := reflection.BuildInput(context.Background(), reflection.Sources{
		Decisions: log, Specs: reflectionSpecs{specs}, Intents: reflectionIntents{its}, Profile: reflectionProfile{prof},
		Session: reflectionSession{msgs},
	}, day, day.Add(23*time.Hour), time.UTC, "")
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// splitReflectionUserMessage splits the reflection turn's user message into the
// contract and the input.
func splitReflectionUserMessage(content, input string) []sizedPart {
	contract := strings.TrimSpace(reflectionContract)
	return []sizedPart{{"contract", len(contract)}, {"input (worst case)", len(input)},
		{"other", len(content) - len(contract) - len(input)}}
}

func TestReflectionTurn_FirstCallWithinBudget(t *testing.T) {
	in := worstCaseReflectionInput(t)
	t.Logf("input %d bytes (limit %d); sections %v", len(in.Text), reflection.MaxInputBytes, in.Sections)
	if len(in.Text) > reflection.MaxInputBytes {
		t.Fatalf("the input is %d bytes, over its own %d byte budget", len(in.Text), reflection.MaxInputBytes)
	}
	// The case is only the worst one if the sections are really full: the caps sum
	// to less than MaxInputBytes, and a day this busy must come close to the sum.
	if len(in.Text) < reflection.MaxInputBytes-1_500 {
		t.Fatalf("the worst-case input is only %d bytes: the fixture no longer fills the sections (%v)", len(in.Text), in.Sections)
	}

	// Ledger on and filled, portfolio populated: whatever financial context the system
	// prompt would carry for a proactive prompt is at its largest (the turn leaves the
	// background sections out, so the first call must come out the same).
	prov := script(callsResponse(proposeReflectionCall("p1")))
	al, _ := defaultWorkspaceLoopWith(t, prov, merge(portfolioFixture(), intentsFullFixture()))
	registerGatewayReadTools(al)
	sched, _ := startScheduler(t, cogCfg(1))
	al.sched.Store(&schedulerHolder{s: sched})

	res, err := al.RunReflectionTurn(context.Background(), in.Text)
	if err != nil {
		t.Fatalf("RunReflectionTurn: %v", err)
	}
	if !res.Called {
		t.Fatal("the scripted model's propose_reflection call did not reach the turn")
	}

	fc := measureFirstCall(t, prov)
	prov.mu.Lock()
	for _, m := range prov.msgs[0] {
		if m.Role == "user" {
			fc.user = splitReflectionUserMessage(m.Content, in.Text)
		}
	}
	prov.mu.Unlock()
	t.Logf("\n%s", fc)
	t.Logf("reflection first call: %d bytes, budget %d, headroom %d", fc.total, reflectionFirstCallBudget, reflectionFirstCallBudget-fc.total)

	// The budget is only honest if the turn was offered exactly its three tools.
	var offered []string
	for _, p := range fc.tools {
		offered = append(offered, p.name)
	}
	sort.Strings(offered)
	want := []string{tools.NameGetProfile, tools.NameListIntents, tools.NameProposeReflection}
	sort.Strings(want)
	if strings.Join(offered, ",") != strings.Join(want, ",") {
		t.Errorf("the turn was offered %v, want %v", offered, want)
	}
	// The measured request must really carry the contract and the whole input.
	for _, p := range fc.user {
		if p.name == "other" && p.bytes != len("\n\n") {
			t.Errorf("the user message holds %d bytes beyond the contract and the input, want the 2-byte separator", p.bytes)
		}
	}
	if fc.total > reflectionFirstCallBudget {
		t.Errorf("the reflection turn's first call is %d bytes, budget %d (about 10K tokens)\n%s",
			fc.total, reflectionFirstCallBudget, fc)
	}
}
