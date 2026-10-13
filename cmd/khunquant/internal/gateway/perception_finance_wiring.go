package gateway

import (
	"io"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/agent"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/logger"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/orderfeed"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/proactive"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// This file wires what perception knows about money: the scheduled portfolio
// snapshots, the futures positions and the order feed. None of it exists unless
// perception is effectively on (buildPerception returns before reaching it), and
// the sources exist only when an exchange account is configured.
//
// It does not touch the daily loss guard: that sampler (lossguard.go) keeps its own
// cadence, because samples that come every 15 minutes instead of every 5 would
// leave the guard's account samples stale and make it refuse orders.

// financeSources builds the money sources and the things they must close. It is a
// seam: tests replace it so that no exchange is asked.
var financeSources = func(d assemblyDeps) (srcs []perception.Source, closers []io.Closer) {
	if len(broker.ListConfiguredAccounts(d.Cfg)) == 0 {
		return nil, nil
	}

	if store, err := snapshot.NewStore(agent.DefaultAgentWorkspace(d.Cfg)); err != nil {
		logger.WarnCF("perception", "Portfolio snapshots disabled: the snapshot store could not be opened",
			map[string]any{"error": err.Error()})
	} else {
		closers = append(closers, store)
		srcs = append(srcs, sources.NewPortfolio(sources.PortfolioOptions{
			Cfg:      d.Cfg,
			Store:    store,
			Interval: time.Duration(d.Cfg.Proactive.Perception.SnapshotMinutes) * time.Minute,
		}))
	}

	srcs = append(srcs, sources.NewFutures(sources.FuturesOptions{Cfg: d.Cfg}))
	return srcs, closers
}

// financeRuntime is what attachFinance leaves on a perceptionRuntime.
type financeRuntime struct {
	closers   []io.Closer
	clearFeed func()
	portfolio *sources.Portfolio
	futures   *sources.Futures
}

// attachFinance adds the money sources to the manager and connects the order feed
// to the engine. The publisher it installs is removed again by stopFinance, and
// only if it is still the current one (a reload builds the next generation's before
// it stops this one's).
func (rt *perceptionRuntime) attachFinance(d assemblyDeps) {
	srcs, closers := financeSources(d)
	rt.finance.closers = closers
	for _, s := range srcs {
		switch v := s.(type) {
		case *sources.Portfolio:
			rt.finance.portfolio = v
		case *sources.Futures:
			rt.finance.futures = v
		}
		rt.manager.Add(s)
	}
	rt.finance.clearFeed = orderfeed.SetPublisher(rt.publishOrder)
}

// stopFinanceFeed ends the order feed's goroutine, so no more orders reach the
// engine while it is being stopped.
func (rt *perceptionRuntime) stopFinanceFeed() {
	if rt.finance.clearFeed != nil {
		rt.finance.clearFeed()
	}
}

// closeFinance releases what the sources held. It runs after the sources stopped.
func (rt *perceptionRuntime) closeFinance() {
	for _, c := range rt.finance.closers {
		if err := c.Close(); err != nil {
			logger.WarnCF("perception", "Closing a perception store failed", map[string]any{"error": err.Error()})
		}
	}
	rt.finance.closers = nil
}

// publishOrder turns an order a tool placed into a perception event. It runs on the
// order feed's goroutine and never waits: a full engine inbox drops the event and
// counts it.
//
// An order that closed or filled is perception.order.filled; any other is
// perception.order.placed. A paper order (never on an exchange) is always
// perception.order.placed, whatever its simulated status, and carries Paper in its
// payload: nothing that reads "filled" should ever be able to take a simulation for
// a fill.
func (rt *perceptionRuntime) publishOrder(ev orderfeed.OrderEvent) {
	if !rt.engine.Emitter().EmitNonBlocking(orderEvent(ev)) {
		orderfeed.NoteDropped()
	}
}

// orderEvent is the perception event for an order (see publishOrder for the kind).
func orderEvent(ev orderfeed.OrderEvent) events.Event {
	kind := events.KindPerceptionOrderPlaced
	if !ev.Paper && (ev.Status == "closed" || ev.Status == "filled") {
		kind = events.KindPerceptionOrderFilled
	}
	return events.Event{
		Kind:     kind,
		Time:     ev.At,
		Source:   events.Source{Component: "perception", Name: "orderfeed"},
		Severity: events.SeverityInfo,
		Payload:  ev,
		Attrs:    map[string]any{"subject": ev.Provider + "/" + ev.Account, "paper": ev.Paper, "origin": ev.Origin},
		Trust:    events.TrustPtr(proactive.TrustExchange),
	}
}

// financeStats are the counters of the money sources and the order feed. Counts
// only. The order feed's counters are the process's, not the generation's: a config
// reload does not reset them.
type financeStats struct {
	OrderFeed orderFeedStats     `json:"orderfeed"`
	Portfolio *portfolioSrcStats `json:"portfolio,omitempty"`
	Futures   *futuresSrcStats   `json:"futures,omitempty"`
}

type orderFeedStats struct {
	Published uint64 `json:"published"`
	Delivered uint64 `json:"delivered"`
	Dropped   uint64 `json:"dropped"`
	Panics    uint64 `json:"panics"`
}

type portfolioSrcStats struct {
	Cycles  uint64 `json:"cycles"`
	Saved   uint64 `json:"saved"`
	Partial uint64 `json:"partial"`
	Failed  uint64 `json:"failed"`
	Dropped uint64 `json:"dropped"`
	Pruned  uint64 `json:"pruned"`

	WithUnpriced uint64 `json:"with_unpriced"`
}

type futuresSrcStats struct {
	Polls   uint64 `json:"polls"`
	Emitted uint64 `json:"emitted"`
	Failed  uint64 `json:"failed"`
	Dropped uint64 `json:"dropped"`
	Skipped uint64 `json:"skipped"`

	Degraded uint64 `json:"degraded"`
}

func (rt *perceptionRuntime) fillFinanceStats(out *financeStats) {
	s := orderfeed.ReadStats()
	out.OrderFeed = orderFeedStats{Published: s.Published, Delivered: s.Delivered, Dropped: s.Dropped, Panics: s.Panics}
	if p := rt.finance.portfolio; p != nil {
		st := p.Stats()
		out.Portfolio = &portfolioSrcStats{st.Cycles, st.Saved, st.Partial, st.Failed, st.Dropped, st.Pruned, st.WithUnpriced}
	}
	if f := rt.finance.futures; f != nil {
		st := f.Stats()
		out.Futures = &futuresSrcStats{st.Polls, st.Emitted, st.Failed, st.Dropped, st.Skipped, st.Degraded}
	}
}
