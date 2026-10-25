package triggers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

func tickFactory(fn evalFn) map[string]Factory {
	return map[string]Factory{testKind: factoryOf(testKind, []events.Kind{kindTick}, fn, nil)}
}

func TestRegistry_ModeCapping(t *testing.T) {
	cases := []struct {
		global, spec string
		wantRun      bool
		wantShadow   bool
	}{
		{ModeLive, ModeLive, true, false},
		{ModeLive, ModeShadow, true, true},
		{ModeShadow, ModeLive, true, true},
		{ModeShadow, ModeShadow, true, true},
		{ModeLive, ModeOff, false, false},
		{ModeShadow, ModeOff, false, false},
		{ModeOff, ModeLive, false, false},
		{ModeOff, ModeShadow, false, false},
		{"garbage", ModeLive, false, false}, // a mode that cannot be read is off
		{"", ModeLive, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.global+"/"+tc.spec, func(t *testing.T) {
			e := newEnv(t, tickFactory(fires(testKind)))
			e.setGlobal(tc.global)
			e.add("s1", testKind, tc.spec, `{}`)
			got := e.eval(kindTick)
			if (len(got) == 1) != tc.wantRun {
				t.Fatalf("got %d candidates, want run=%v", len(got), tc.wantRun)
			}
			if tc.wantRun && got[0].Shadow != tc.wantShadow {
				t.Fatalf("Shadow = %v, want %v", got[0].Shadow, tc.wantShadow)
			}
			if !tc.wantRun {
				if n := e.r.snap.Load().byID["s1"].trig.(*scripted).calls.Load(); n != 0 {
					t.Fatalf("an off trigger was evaluated %d times", n)
				}
			}
		})
	}
}

// A trigger cannot make its own candidate deliverable: a Shadow it sets stays set.
func TestRegistry_ShadowFlagSetByTriggerIsKept(t *testing.T) {
	e := newEnv(t, tickFactory(func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
		c := cand(testKind, "x")
		c.Shadow = true
		return []proactive.Candidate{c}, nil
	}))
	e.add("s1", testKind, ModeLive, `{}`)
	if got := e.eval(kindTick); len(got) != 1 || !got[0].Shadow {
		t.Fatalf("got %+v", got)
	}
}

// The global mode is read on every evaluation: a reload applies to a registry that
// was not rebuilt.
func TestRegistry_GlobalModeIsReReadOnEachEvaluation(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	e.add("s1", testKind, ModeLive, `{"symbols":["BTC/USDT"]}`)

	steps := []struct {
		mode       string
		wantN      int
		wantShadow bool
		wantDemand int
	}{
		{ModeLive, 1, false, 1},
		{ModeShadow, 1, true, 1},
		{ModeOff, 0, false, 0},
		{ModeLive, 1, false, 1},
	}
	for i, s := range steps {
		e.setGlobal(s.mode)
		got := e.eval(kindTick)
		if len(got) != s.wantN || (s.wantN == 1 && got[0].Shadow != s.wantShadow) {
			t.Fatalf("step %d (%s): got %+v", i, s.mode, got)
		}
		if n := len(e.r.Demands()); n != s.wantDemand {
			t.Fatalf("step %d (%s): %d demands, want %d", i, s.mode, n, s.wantDemand)
		}
	}
}

func TestRegistry_RoutesByKind(t *testing.T) {
	var built sync.Map
	f := map[string]Factory{
		testKind:  factoryOf(testKind, []events.Kind{kindTick}, fires(testKind), &built),
		otherKind: factoryOf(otherKind, []events.Kind{kindPort, kindTick}, fires(otherKind), &built),
		"clock_x": factoryOf("clock_x", []events.Kind{events.KindPerceptionClockSession}, fires("clock_x"), &built),
	}
	e := newEnv(t, f)
	e.add("a", testKind, ModeLive, `{}`)
	e.add("b", otherKind, ModeLive, `{}`)
	e.add("c", "clock_x", ModeLive, `{}`)
	calls := func(id string) int64 {
		v, _ := built.Load(id)
		return v.(*scripted).calls.Load()
	}

	if got := e.eval(kindTick); len(got) != 2 {
		t.Fatalf("tick: %d candidates, want 2 (a and b)", len(got))
	}
	if got := e.eval(kindPort); len(got) != 1 || got[0].TriggerID != otherKind+":b" {
		t.Fatalf("portfolio: %+v, want only b", got)
	}
	if got := e.eval(events.KindPerceptionDevice); len(got) != 0 {
		t.Fatalf("an unlisted kind reached a trigger: %+v", got)
	}
	if a, b, c := calls("a"), calls("b"), calls("c"); a != 1 || b != 2 || c != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want 1 2 0", a, b, c)
	}
}

