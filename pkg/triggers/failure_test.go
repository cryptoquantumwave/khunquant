package triggers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func TestRegistry_AutoDisableAfterErrors(t *testing.T) {
	e := newEnv(t, tickFactory(failsWith(errors.New(secretError))))
	e.add("bad", testKind, ModeLive, `{"symbols":["BTC/USDT"]}`)
	trig := func() *scripted { return e.r.snap.Load().byID["bad"].trig.(*scripted) }

	var notices []proactive.Candidate
	for i := 1; i <= 15; i++ {
		got := e.eval(kindTick)
		notices = append(notices, got...)
		if i < MaxConsecutiveErrors && len(got) != 0 {
			t.Fatalf("event %d produced %+v before the limit", i, got)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("%d notices, want exactly one: %+v", len(notices), notices)
	}
	n := notices[0]
	if n.Severity != proactive.SeverityNotice || n.Protective || n.Shadow || n.Compose != proactive.ComposeTemplate {
		t.Fatalf("notice = %+v", n)
	}
	if n.TriggerID != testKind+":bad" || n.DedupKey != "triggers:bad:auto_disabled" {
		t.Fatalf("TriggerID %q, DedupKey %q", n.TriggerID, n.DedupKey)
	}
	if err := checkCandidate("triggers", n); err != nil {
		t.Fatalf("the notice does not follow the convention: %v", err)
	}
	if !strings.Contains(n.Title, testKind) || !strings.Contains(n.Title, "bad") {
		t.Fatalf("title %q names neither the kind nor the ID", n.Title)
	}
	if text := fmt.Sprint(n.Title, n.Evidence, n.Subject); strings.Contains(text, secretError) {
		t.Fatalf("the notice carries the error text: %q", text)
	}
	// It stopped being evaluated at the tenth error.
	if c := trig().calls.Load(); c != MaxConsecutiveErrors {
		t.Fatalf("evaluated %d times, want %d", c, MaxConsecutiveErrors)
	}
	if mode, _, errs := e.row("bad"); mode != ModeOff || errs != MaxConsecutiveErrors {
		t.Fatalf("row: mode %q errors %d", mode, errs)
	}
	if d := e.r.Demands(); len(d) != 0 {
		t.Fatalf("a switched-off spec still demands: %v", d)
	}
	if st := e.r.Stats(); st.AutoDisabled != 1 || st.Errors != MaxConsecutiveErrors {
		t.Fatalf("stats = %+v", st)
	}

	// A restart does not bring it back, nor send a second notice.
	e.r = e.registry(tickFactory(failsWith(errors.New(secretError))))
	for i := 0; i < 5; i++ {
		if got := e.eval(kindTick); len(got) != 0 {
			t.Fatalf("after a restart: %+v", got)
		}
	}
	// Turning it back on starts the count again.
	specs, _ := e.r.List(context.Background(), ListFilter{})
	specs[0].Mode = ModeLive
	if err := e.r.Update(context.Background(), specs[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, errs := e.row("bad"); errs != 0 {
		t.Fatalf("errors after re-enabling = %d", errs)
	}
	for i := 1; i < MaxConsecutiveErrors; i++ {
		if got := e.eval(kindTick); len(got) != 0 {
			t.Fatalf("re-enabled, event %d: %+v", i, got)
		}
	}
}

// A shadow spec's notice is shadow too: only a live spec under a live global mode
// speaks.
func TestRegistry_AutoDisableNoticeIsCappedByMode(t *testing.T) {
	for _, tc := range []struct{ global, spec string }{{ModeShadow, ModeLive}, {ModeLive, ModeShadow}} {
		e := newEnv(t, tickFactory(failsWith(errors.New("x"))))
		e.setGlobal(tc.global)
		e.add("bad", testKind, tc.spec, `{}`)
		var got []proactive.Candidate
		for i := 0; i < MaxConsecutiveErrors; i++ {
			got = e.eval(kindTick)
		}
		if len(got) != 1 || !got[0].Shadow {
			t.Fatalf("global %s spec %s: %+v", tc.global, tc.spec, got)
		}
	}
}

func TestRegistry_SuccessResetsTheErrorCounter(t *testing.T) {
	// Fails nine times in a row, succeeds once, fails nine again: never disabled.
	e := newEnv(t, tickFactory(func(call int, _ *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		if call%10 == 0 {
			return nil, nil
		}
		return nil, errors.New("flaky")
	}))
	e.add("flaky", testKind, ModeLive, `{}`)
	for i := 0; i < 50; i++ {
		if got := e.eval(kindTick); len(got) != 0 {
			t.Fatalf("event %d: %+v", i, got)
		}
	}
	if mode, _, _ := e.row("flaky"); mode != ModeLive {
		t.Fatalf("mode = %s", mode)
	}
	if st := e.r.Stats(); st.AutoDisabled != 0 || st.Errors != 45 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRegistry_PanicIsolation(t *testing.T) {
	f := map[string]Factory{
		testKind:  factoryOf(testKind, []events.Kind{kindTick}, func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) { panic(secretError) }, nil),
		otherKind: factoryOf(otherKind, []events.Kind{kindTick}, fires(otherKind), nil),
	}
	e := newEnv(t, f)
	e.add("a", testKind, ModeLive, `{}`)
	e.add("b", otherKind, ModeLive, `{}`)
	e.add("c", testKind, ModeLive, `{}`)

	var all []proactive.Candidate
	for i := 0; i < MaxConsecutiveErrors+3; i++ {
		got := e.eval(kindTick) // never panics
		var fromB int
		for _, c := range got {
			if c.TriggerID == otherKind+":b" {
				fromB++
			}
		}
		if fromB != 1 {
			t.Fatalf("event %d: the healthy trigger produced %d candidates", i, fromB)
		}
		all = append(all, got...)
	}
	var notices []proactive.Candidate
	for _, c := range all {
		if strings.HasPrefix(c.DedupKey, "triggers:") {
			notices = append(notices, c)
		}
	}
	if len(notices) != 2 { // one for a, one for c
		t.Fatalf("%d notices, want 2: %+v", len(notices), notices)
	}
	for _, n := range notices {
		if strings.Contains(fmt.Sprint(n), secretError) {
			t.Fatalf("a notice carries the panic value: %+v", n)
		}
	}
	if st := e.r.Stats(); st.Panics != 2*MaxConsecutiveErrors {
		t.Fatalf("panics = %d", st.Panics)
	}
	for _, id := range []string{"a", "c"} {
		if mode, _, _ := e.row(id); mode != ModeOff {
			t.Fatalf("%s mode = %s", id, mode)
		}
	}
	if mode, _, _ := e.row("b"); mode != ModeLive {
		t.Fatalf("the healthy trigger was switched off")
	}
}

// If attention does not record the notice's decision, the switch-off does not
// happen and the next error asks for the notice again: at-least-once, with
// attention's dedup absorbing a repeat.
func TestRegistry_DisableWaitsForTheNoticeToBeDecided(t *testing.T) {
	e := newEnv(t, tickFactory(failsWith(errors.New("x"))))
	e.add("bad", testKind, ModeLive, `{}`)
	ctx := context.Background()
	for i := 0; i < MaxConsecutiveErrors-1; i++ {
		e.eval(kindTick)
	}
	notice := e.evalUncommitted(kindTick) // the tenth error
	if len(notice) != 1 {
		t.Fatalf("notice = %+v", notice)
	}
	e.r.Committed(ctx, nil) // attention failed to decide it
	if mode, _, errs := e.row("bad"); mode != ModeLive || errs != MaxConsecutiveErrors-1 {
		t.Fatalf("row after an undecided notice: mode %s errors %d", mode, errs)
	}
	again := e.eval(kindTick) // decided this time
	if len(again) != 1 || again[0].DedupKey != notice[0].DedupKey {
		t.Fatalf("the notice was not asked for again: %+v", again)
	}
	if mode, _, _ := e.row("bad"); mode != ModeOff {
		t.Fatalf("mode = %s, want off", mode)
	}
	if got := e.eval(kindTick); len(got) != 0 {
		t.Fatalf("evaluated after the switch-off: %+v", got)
	}
}

func TestRegistry_CandidateValidation(t *testing.T) {
	good := cand(testKind, "x")
	mut := func(f func(*proactive.Candidate)) proactive.Candidate {
		c := good
		c.Evidence = map[string]any{"price": 1.5}
		f(&c)
		return c
	}
	nan := nanFloat()
	cases := []struct {
		name string
		c    proactive.Candidate
		ok   bool
	}{
		{"good", good, true},
		{"subject with a colon", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + ":BTC/USDT:USDT:below" }), true},
		{"dn style", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + ":12:margin_critical" }), true},
		{"empty key", mut(func(c *proactive.Candidate) { c.DedupKey = "" }), false},
		{"other kind's prefix", mut(func(c *proactive.Candidate) { c.DedupKey = otherKind + ":BTC:x" }), false},
		{"no prefix", mut(func(c *proactive.Candidate) { c.DedupKey = "BTC:x" }), false},
		{"prefix of the kind name only", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + "x:BTC:y" }), false},
		{"no variant", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + ":BTC" }), false},
		{"empty subject", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + "::x" }), false},
		{"empty variant", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + ":BTC:" }), false},
		{"whitespace in key", mut(func(c *proactive.Candidate) { c.DedupKey = testKind + ":BTC USDT:x" }), false},
		{"key too long", mut(func(c *proactive.Candidate) {
			c.DedupKey = testKind + ":" + strings.Repeat("a", MaxDedupKeyBytes) + ":x"
		}), false},
		{"severity too high", mut(func(c *proactive.Candidate) { c.Severity = proactive.Severity(4) }), false},
		{"severity negative", mut(func(c *proactive.Candidate) { c.Severity = proactive.Severity(-1) }), false},
		{"critical", mut(func(c *proactive.Candidate) { c.Severity = proactive.SeverityCritical }), true},
		{"protective critical", mut(func(c *proactive.Candidate) { c.Severity, c.Protective = proactive.SeverityCritical, true }), true},
		{"protective warning", mut(func(c *proactive.Candidate) { c.Severity, c.Protective = proactive.SeverityWarning, true }), false},
		{"protective info", mut(func(c *proactive.Candidate) { c.Severity, c.Protective = proactive.SeverityInfo, true }), false},
		{"empty title", mut(func(c *proactive.Candidate) { c.Title = " " }), false},
		{"long title", mut(func(c *proactive.Candidate) { c.Title = strings.Repeat("t", MaxTitleBytes+1) }), false},
		{"long subject", mut(func(c *proactive.Candidate) { c.Subject = strings.Repeat("s", MaxSubjectBytes+1) }), false},
		{"NaN in evidence", mut(func(c *proactive.Candidate) { c.Evidence = map[string]any{"x": nan} }), false},
		{"nested NaN in evidence", mut(func(c *proactive.Candidate) { c.Evidence = map[string]any{"x": map[string]any{"y": []float64{1, nan}}} }), false},
		{"evidence that cannot be encoded", mut(func(c *proactive.Candidate) { c.Evidence = map[string]any{"x": make(chan int)} }), false},
		{"evidence too big", mut(func(c *proactive.Candidate) { c.Evidence = map[string]any{"x": strings.Repeat("e", MaxEvidenceBytes)} }), false},
		{"NaN salience", mut(func(c *proactive.Candidate) { c.Salience = nan }), false},
		{"salience over 1", mut(func(c *proactive.Candidate) { c.Salience = 1.5 }), false},
		{"negative window", mut(func(c *proactive.Candidate) { c.DedupWindow = -1 }), false},
		{"too many skills", mut(func(c *proactive.Candidate) { c.ForcedSkills = make([]string, MaxForcedSkills+1) }), false},
		{"unknown compose mode", mut(func(c *proactive.Candidate) { c.Compose = proactive.ComposeMode(5) }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.c
			e := newEnv(t, tickFactory(func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
				return []proactive.Candidate{c}, nil
			}))
			e.add("s1", testKind, ModeLive, `{}`)
			got := e.eval(kindTick)
			st := e.r.Stats()
			if tc.ok && (len(got) != 1 || st.Errors != 0) {
				t.Fatalf("a good candidate was refused: %+v %+v", got, st)
			}
			if !tc.ok && (len(got) != 0 || st.Errors != 1 || st.InvalidCandidates != 1) {
				t.Fatalf("a malformed candidate got through: %+v %+v", got, st)
			}
		})
	}
}

