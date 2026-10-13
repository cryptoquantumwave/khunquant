package worldmodel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

// t0 is 12:00 on 2026-03-10 in Bangkok (05:00 UTC), on a minute and an hour
// boundary of the Unix timeline.
var t0 = time.Date(2026, 3, 10, 5, 0, 0, 0, time.UTC)

var bkk = func() *time.Location {
	l, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic(err)
	}
	return l
}()

func zoneBKK() *time.Location { return bkk }

// foreignTick has the JSON shape of sources.MarketTick but is not the model's own
// payload type, so applying it goes through the same JSON decode a real event does.
type foreignTick struct {
	Provider string    `json:"provider"`
	Symbol   string    `json:"symbol"`
	Last     float64   `json:"last"`
	Bid      float64   `json:"bid,omitempty"`
	At       time.Time `json:"at"`
}

func tickAt(at time.Time, prov, sym string, price float64) events.Event {
	return events.Event{
		ID: fmt.Sprintf("t-%s-%s-%d", prov, sym, at.UnixNano()), Kind: events.KindPerceptionMarketTick, Time: at,
		Source:  events.Source{Component: "perception", Name: "market"},
		Payload: foreignTick{Provider: prov, Symbol: sym, Last: price, At: at},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}
}

func newClock() *perceptiontest.Clock { return perceptiontest.NewClock(t0) }

// newMemStore is a store without a database.
func newMemStore(t testing.TB, clk *perceptiontest.Clock, opts ...Option) *Store {
	t.Helper()
	s, err := NewStore(context.Background(), nil, clk, append([]Option{WithZone(zoneBKK)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newDBStore(t testing.TB, db *sql.DB, clk *perceptiontest.Clock, opts ...Option) *Store {
	t.Helper()
	s, err := NewStore(context.Background(), db, clk, append([]Option{WithZone(zoneBKK)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openDB(t testing.TB) *sql.DB { return sqlitextest.Open(t) }

// apply runs one event through the store. A test clock is moved up to the event's
// time first, as real time would have been when the event happened.
func apply(t testing.TB, s *Store, ev events.Event) Diff {
	t.Helper()
	if c, ok := s.clock.(*perceptiontest.Clock); ok {
		if d := ev.Time.Sub(c.Now()); d > 0 {
			c.Advance(d)
		}
	}
	d, err := s.Apply(context.Background(), ev)
	if err != nil {
		t.Fatalf("Apply(%s): %v", ev.Kind, err)
	}
	return d
}

func snapshotEvent(at time.Time, total float64, unpriced ...string) events.Event {
	return events.Event{
		ID: fmt.Sprintf("snap-%d", at.UnixNano()), Kind: events.KindPerceptionPortfolioSnapshot, Time: at,
		Source: events.Source{Component: "perception", Name: "portfolio"},
		Payload: PortfolioPayload{
			SnapshotID: 1, TakenAt: at, Quote: "USDT", TotalValueUSD: total,
			ByClass:   map[string]float64{"crypto": total * 0.75, "cash": total * 0.25},
			Positions: []PortfolioPositionPayload{{Asset: "BTC", Source: "binance", Class: "crypto", Value: total * 0.75, Qty: 1}, {Asset: "USDT", Source: "binance", Class: "cash", Value: total * 0.25, Qty: total * 0.25}},
			Unpriced:  unpriced,
		},
		Trust: events.TrustPtr(proactive.TrustSystem),
	}
}

func futuresEvent(at time.Time, provider, account string, positions ...FuturesPositionPayload) events.Event {
	return events.Event{
		ID: fmt.Sprintf("fut-%d", at.UnixNano()), Kind: events.KindPerceptionFuturesPositions, Time: at,
		Source:  events.Source{Component: "perception", Name: "futures"},
		Payload: FuturesPayload{Provider: provider, Account: account, At: at, Positions: positions},
		Trust:   events.TrustPtr(proactive.TrustExchange),
	}
}

func longBTC(contracts float64) FuturesPositionPayload {
	return FuturesPositionPayload{
		Symbol: "BTC/USDT:USDT", Side: "long", Contracts: contracts,
		ContractSize: 0.01, ContractSizeKnown: true, MarkPrice: 60000, MarkPriceKnown: true,
		NotionalUSD: contracts * 0.01 * 60000, NotionalKnown: true,
	}
}

func healthEvent(at time.Time, p HealthPayload) events.Event {
	return events.Event{
		ID: "h", Kind: events.KindPerceptionSourceHealth, Time: at,
		Source: events.Source{Component: "perception", Name: p.Source}, Payload: p,
		Trust: events.TrustPtr(proactive.TrustSystem),
	}
}

func fillEvent(at time.Time, o orderfeed.OrderEvent) events.Event {
	return events.Event{
		ID: "f", Kind: events.KindPerceptionOrderFilled, Time: at,
		Source: events.Source{Component: "perception", Name: "orderfeed"}, Payload: o,
		Trust: events.TrustPtr(proactive.TrustExchange),
	}
}

func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// feed applies a ramp of ticks, one every 10 seconds from start, for n minutes.
func feedMinutes(t testing.TB, s *Store, start time.Time, prov, sym string, minutes int, price func(i int) float64) []Diff {
	t.Helper()
	var out []Diff
	for m := 0; m < minutes; m++ {
		for sec := 0; sec < 60; sec += 10 {
			at := start.Add(time.Duration(m)*time.Minute + time.Duration(sec)*time.Second)
			out = append(out, apply(t, s, tickAt(at, prov, sym, price(m))))
		}
	}
	return out
}