func TestRegistry_CandidatesCarryRegistryFields(t *testing.T) {
	e := newEnv(t, tickFactory(func(int, *State, worldmodel.Diff) ([]proactive.Candidate, error) {
		c := cand(testKind, "x")
		c.ID, c.TriggerID, c.IntentID = "mine", "forged:1", "forged-intent"
		c.Trust = proactive.TrustUserAuthored // more than the event's
		return []proactive.Candidate{c}, nil
	}))
	if _, err := e.r.Add(context.Background(), Spec{ID: "s1", Kind: testKind, Mode: ModeLive, Origin: "intent", IntentID: "intent-7"}); err != nil {
		t.Fatal(err)
	}
	got := e.eval(kindTick)
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	c := got[0]
	if c.ID == "mine" || c.ID == "" || c.TriggerID != testKind+":s1" || c.IntentID != "intent-7" {
		t.Fatalf("ID/TriggerID/IntentID = %q %q %q", c.ID, c.TriggerID, c.IntentID)
	}
	if c.Trust != proactive.TrustExchange {
		t.Fatalf("trust %s, want the event's (exchange)", c.Trust)
	}
	if !c.At.Equal(noon) {
		t.Fatalf("At = %v, want the view's time", c.At)
	}
}

func TestRegistry_Demands(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	if d := e.r.Demands(); len(d) != 0 {
		t.Fatalf("empty registry has demands: %v", d)
	}
	e.add("a", testKind, ModeLive, `{"symbols":["ETH/USDT","BTC/USDT"]}`)
	e.add("b", testKind, ModeShadow, `{"symbols":["BTC/USDT","SOL/USDT"]}`)
	e.add("c", testKind, ModeOff, `{"symbols":["DOGE/USDT"]}`)

	var syms []string
	for _, d := range e.r.Demands() {
		syms = append(syms, d.Symbol)
	}
	if want := []string{"BTC/USDT", "ETH/USDT", "SOL/USDT"}; !reflect.DeepEqual(syms, want) {
		t.Fatalf("demands = %v, want %v (de-duplicated, sorted, the off spec left out)", syms, want)
	}

	e.setGlobal(ModeOff)
	if d := e.r.Demands(); len(d) != 0 {
		t.Fatalf("global off still demands: %v", d)
	}
	e.setGlobal(ModeShadow)
	if d := e.r.Demands(); len(d) != 3 {
		t.Fatalf("global shadow demands %d, want 3", len(d))
	}

	// Turning a spec off removes its demand; removing one does too.
	s, _ := e.r.List(context.Background(), ListFilter{Kind: testKind})
	for _, sp := range s {
		if sp.ID == "b" {
			sp.Mode = ModeOff
			if err := e.r.Update(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.r.Remove(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	syms = nil
	for _, d := range e.r.Demands() {
		syms = append(syms, d.Symbol)
	}
	if want := []string{"BTC/USDT", "ETH/USDT"}; !reflect.DeepEqual(syms, want) {
		t.Fatalf("demands = %v, want %v", syms, want)
	}
	var _ perception.DemandSource = e.r
}

// Demands is called from the market source's goroutine while specs change and the
// pipeline evaluates.
func TestRegistry_ConcurrentMutationEvaluationAndDemands(t *testing.T) {
	e := newEnv(t, tickFactory(func(call int, st *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		st.Raw, st.Dirty = json.RawMessage(fmt.Sprintf(`{"n":%d}`, call)), true
		return []proactive.Candidate{cand(testKind, fmt.Sprintf("n%d", call))}, nil
	}))
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = e.r.Demands()
					_, _ = e.r.List(ctx, ListFilter{})
					_ = e.r.Stats()
				}
			}
		}()
	}
	wg.Add(1)
	go func() { // the mutator (a tool in a user turn)
		defer wg.Done()
		for i := 0; i < 30; i++ {
			id := fmt.Sprintf("m%02d", i)
			if _, err := e.r.Add(ctx, Spec{ID: id, Kind: testKind, Mode: ModeLive, Origin: "t",
				Params: json.RawMessage(fmt.Sprintf(`{"symbols":["S%d/USDT"]}`, i))}); err != nil {
				t.Errorf("Add: %v", err)
				return
			}
			if i%3 == 0 {
				_ = e.r.Update(ctx, Spec{ID: id, Kind: testKind, Mode: ModeShadow, Origin: "t2"})
			}
			if i%5 == 0 {
				_ = e.r.Remove(ctx, id)
			}
		}
	}()
	for i := 0; i < 60; i++ { // the pipeline
		cs := e.evalUncommitted(kindTick)
		e.r.Committed(ctx, idsOf(cs))
	}
	close(stop)
	wg.Wait()
}

