package triggers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

var allKinds = []string{
	KindPriceCross, KindIndicatorCross, KindDCADue, KindDNHealth, KindExternal,
	KindMoveVsVol, KindDrawdown, KindAllocationDrift, KindLiquidationDistance, KindDataStale,
}

func TestOwnerAlertSpec(t *testing.T) {
	cases := []struct {
		kind, origin string
		want         bool
	}{
		{KindPriceCross, "tool:set_price_alert", true},
		{KindIndicatorCross, "tool:set_indicator_alert", true},
		{KindPriceCross, "migration:cron:1234", true},
		{KindIndicatorCross, "migration:cron:abc", true},
		// the wrong tool for the kind, and tools that are not the alert tools
		{KindPriceCross, "tool:set_indicator_alert", false},
		{KindIndicatorCross, "tool:set_price_alert", false},
		{KindPriceCross, "tool:", false},
		{KindPriceCross, "tool:something_else", false},
		{KindPriceCross, "tool:set_price_alert2", false},
		// a migration prefix with no job
		{KindPriceCross, "migration:cron:", false},
		{KindPriceCross, "migration:", false},
		// everything that is not an owner's alert
		{KindPriceCross, "builtin", false},
		{KindPriceCross, "builtin:portfolio", false},
		{KindPriceCross, "intent", false},
		{KindPriceCross, "test", false},
		{KindPriceCross, "", false},
		{KindPriceCross, " tool:set_price_alert", false},
	}
	for _, c := range cases {
		if got := OwnerAlertSpec(c.kind, c.origin); got != c.want {
			t.Errorf("OwnerAlertSpec(%q, %q) = %v, want %v", c.kind, c.origin, got, c.want)
		}
	}
	// No other kind, whatever the origin.
	if len(allKinds) != 10 {
		t.Fatalf("the table must cover all ten kinds, has %d", len(allKinds))
	}
	for _, k := range allKinds {
		if k == KindPriceCross || k == KindIndicatorCross {
			continue
		}
		for _, o := range []string{OriginSetPriceAlert, OriginSetIndicatorAlert, MigratedOriginPrefix + "1", "builtin", PortfolioSpecOrigin, DCASpecOrigin} {
			if OwnerAlertSpec(k, o) {
				t.Errorf("OwnerAlertSpec(%q, %q) = true", k, o)
			}
		}
	}
}

func addOrigin(t *testing.T, ce *crossEnv, id, kind, mode, origin, params string) {
	t.Helper()
	if _, err := ce.r.Add(context.Background(), Spec{
		ID: id, Kind: kind, Mode: mode, Origin: origin, Params: json.RawMessage(params),
	}); err != nil {
		t.Fatalf("Add %s: %v", id, err)
	}
}

func TestOwnerAlert_PriceCrossMarkFollowsTheOrigin(t *testing.T) {
	for _, c := range []struct {
		origin string
		want   bool
	}{
		{OriginSetPriceAlert, true},
		{MigratedOriginPrefix + "job1", true},
		{"test", false},
		{"builtin", false},
		{"intent", false},
	} {
		t.Run(c.origin, func(t *testing.T) {
			ce := newCrossEnv(t)
			addOrigin(t, ce, "p1", KindPriceCross, ModeLive, c.origin, priceParams("above", 60000, `"fire_if_already":false`))
			ce.at(noon).tick(59000)
			cs := ce.at(noon.Add(time.Minute)).tick(61000)
			if len(cs) != 1 {
				t.Fatalf("got %d candidates", len(cs))
			}
			if cs[0].OwnerRequested != c.want {
				t.Fatalf("OwnerRequested = %v, want %v", cs[0].OwnerRequested, c.want)
			}
			if got := ce.r.Stats().OwnerMarkCleared; got != 0 {
				t.Fatalf("a trigger that sets the mark by its own rule was counted as cleared: %d", got)
			}
		})
	}
}

func TestOwnerAlert_IndicatorCrossMarkFollowsTheOrigin(t *testing.T) {
	for _, c := range []struct {
		origin string
		want   bool
	}{
		{OriginSetIndicatorAlert, true},
		{MigratedOriginPrefix + "job2", true},
		{"test", false},
		{OriginSetPriceAlert, false},
	} {
		t.Run(c.origin, func(t *testing.T) {
			ce := newCrossEnv(t)
			addOrigin(t, ce, "i1", KindIndicatorCross, ModeLive, c.origin, indParams(IndSMA, 5, DirAbove, 105, `"fire_if_already":false`))
			ce.bar([]float64{100, 102, 104, 106, 108})
			cs := ce.bar([]float64{102, 104, 106, 108, 112})
			if len(cs) != 1 {
				t.Fatalf("got %d candidates", len(cs))
			}
			if cs[0].OwnerRequested != c.want {
				t.Fatalf("OwnerRequested = %v, want %v", cs[0].OwnerRequested, c.want)
			}
		})
	}
}

