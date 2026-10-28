package sources_test

import (
	"context"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// Each polling source is busy from the moment a pass begins (queued behind a lock or
// taken and still running) and idle again once it returns; asking changes nothing.

func TestMarketSource_IdleCoversAPoll(t *testing.T) {
	r := newRig(t, sources.MarketOptions{}, tickDemand("binance", "a", "BTC/USDT", 0))
	entered, gate := make(chan struct{}), make(chan struct{})
	r.reader.hook = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-gate
	}
	r.reader.tickers["BTC/USDT"] = tk(100, time.Time{})
	if !r.src.Idle() {
		t.Fatal("busy before any poll")
	}
	done := make(chan struct{})
	go func() { _ = r.src.Reconcile(context.Background(), r.out); close(done) }()
	<-entered
	statsBefore := r.src.Stats()
	if r.src.Idle() {
		t.Fatal("idle while a poll is in flight")
	}
	if got := r.src.Stats(); got != statsBefore {
		t.Fatal("Idle changed the source's counters")
	}
	close(gate)
	<-done
	if !r.src.Idle() {
		t.Fatal("busy after every poll returned")
	}
}

func TestPortfolioSource_IdleCoversACollection(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	r := newPortfolioRig(t, func() (*snapshot.CollectResult, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-gate
		return completeResult(), nil
	})
	<-entered
	if r.src.Idle() {
		t.Fatal("idle while a collection is running")
	}
	close(gate)
	waitFor(t, "the snapshot", func() bool { return len(r.out.events()) == 1 })
	waitFor(t, "idle after the collection", r.src.Idle)
}

// blockingFutures holds FetchFuturesPositions until released.
type blockingFutures struct {
	*fakeFutures
	entered chan struct{}
	gate    chan struct{}
}

func (b *blockingFutures) FetchFuturesPositions(ctx context.Context, s []string) ([]ccxt.Position, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.gate
	return b.fakeFutures.FetchFuturesPositions(ctx, s)
}

func TestFuturesSource_IdleCoversAPoll(t *testing.T) {
	fp := &blockingFutures{fakeFutures: &fakeFutures{}, entered: make(chan struct{}), gate: make(chan struct{})}
	var _ broker.FuturesProvider = fp
	r := newFuturesRig(t, oneAccount(fp))
	<-fp.entered
	if r.src.Idle() {
		t.Fatal("idle while an account is being polled")
	}
	close(fp.gate)
	waitFor(t, "the poll to finish", func() bool { n, _ := fp.fakeFutures.counts(); return n == 1 })
	waitFor(t, "idle after the poll", r.src.Idle)
}

func TestDNPlansSource_IdleCoversAPass(t *testing.T) {
	r := newDNRig(t, nil, dnPlan(1, "1m"))
	r.checker.block = make(chan struct{})
	if !r.src.Idle() {
		t.Fatal("busy before any pass")
	}
	done := make(chan struct{})
	go func() { _ = r.src.Reconcile(context.Background(), r.out); close(done) }()
	waitFor(t, "the check to start", func() bool { return r.checker.count() == 1 })
	if r.src.Idle() {
		t.Fatal("idle while a plan is being checked")
	}
	close(r.checker.block)
	<-done
	if !r.src.Idle() {
		t.Fatal("busy after the pass returned")
	}
}

var _ perception.Idler = (*sources.MarketSource)(nil)
var _ perception.Idler = (*sources.Portfolio)(nil)
var _ perception.Idler = (*sources.Futures)(nil)
var _ perception.Idler = (*sources.DNPlans)(nil)
var _ perception.Idler = (*sources.News)(nil)
