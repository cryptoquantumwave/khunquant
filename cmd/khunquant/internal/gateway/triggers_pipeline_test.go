package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive/engine"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
	"github.com/cryptoquantumwave/khunquant/pkg/triggers"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// seen is what the observing trigger saw on one evaluation.
type seen struct {
	kind  events.Kind
	empty bool
	price float64
	mark  float64
}

// observer is a trigger that lists three kinds and records, for each evaluation,
// what the Diff said and what the View holds. It never fires.
type observer struct {
	id  string
	mu  *sync.Mutex
	out *[]seen
}

func (o observer) ID() string { return o.id }
func (observer) Kinds() []events.Kind {
	return []events.Kind{events.KindPerceptionMarketTick, events.KindPerceptionFuturesPositions, events.KindPerceptionDevice}
}
func (observer) Demands() []perception.Demand { return nil }
func (o observer) Evaluate(_ context.Context, v worldmodel.View, d worldmodel.Diff, _ *triggers.State) ([]proactive.Candidate, error) {
	s := seen{kind: d.Event.Kind, empty: d.Empty()}
	s.price, _, _ = v.Price("binance", "BTC/USDT")
	if p, ok := v.FuturesPosition("okx/main", "BTC/USDT:USDT"); ok {
		s.mark = p.MarkPrice
	}
	o.mu.Lock()
	*o.out = append(*o.out, s)
	o.mu.Unlock()
	return nil, nil
}

// realPipeline is the real engine over the real world model and the real registry,
// joined by the gateway's two stage adapters, with a bus to watch.
type realPipeline struct {
	t    *testing.T
	eng  *engine.Engine
	ch   <-chan events.Event
	mu   sync.Mutex
	got  []seen
	sent uint64
}