// One malformed candidate costs the whole result of that trigger, not the others'.
func TestRegistry_MalformedResultIsAnErrorAndDropsTheOthers(t *testing.T) {
	e := newEnv(t, tickFactory(func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
		bad := cand(testKind, "bad")
		bad.DedupKey = ""
		return []proactive.Candidate{cand(testKind, "ok"), bad}, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	if got := e.eval(kindTick); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestRegistry_TooManyCandidatesIsAnError(t *testing.T) {
	e := newEnv(t, tickFactory(func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
		var cs []proactive.Candidate
		for i := 0; i <= MaxCandidatesPerEvaluation; i++ {
			cs = append(cs, cand(testKind, fmt.Sprintf("v%d", i)))
		}
		return cs, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	if got := e.eval(kindTick); len(got) != 0 || e.r.Stats().InvalidCandidates != 1 {
		t.Fatalf("got %d candidates, stats %+v", len(got), e.r.Stats())
	}
}

func TestRegistry_BadFactoryResults(t *testing.T) {
	ctx := context.Background()
	mk := func(t Trigger, err error) map[string]Factory {
		return map[string]Factory{testKind: func(Spec) (Trigger, error) { return t, err }}
	}
	for name, f := range map[string]map[string]Factory{
		"nil trigger":       mk(nil, nil),
		"wrong id":          mk(&scripted{id: "other:1", kinds: []events.Kind{kindTick}}, nil),
		"no kinds":          mk(&scripted{id: testKind + ":s1"}, nil),
		"too many demands":  mk(manyDemands(), nil),
		"factory panics":    {testKind: func(Spec) (Trigger, error) { panic("boom") }},
		"factory errors":    mk(nil, errors.New("no")),
		"negative interval": mk(negativeInterval(), nil),
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, f)
			if _, err := e.r.Add(ctx, Spec{ID: "s1", Kind: testKind, Mode: ModeLive, Origin: "t"}); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("Add = %v, want ErrInvalidSpec", err)
			}
			if n := e.count(`SELECT COUNT(*) FROM triggers`); n != 0 {
				t.Fatalf("a refused spec was stored")
			}
		})
	}
}