func TestOwnerAlert_MarkKeptOnAShadowCandidate(t *testing.T) {
	// The mark says what the spec is; the shadow flag says it is only logged. Attention
	// reads the shadow flag first.
	ce := newCrossEnv(t)
	addOrigin(t, ce, "p1", KindPriceCross, ModeShadow, OriginSetPriceAlert, priceParams("above", 60000, `"fire_if_already":false`))
	ce.at(noon).tick(59000)
	cs := ce.at(noon.Add(time.Minute)).tick(61000)
	if len(cs) != 1 || !cs[0].Shadow || !cs[0].OwnerRequested {
		t.Fatalf("candidates = %+v", cs)
	}
}

func TestOwnerAlert_UpdatingTheOriginRebuildsTheMark(t *testing.T) {
	ce := newCrossEnv(t)
	addOrigin(t, ce, "p1", KindPriceCross, ModeLive, "test", priceParams("above", 60000, `"fire_if_already":false`))
	specs, err := ce.r.List(context.Background(), ListFilter{})
	if err != nil || len(specs) != 1 {
		t.Fatalf("list: %v %v", specs, err)
	}
	s := specs[0]
	s.Origin = OriginSetPriceAlert
	if err := ce.r.UpdateFresh(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	ce.at(noon).tick(59000)
	cs := ce.at(noon.Add(time.Minute)).tick(61000)
	if len(cs) != 1 || !cs[0].OwnerRequested {
		t.Fatalf("after the origin changed: %+v", cs)
	}
}

// markedFires is a trigger that returns one candidate with the mark set, whatever its
// kind: what a buggy or new kind would do.
func markedFires(kind string) evalFn {
	return func(call int, _ *State, _ worldmodel.Diff) ([]proactive.Candidate, error) {
		c := cand(kind, fmt.Sprintf("n%d", call))
		c.OwnerRequested = true
		return []proactive.Candidate{c}, nil
	}
}

// The registry clears the mark from every candidate whose spec is not an owner's
// alert, for all ten kinds and for the origins an owner's alert has, and counts it.
func TestOwnerAlert_RegistryClearsTheMarkFromEveryOtherSpec(t *testing.T) {
	origins := []string{OriginSetPriceAlert, OriginSetIndicatorAlert, MigratedOriginPrefix + "j1", "builtin", "test", "intent"}
	for _, kind := range allKinds {
		for _, origin := range origins {
			t.Run(kind+"/"+origin, func(t *testing.T) {
				e := newEnv(t, map[string]Factory{kind: factoryOf(kind, []events.Kind{kindTick}, markedFires(kind), nil)})
				if _, err := e.r.Add(context.Background(), Spec{ID: "s1", Kind: kind, Mode: ModeLive, Origin: origin, Params: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
				cs := e.eval(kindTick)
				if len(cs) != 1 {
					t.Fatalf("got %d candidates", len(cs))
				}
				keep := OwnerAlertSpec(kind, origin)
				if cs[0].OwnerRequested != keep {
					t.Fatalf("OwnerRequested = %v, want %v", cs[0].OwnerRequested, keep)
				}
				wantCleared := uint64(1)
				if keep {
					wantCleared = 0
				}
				if got := e.r.Stats().OwnerMarkCleared; got != wantCleared {
					t.Fatalf("OwnerMarkCleared = %d, want %d", got, wantCleared)
				}
				if st := e.r.Stats(); st.Errors != 0 || st.Candidates != 1 {
					t.Fatalf("the candidate was refused rather than cleared: %+v", st)
				}
			})
		}
	}
}

// The external trigger is the one that must never carry the mark: its text comes from
// outside. A candidate it returned marked would lose the mark in the registry.
func TestOwnerAlert_ExternalCandidateCannotCarryTheMark(t *testing.T) {
	e := newEnv(t, map[string]Factory{KindExternal: factoryOf(KindExternal, []events.Kind{kindTick}, markedFires(KindExternal), nil)})
	if _, err := e.r.Add(context.Background(), Spec{ID: ExternalSpecID, Kind: KindExternal, Mode: ModeLive, Origin: "builtin", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	cs := e.eval(kindTick)
	if len(cs) != 1 || cs[0].OwnerRequested {
		t.Fatalf("candidates = %+v", cs)
	}
	if got := e.r.Stats().OwnerMarkCleared; got != 1 {
		t.Fatalf("OwnerMarkCleared = %d, want 1", got)
	}
}

// The auto-disabled notice is a message of the system, not of the owner's alert, even
// for an alert spec: it waits for the owner's active hours.
func TestOwnerAlert_DisabledNoticeIsNotMarked(t *testing.T) {
	e := newEnv(t, map[string]Factory{testKind: factoryOf(testKind, []events.Kind{kindTick}, failsWith(fmt.Errorf("boom")), nil)})
	if _, err := e.r.Add(context.Background(), Spec{ID: "s1", Kind: testKind, Mode: ModeLive, Origin: OriginSetPriceAlert, Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var notice []proactive.Candidate
	for i := 0; i < MaxConsecutiveErrors; i++ {
		notice = e.eval(kindTick)
	}
	if len(notice) != 1 || notice[0].OwnerRequested {
		t.Fatalf("the auto-disabled notice = %+v", notice)
	}
}