func TestRegistry_UnknownKindAtLoadIsKeptSkippedAndReportedOnce(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	e.add("known", testKind, ModeLive, `{"symbols":["BTC/USDT"]}`)
	now := e.now.Load().UnixMilli()
	// Rows another version wrote: a kind this build has no factory for, params this
	// build refuses, and a row with a mode this build does not know.
	for _, r := range []struct{ id, kind, params, mode string }{
		{"future", "indicator_cross", `{"x":1}`, ModeLive},
		{"refused", testKind, `{"reject":true}`, ModeLive},
		{"oddmode", testKind, `{}`, "paused"},
	} {
		if _, err := e.db.Exec(`INSERT INTO triggers (id, kind, params_json, mode, origin, state_json, created_ms, updated_ms)
			VALUES (?, ?, ?, ?, 'other', '{"keep":true}', ?, ?)`, r.id, r.kind, r.params, r.mode, now, now); err != nil {
			t.Fatal(err)
		}
	}

	r2 := e.registry(tickFactory(fires(testKind)))
	if st := r2.Stats(); st.Unusable != 3 || st.Specs != 4 {
		t.Fatalf("stats = %+v, want 3 unusable of 4", st)
	}
	specs, _ := r2.List(context.Background(), ListFilter{})
	if len(specs) != 4 {
		t.Fatalf("listed %d specs, want all 4", len(specs))
	}
	// They never run and never demand.
	e.r = r2
	if got := e.eval(kindTick); len(got) != 1 || got[0].TriggerID != testKind+":known" {
		t.Fatalf("got %+v", got)
	}
	if d := r2.Demands(); len(d) != 1 {
		t.Fatalf("demands = %v", d)
	}
	// And they are not deleted, state included.
	if n := e.count(`SELECT COUNT(*) FROM triggers`); n != 4 {
		t.Fatalf("%d rows, want 4", n)
	}
	if _, st, _ := e.row("future"); st != `{"keep":true}` {
		t.Fatalf("state of the skipped spec = %q", st)
	}
	// A build that knows the kind picks the spec up again.
	f := tickFactory(fires(testKind))
	f["indicator_cross"] = factoryOf("indicator_cross", []events.Kind{kindTick}, fires("indicator_cross"), nil)
	r3 := e.registry(f)
	if st := r3.Stats(); st.Unusable != 2 {
		t.Fatalf("with the kind known: %+v", st)
	}
	// An unknown kind cannot be added or updated to.
	if _, err := r2.Add(context.Background(), Spec{Kind: "indicator_cross", Mode: ModeLive, Origin: "t"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Add of an unknown kind: %v", err)
	}
	// Remove, the owner's explicit act, does delete it.
	if err := r2.Remove(context.Background(), "future"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT COUNT(*) FROM triggers`); n != 3 {
		t.Fatalf("%d rows after Remove, want 3", n)
	}
}

func TestRegistry_AddUpdateValidation(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	ctx := context.Background()
	ok := Spec{Kind: testKind, Mode: ModeShadow, Origin: "tool:set_price_alert", Params: json.RawMessage(`{}`)}
	e.add("dup", testKind, ModeLive, `{}`)

	bad := func(name string, mutate func(*Spec), want error) {
		t.Run(name, func(t *testing.T) {
			s := ok
			mutate(&s)
			if _, err := e.r.Add(ctx, s); !errors.Is(err, want) {
				t.Fatalf("Add error = %v, want %v", err, want)
			}
		})
	}
	bad("unknown kind", func(s *Spec) { s.Kind = "nope" }, ErrUnknownKind)
	bad("empty kind", func(s *Spec) { s.Kind = "" }, ErrInvalidSpec)
	bad("upper-case kind", func(s *Spec) { s.Kind = "Price" }, ErrInvalidSpec)
	bad("bad mode", func(s *Spec) { s.Mode = "paused" }, ErrInvalidSpec)
	bad("empty mode", func(s *Spec) { s.Mode = "" }, ErrInvalidSpec)
	bad("empty origin", func(s *Spec) { s.Origin = "  " }, ErrInvalidSpec)
	bad("long origin", func(s *Spec) { s.Origin = strings.Repeat("x", MaxOriginRunes+1) }, ErrInvalidSpec)
	bad("control in origin", func(s *Spec) { s.Origin = "a\nb" }, ErrInvalidSpec)
	bad("params over 4 KB", func(s *Spec) {
		s.Params = json.RawMessage(`{"tag":"` + strings.Repeat("x", MaxParamsBytes) + `"}`)
	}, ErrInvalidSpec)
	bad("params not JSON", func(s *Spec) { s.Params = json.RawMessage(`{nope`) }, ErrInvalidSpec)
	bad("factory refuses params", func(s *Spec) { s.Params = json.RawMessage(`{"reject":true}`) }, ErrInvalidSpec)
	bad("bad id", func(s *Spec) { s.ID = "a:b" }, ErrInvalidSpec)
	bad("duplicate id", func(s *Spec) { s.ID = "dup" }, ErrExists)
	bad("long intent id", func(s *Spec) { s.IntentID = strings.Repeat("i", 65) }, ErrInvalidSpec)

	// Exactly 4 KB is accepted.
	s := ok
	s.Params = json.RawMessage(`{"tag":"` + strings.Repeat("x", MaxParamsBytes-len(`{"tag":""}`)) + `"}`)
	if len(s.Params) != MaxParamsBytes {
		t.Fatalf("test params are %d bytes", len(s.Params))
	}
	if _, err := e.r.Add(ctx, s); err != nil {
		t.Fatalf("4 KB params refused: %v", err)
	}

	// A generated ID is a ULID-shaped token and the stamps are filled in.
	got, err := e.r.Add(ctx, ok)
	if err != nil || got.ID == "" || got.CreatedAt.IsZero() || !got.CreatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("Add = %+v, %v", got, err)
	}

	// Update is validated the same way, and an unknown ID is not found.
	if err := e.r.Update(ctx, Spec{ID: "missing", Kind: testKind, Mode: ModeLive, Origin: "t"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update of a missing spec: %v", err)
	}
	if err := e.r.Update(ctx, Spec{ID: "dup", Kind: testKind, Mode: "bogus", Origin: "t"}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Update with a bad mode: %v", err)
	}
	if err := e.r.Update(ctx, Spec{ID: "dup", Kind: "nope", Mode: ModeLive, Origin: "t"}); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("Update to an unknown kind: %v", err)
	}
	if err := e.r.Remove(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove of a missing spec: %v", err)
	}
}

func TestRegistry_TooManySpecs(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	for i := 0; i < MaxSpecs; i++ {
		e.add(fmt.Sprintf("s%04d", i), testKind, ModeOff, `{}`)
	}
	if _, err := e.r.Add(context.Background(), Spec{Kind: testKind, Mode: ModeOff, Origin: "t"}); !errors.Is(err, ErrTooMany) {
		t.Fatalf("Add over the limit: %v", err)
	}
}

func TestRegistry_ListFilter(t *testing.T) {
	f := tickFactory(fires(testKind))
	f[otherKind] = factoryOf(otherKind, []events.Kind{kindTick}, fires(otherKind), nil)
	e := newEnv(t, f)
	e.add("a", testKind, ModeLive, `{}`)
	e.add("b", otherKind, ModeShadow, `{}`)
	if _, err := e.r.Add(context.Background(), Spec{ID: "c", Kind: testKind, Mode: ModeOff, Origin: "migration:cron:7", IntentID: "i1"}); err != nil {
		t.Fatal(err)
	}
	ids := func(f ListFilter) string {
		ss, _ := e.r.List(context.Background(), f)
		var out []string
		for _, s := range ss {
			out = append(out, s.ID)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		f    ListFilter
		want string
	}{
		{ListFilter{}, "a,b,c"},
		{ListFilter{Kind: testKind}, "a,c"},
		{ListFilter{Mode: ModeShadow}, "b"},
		{ListFilter{Origin: "migration:"}, "c"},
		{ListFilter{IntentID: "i1"}, "c"},
		{ListFilter{Kind: otherKind, Mode: ModeLive}, ""},
	} {
		if got := ids(tc.f); got != tc.want {
			t.Errorf("List(%+v) = %q, want %q", tc.f, got, tc.want)
		}
	}
}

// The same events in the same order give the same candidates: nothing depends on
// map order or the wall clock.
func TestRegistry_Deterministic(t *testing.T) {
	run := func() []proactive.Candidate {
		f := map[string]Factory{
			testKind:  factoryOf(testKind, []events.Kind{kindTick}, fires(testKind), nil),
			otherKind: factoryOf(otherKind, []events.Kind{kindTick}, fires(otherKind), nil),
		}
		e := newEnv(t, f)
		for _, id := range []string{"z", "m", "a", "q"} {
			kind := testKind
			if id == "m" || id == "q" {
				kind = otherKind
			}
			e.add(id, kind, ModeLive, `{}`)
		}
		var out []proactive.Candidate
		for i := 0; i < 5; i++ {
			out = append(out, e.eval(kindTick)...)
		}
		return out
	}
	a, b := run(), run()
	if len(a) != 20 || !reflect.DeepEqual(a, b) {
		t.Fatalf("two runs differ (%d, %d candidates)", len(a), len(b))
	}
	if a[0].TriggerID != testKind+":a" || a[1].TriggerID != otherKind+":m" {
		t.Fatalf("not in spec ID order: %s, %s", a[0].TriggerID, a[1].TriggerID)
	}
}

func TestRegistry_NilViewAndOffGlobalEvaluateNothing(t *testing.T) {
	e := newEnv(t, tickFactory(fires(testKind)))
	e.add("s1", testKind, ModeLive, `{}`)
	if cs, err := e.r.Evaluate(context.Background(), nil, diffOf(kindTick)); err != nil || len(cs) != 0 {
		t.Fatalf("nil view: %v, %v", cs, err)
	}
	if n := e.r.snap.Load().byID["s1"].trig.(*scripted).calls.Load(); n != 0 {
		t.Fatalf("evaluated %d times with no view", n)
	}
}

func TestMinInputTrust(t *testing.T) {
	d := diffOf(kindTick) // event: exchange
	d.Changes = []worldmodel.Change{{Trust: proactive.TrustSystem}, {Trust: proactive.TrustUntrusted}}
	if got := MinInputTrust(d); got != proactive.TrustUntrusted {
		t.Fatalf("got %s", got)
	}
	d.Changes = []worldmodel.Change{{Trust: proactive.TrustSystem}}
	d.Signals = []worldmodel.Signal{{Trust: proactive.TrustExchange}}
	if got := MinInputTrust(d, proactive.TrustUserAuthored); got != proactive.TrustExchange {
		t.Fatalf("got %s", got)
	}
}
