package triggers

import (
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/calendar"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// ---- the market source as a data_stale source (O-13) ---------------------------------

func btcAlert() perception.Demand {
	return perception.Demand{
		Provider: "binance", Symbol: "BTC/USDT", Kind: perception.DemandTicker, MinInterval: PriceCrossPollInterval,
	}
}

// With a live price alert or watch (a ticker or candle demand of a crypto venue) the market
// source is Active; with none, or with only demands the market source does not serve or
// equity venues' (their hours are not crypto's), it is not.
func TestAnyCryptoMarketDemand_ActiveOnlyWithACryptoDemand(t *testing.T) {
	d := perception.NewStaticDemands()
	active := AnyCryptoMarketDemand(d)
	var v worldmodel.View
	if active(v) {
		t.Fatal("active with no alert or watch")
	}
	d.Set([]perception.Demand{
		{Provider: "settrade", Symbol: "PTT", Kind: perception.DemandTicker},
		{Provider: "webull", Symbol: "AAPL", Kind: perception.DemandOHLCV, Timeframe: "1h"},
		{Provider: "okx", Account: "main", Kind: perception.DemandFuturesPositions},
		{Provider: "dn", Symbol: "1", Kind: perception.DemandPlanHealth},
	})
	if active(v) {
		t.Fatal("active for demands that are equity venues' or that the market source does not serve")
	}
	d.Set([]perception.Demand{btcAlert()})
	if !active(v) {
		t.Fatal("not active with a live price alert")
	}
	d.Set([]perception.Demand{{Provider: "bitkub", Symbol: "BTC/THB", Kind: perception.DemandOHLCV, Timeframe: "1h"}})
	if !active(v) {
		t.Fatal("not active with a crypto candle watch")
	}
	if AnyCryptoMarketDemand(nil)(v) {
		t.Fatal("active with no demand source")
	}
}

func marketRig(t *testing.T, d perception.DemandSource) *rig {
	return newRig(t, KindDataStale, `{}`, func(c *PortfolioConfig) {
		c.StaleSources = func() []StaleSource {
			return []StaleSource{{
				Name: "market", ExpectedEvery: PriceCrossPollInterval, Market: calendar.Crypto,
				Active: AnyCryptoMarketDemand(d), UnreadableAfter: 1,
			}}
		}
	})
}

// A quiet market source with nothing demanded, or with only an equity venue demanded (that
// market may simply be closed, and the source's one health record cannot tell the venues
// apart), gives no notice.
func TestDataStale_MarketSilentButNothingDependsOnIt(t *testing.T) {
	d := perception.NewStaticDemands()
	r := marketRig(t, d)
	r.v.src = map[string]perception.SourceHealth{"market": {LastEventAt: noon.Add(-time.Hour)}}
	r.eval(events.KindPerceptionClockSession, noon)
	if cs := r.eval(events.KindPerceptionClockSession, noon.Add(time.Hour)); len(cs) != 0 {
		t.Fatalf("a notice with no alert or watch: %v", keys(cs))
	}
	d.Set([]perception.Demand{{Provider: "settrade", Symbol: "PTT", Kind: perception.DemandTicker}})
	if cs := r.eval(events.KindPerceptionClockSession, noon.Add(2*time.Hour)); len(cs) != 0 {
		t.Fatalf("a notice for an equity venue whose market may simply be closed: %v", keys(cs))
	}
}

// The silent path: a price alert is live, the source says nothing for more than the factor
// times its period, the owner is told once; when ticks resume there is one recovery.
func TestDataStale_MarketQuietThenRecovered(t *testing.T) {
	r := marketRig(t, perception.NewStaticDemands(btcAlert()))
	last := noon
	r.v.src = map[string]perception.SourceHealth{"market": {LastEventAt: last}}
	r.eval(events.KindPerceptionClockSession, noon) // t0
	if cs := r.eval(events.KindPerceptionClockSession, noon.Add(3*time.Minute)); len(cs) != 0 {
		t.Fatalf("a notice inside the allowed silence (3 periods): %v", keys(cs))
	}
	c := mustOne(t, r.eval(events.KindPerceptionClockSession, noon.Add(4*time.Minute)))
	if c.DedupKey != "data_stale:market:stale1" || c.Evidence["reason"] != "quiet" || c.Evidence["source"] != "market" {
		t.Fatalf("%+v", c)
	}
	if cs := r.eval(events.KindPerceptionClockSession, noon.Add(10*time.Minute)); len(cs) != 0 {
		t.Fatalf("the same outage was announced twice: %v", keys(cs))
	}
	back := noon.Add(31 * time.Minute)
	r.v.src["market"] = perception.SourceHealth{LastEventAt: back}
	c = mustOne(t, r.eval(events.KindPerceptionMarketTick, back))
	if c.DedupKey != "data_stale:market:ok1" {
		t.Fatalf("%+v", c)
	}
}

// The unreadable path, the one the rate-limit outage takes: the source reports the provider
// degraded once (it backs off and sends no second report), and that one report is enough.
// With the default of three reports the owner would never be told.
func TestDataStale_MarketUnreadableOnTheSourcesOneReport(t *testing.T) {
	r := marketRig(t, perception.NewStaticDemands(btcAlert()))
	r.v.src = map[string]perception.SourceHealth{"market": {LastEventAt: noon}}
	r.v.srcState = map[string]worldmodel.SourceView{"market": {Name: "market", Degraded: true, ConsecutiveDegraded: 1}}
	c := mustOne(t, r.eval(events.KindPerceptionSourceHealth, noon))
	if c.DedupKey != "data_stale:market:stale1" || c.Evidence["reason"] != "unreadable" {
		t.Fatalf("%+v", c)
	}
	// data returns: the source is readable again and says so
	r.v.srcState["market"] = worldmodel.SourceView{Name: "market"}
	back := noon.Add(31 * time.Minute)
	r.v.src["market"] = perception.SourceHealth{LastEventAt: back}
	c = mustOne(t, r.eval(events.KindPerceptionMarketTick, back))
	if c.DedupKey != "data_stale:market:ok1" {
		t.Fatalf("%+v", c)
	}

	// the same state on a source left at the default needs three reports
	slow := newRig(t, KindDataStale, `{}`, func(c *PortfolioConfig) {
		c.StaleSources = func() []StaleSource {
			return []StaleSource{{Name: "market", ExpectedEvery: time.Minute, Market: calendar.Crypto}}
		}
	})
	slow.v.src = map[string]perception.SourceHealth{"market": {LastEventAt: noon}}
	slow.v.srcState = map[string]worldmodel.SourceView{"market": {Name: "market", Degraded: true, ConsecutiveDegraded: 1}}
	if cs := slow.eval(events.KindPerceptionSourceHealth, noon); len(cs) != 0 {
		t.Fatalf("%v", keys(cs))
	}
}

// A closed crypto calendar (the injected one) is not an outage either.
func TestDataStale_MarketClosedCalendarGivesNoNotice(t *testing.T) {
	r := newRig(t, KindDataStale, `{}`, func(c *PortfolioConfig) {
		c.MarketOpen = func(calendar.Market, time.Time) bool { return false }
		c.StaleSources = func() []StaleSource {
			return []StaleSource{{
				Name: "market", ExpectedEvery: time.Minute, Market: calendar.Crypto,
				Active: AnyCryptoMarketDemand(perception.NewStaticDemands(btcAlert())), UnreadableAfter: 1,
			}}
		}
	})
	r.v.src = map[string]perception.SourceHealth{"market": {LastEventAt: noon.Add(-time.Hour)}}
	r.v.srcState = map[string]worldmodel.SourceView{"market": {Name: "market", Degraded: true, ConsecutiveDegraded: 1}}
	if cs := r.eval(events.KindPerceptionSourceHealth, noon.Add(time.Hour)); len(cs) != 0 {
		t.Fatalf("%v", keys(cs))
	}
}