func newRealPipeline(t *testing.T) *realPipeline {
	t.Helper()
	p := &realPipeline{t: t}
	db := sqlitextest.Open(t)
	store, err := worldmodel.NewStore(context.Background(), nil, perception.RealClock{})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := triggers.NewRegistry(context.Background(), db, func() string { return triggers.ModeShadow },
		map[string]triggers.Factory{"obs": func(s triggers.Spec) (triggers.Trigger, error) {
			return observer{id: s.Kind + ":" + s.ID, mu: &p.mu, out: &p.got}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(context.Background(), triggers.Spec{
		ID: "o1", Kind: "obs", Mode: triggers.ModeLive, Origin: "test", Params: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus()
	t.Cleanup(func() { _ = bus.Close() })
	_, p.ch, err = bus.Channel().SubscribeChan(context.Background(), events.SubscribeOptions{Buffer: 1000})
	if err != nil {
		t.Fatal(err)
	}
	p.eng = engine.New(engine.Options{
		WorldModel: worldModelStage{s: store}, Evaluator: triggerStage{r: reg}, Bus: bus,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p
}

func (p *realPipeline) emit(evs ...events.Event) {
	p.t.Helper()
	for _, ev := range evs {
		p.sent++
		p.eng.Emitter().Emit(ev)
	}
	waitUntil(p.t, "the events", func() bool { return p.eng.Stats().Processed == p.sent })
}

func (p *realPipeline) seen() []seen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seen(nil), p.got...)
}

// diffsOnBus counts the worldmodel.diff events published so far.
func (p *realPipeline) diffsOnBus() int {
	n := 0
	for {
		select {
		case ev := <-p.ch:
			if ev.Kind == events.KindWorldModelDiff {
				n++
			}
		default:
			return n
		}
	}
}

func tickAt(at time.Time, last float64) events.Event {
	return events.Event{
		Kind: events.KindPerceptionMarketTick, Time: at, Source: events.Source{Component: "perception", Name: "market"},
		Payload: sources.MarketTick{Provider: "binance", Symbol: "BTC/USDT", Last: last, At: at},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}
}

func positionsAt(at time.Time, contracts, mark float64) events.Event {
	return events.Event{
		Kind: events.KindPerceptionFuturesPositions, Time: at, Source: events.Source{Component: "perception", Name: "futures"},
		Payload: sources.FuturesPositionsPayload{Provider: "okx", Account: "main", At: at, Positions: []sources.FuturesPosition{{
			Symbol: "BTC/USDT:USDT", Side: "long", Contracts: contracts, ContractSize: 0.01, ContractSizeKnown: true,
			MarkPrice: mark, MarkPriceKnown: true, LiqPrice: 50000, LiqPriceKnown: true,
			NotionalUSD: contracts * 0.01 * mark, NotionalKnown: true,
		}}},
		Trust: events.TrustPtr(proactive.TrustExchange),
	}
}

// (a) Two ticks inside one open minute bucket change no fact in the change log, and
// both reach a trigger that lists the tick kind, which reads the new price from the
// View.
func TestTriggersPipeline_EveryTickReachesTheTriggerAndSeesTheNewPrice(t *testing.T) {
	p := newRealPipeline(t)
	t0 := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
	p.emit(tickAt(t0.Add(5*time.Second), 62000), tickAt(t0.Add(25*time.Second), 62100), tickAt(t0.Add(45*time.Second), 62200))

	got := p.seen()
	if len(got) != 3 {
		t.Fatalf("the trigger saw %d of 3 ticks: %+v", len(got), got)
	}
	for i, want := range []float64{62000, 62100, 62200} {
		if got[i].kind != events.KindPerceptionMarketTick || got[i].price != want || !got[i].empty {
			t.Fatalf("tick %d: %+v, want an empty diff and price %v", i, got[i], want)
		}
	}
	// (c) None of them went onto the bus as a worldmodel.diff.
	if n := p.diffsOnBus(); n != 0 {
		t.Fatalf("%d worldmodel.diff events for empty diffs", n)
	}
	if st := p.eng.Stats(); st.EmptyDiffs != 3 || st.StageErrors != 0 || st.StagePanics != 0 {
		t.Fatalf("engine stats = %+v", st)
	}
}

// (b) A futures-positions event whose contract count did not change gives no Change,
// and still reaches the trigger, which reads the new mark from the View. (c) The
// first one (the position appears) is a real change and is published.
func TestTriggersPipeline_UnchangedPositionsStillReachTheTriggerWithTheNewMark(t *testing.T) {
	p := newRealPipeline(t)
	t0 := time.Now().UTC().Add(-time.Hour)
	p.emit(positionsAt(t0, 135, 60000), positionsAt(t0.Add(time.Minute), 135, 59000), positionsAt(t0.Add(2*time.Minute), 135, 57500))

	got := p.seen()
	if len(got) != 3 {
		t.Fatalf("the trigger saw %d of 3 events: %+v", len(got), got)
	}
	if got[0].empty || got[0].mark != 60000 {
		t.Fatalf("the appearance of the position: %+v", got[0])
	}
	for i, want := range []float64{59000, 57500} {
		if g := got[i+1]; !g.empty || g.mark != want {
			t.Fatalf("event %d: %+v, want an empty diff and mark %v", i+1, g, want)
		}
	}
	if n := p.diffsOnBus(); n != 1 {
		t.Fatalf("%d worldmodel.diff events, want exactly 1 (the non-empty one)", n)
	}
	if st := p.eng.Stats(); st.EmptyDiffs != 2 {
		t.Fatalf("engine stats = %+v", st)
	}
}

// (d) An untracked kind still routes, as an envelope-only diff.
func TestTriggersPipeline_UntrackedKindStillRoutes(t *testing.T) {
	p := newRealPipeline(t)
	p.emit(events.Event{
		ID: "d1", Kind: events.KindPerceptionDevice, Time: time.Now().UTC(), Payload: "usb",
		Source: events.Source{Component: "perception", Name: "devices"}, Trust: events.TrustPtr(proactive.TrustSystem),
	})
	got := p.seen()
	if len(got) != 1 || got[0].kind != events.KindPerceptionDevice || !got[0].empty {
		t.Fatalf("seen = %+v", got)
	}
	// A kind no trigger lists is not routed.
	p.emit(events.Event{Kind: events.KindPerceptionClockSession, Time: time.Now().UTC(),
		Source: events.Source{Component: "perception", Name: "clock"}, Trust: events.TrustPtr(proactive.TrustSystem)})
	if n := len(p.seen()); n != 1 {
		t.Fatalf("an unlisted kind reached the trigger (%d evaluations)", n)
	}
}
