package triggers

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// pfView is a worldmodel.View of what the portfolio kinds read.
type pfView struct {
	worldmodel.View
	now      time.Time
	pf       *worldmodel.PortfolioView
	fut      map[string][]worldmodel.FuturesView
	src      map[string]perception.SourceHealth
	srcState map[string]worldmodel.SourceView
}

func (v *pfView) Now() time.Time { return v.now }
func (v *pfView) Fresh(at time.Time, maxAge time.Duration) bool {
	return !at.IsZero() && !at.After(v.now.Add(time.Minute)) && v.now.Sub(at) <= maxAge
}
func (v *pfView) Portfolio() (worldmodel.PortfolioView, bool) {
	if v.pf == nil {
		return worldmodel.PortfolioView{}, false
	}
	return *v.pf, true
}
func (v *pfView) FuturesPositions(a string) ([]worldmodel.FuturesView, bool) {
	ps, ok := v.fut[a]
	return ps, ok
}
func (v *pfView) Source(n string) (perception.SourceHealth, bool) {
	h, ok := v.src[n]
	return h, ok
}
func (v *pfView) SourceState(n string) (worldmodel.SourceView, bool) {
	s, ok := v.srcState[n]
	return s, ok
}

// rig drives one trigger the way the registry does: Evaluate with a private copy of
// the state, every candidate checked by the registry's own validation, and the state
// adopted afterwards (as if attention had decided).
type rig struct {
	t     *testing.T
	kind  string
	tr    Trigger
	v     *pfView
	state json.RawMessage
	stats *PortfolioStats
	cfg   PortfolioConfig
}

func newRig(t *testing.T, kind string, params string, mod func(*PortfolioConfig)) *rig {
	t.Helper()
	r := &rig{t: t, kind: kind, v: &pfView{now: noon}, stats: &PortfolioStats{}}
	r.cfg = PortfolioConfig{
		Stats: r.stats, MarketOpen: func(calendar.Market, time.Time) bool { return true },
		FuturesAccounts: func() []string { return []string{"okx/main"} },
	}
	if mod != nil {
		mod(&r.cfg)
	}
	tr, err := PortfolioFactories(r.cfg)[kind](Spec{ID: "pf", Kind: kind, Mode: ModeLive, Origin: "test", Params: json.RawMessage(params)})
	if err != nil {
		t.Fatalf("%s factory: %v", kind, err)
	}
	r.tr = tr
	return r
}

// eval evaluates an event of evKind stamped at.
func (r *rig) eval(evKind events.Kind, at time.Time) []proactive.Candidate {
	r.t.Helper()
	r.v.now = at
	d := worldmodel.Diff{Event: events.Event{ID: "e", Kind: evKind, Trust: events.TrustPtr(proactive.TrustExchange)}, At: at}
	st := &State{Raw: append(json.RawMessage(nil), r.state...)}
	cs, err := r.tr.Evaluate(context.Background(), r.v, d, st)
	if err != nil {
		r.t.Fatalf("Evaluate: %v", err)
	}
	if len(cs) > MaxCandidatesPerEvaluation {
		r.t.Fatalf("%d candidates", len(cs))
	}
	for i, c := range cs {
		if err := checkCandidate(r.kind, c); err != nil {
			r.t.Fatalf("candidate %d is malformed: %v\n%+v", i, err, c)
		}
		if c.Compose == proactive.ComposeLLM && c.Protective {
			r.t.Fatalf("a protective candidate asks for a model: %+v", c)
		}
	}
	if st.Dirty {
		if len(st.Raw) > MaxStateBytes {
			r.t.Fatalf("state is %d bytes", len(st.Raw))
		}
		if !json.Valid(st.Raw) {
			r.t.Fatalf("state is not JSON: %s", st.Raw)
		}
		r.state = st.Raw
	}
	return cs
}

func (r *rig) snapshot(at time.Time, pf worldmodel.PortfolioView) []proactive.Candidate {
	pf.At = at
	if pf.Trust == 0 {
		pf.Trust = proactive.TrustExchange
	}
	r.v.pf = &pf
	return r.eval(events.KindPerceptionPortfolioSnapshot, at)
}

func keys(cs []proactive.Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.DedupKey)
	}
	return out
}

func mustOne(t *testing.T, cs []proactive.Candidate) proactive.Candidate {
	t.Helper()
	if len(cs) != 1 {
		t.Fatalf("want one candidate, got %d: %v", len(cs), keys(cs))
	}
	return cs[0]
}

// noActions: a candidate of these kinds carries nothing that could act: no forced
// skills, and no field names that read as an order or a tool.
func noActions(t *testing.T, c proactive.Candidate) {
	t.Helper()
	if len(c.ForcedSkills) != 0 {
		t.Fatalf("forced skills on %s: %v", c.DedupKey, c.ForcedSkills)
	}
	for k := range c.Evidence {
		switch k {
		case "action", "actions", "order", "tool", "button", "buttons", "recommended_action":
			t.Fatalf("evidence key %q reads as an action", k)
		}
	}
}
