package gateway

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/feedback"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// countingReaders is the market source's readers with no network: it counts the readers
// asked for and the OHLCV reads made, and serves the bars of the account it is told to.
type countingReaders struct {
	mu       sync.Mutex
	resolved []string
	reads    int
	bars     map[string][]ccxt.OHLCV // by account
	errs     map[string]error        // by account
}

func (c *countingReaders) Reader(provider, account string) (sources.MarketReader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved = append(c.resolved, provider+"/"+account)
	if err := c.errs[account]; err != nil {
		return nil, err
	}
	return countingReader{c: c, account: account}, nil
}

func (c *countingReaders) count() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resolved), c.reads
}

type countingReader struct {
	c       *countingReaders
	account string
}

func (r countingReader) FetchTicker(context.Context, string) (ccxt.Ticker, error) {
	return ccxt.Ticker{}, errors.New("not used")
}

func (r countingReader) FetchTickers(context.Context, []string) (map[string]ccxt.Ticker, error) {
	return nil, errors.New("not used")
}

func (r countingReader) FetchOHLCV(_ context.Context, _, _ string, since *int64, _ int) ([]ccxt.OHLCV, error) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	r.c.reads++
	if since != nil {
		return nil, errors.New("since must not be passed: the cache cannot answer it")
	}
	return r.c.bars[r.account], nil
}

func newMarketPrices(rs sources.MarketReaders, perMinute int, accounts ...broker.AccountRef) marketPrices {
	return marketPrices{src: sources.NewMarketSource(rs, nil, sources.MarketOptions{ReadsPerMinute: perMinute}), accounts: accounts}
}

func TestMarketPrices_ReadsThroughTheSourceAndTriesAccountsInTurn(t *testing.T) {
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	rs := &countingReaders{
		errs: map[string]error{"a": sources.ErrNotPermitted},
		bars: map[string][]ccxt.OHLCV{"b": {{Timestamp: t0.UnixMilli(), Open: 1, High: 2, Low: 1, Close: 1.5, Volume: 1}}},
	}
	mp := newMarketPrices(rs, 30, broker.AccountRef{ProviderID: "okx", Account: "a"}, broker.AccountRef{ProviderID: "okx", Account: "b"})

	bars, err := mp.HourlyBars(context.Background(), "", "", "SOL/USDT", 240)
	if err != nil || len(bars) != 1 || !bars[0].Open.Equal(t0) || bars[0].Close != 1.5 {
		t.Fatalf("bars = %+v, err = %v, want the second account's bar", bars, err)
	}
	// With the alert's own account named, only that account is tried.
	rs.errs = nil
	if _, err := mp.HourlyBars(context.Background(), "okx", "a", "SOL/USDT", 240); err == nil {
		t.Fatal("account a has no bars, but no error came back")
	}
	if got, _ := rs.count(); got != 3 {
		t.Fatalf("readers asked for: %v", rs.resolved)
	}
}

// A read the source declines (the budget is spent) is deferred, not a failure, and
// reaches no exchange.
func TestMarketPrices_DeclinedReadIsDeferred(t *testing.T) {
	rs := &countingReaders{bars: map[string][]ccxt.OHLCV{"a": {{Timestamp: 1000, Close: 1, Open: 1, High: 1, Low: 1}}}}
	mp := newMarketPrices(rs, 1, broker.AccountRef{ProviderID: "okx", Account: "a"})
	if _, err := mp.HourlyBars(context.Background(), "", "", "SOL/USDT", 240); err != nil {
		t.Fatal(err)
	}
	_, err := mp.HourlyBars(context.Background(), "", "", "ETH/USDT", 240)
	if !errors.Is(err, feedback.ErrDeferred) {
		t.Fatalf("err = %v, want ErrDeferred once the read budget is spent", err)
	}
	if _, reads := rs.count(); reads != 1 {
		t.Fatalf("%d reads, want 1: the declined one reached no exchange", reads)
	}
	// No configured account to read through is a failure to price, not a deferral.
	if _, err := newMarketPrices(rs, 30).HourlyBars(context.Background(), "", "", "SOL/USDT", 240); err == nil || errors.Is(err, feedback.ErrDeferred) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

// With learning off the recorder is not built, so the hindsight job does not exist and
// no price is asked for, even with a market source at hand.
func TestHindsight_LearningOffMakesNoFetch(t *testing.T) {
	rs := &countingReaders{}
	pr := &perceptionRuntime{market: sources.NewMarketSource(rs, nil, sources.MarketOptions{})}
	cfg := config.DefaultConfig()
	cfg.Proactive.Enabled = true
	if got := buildFeedback(assemblyDeps{Cfg: cfg}, nil, nil, pr); got != nil {
		t.Fatal("a recorder was built with learning off")
	}
	if resolved, reads := rs.count(); resolved != 0 || reads != 0 {
		t.Fatalf("%d readers resolved and %d reads with learning off", resolved, reads)
	}
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// The snapshot view returns the latest automatic snapshot in the tolerance, only an
// automatic one, with its own quote.
func TestSnapshotValuations_LatestAutoInTheTolerance(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for _, s := range []snapshot.Snapshot{
		{TakenAt: at.Add(-90 * time.Minute), Quote: "USDT", TotalValue: 100},
		{TakenAt: at.Add(-30 * time.Minute), Quote: "USDT", TotalValue: 110},
		{TakenAt: at.Add(10 * time.Minute), Quote: "USDT", TotalValue: 999}, // after the target
		{TakenAt: at.Add(-3 * time.Hour), Quote: "USDT", TotalValue: 50},    // outside the tolerance
	} {
		if _, err := store.SaveAutoSnapshot(ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	// The owner's own snapshot, newer than the automatic ones, is not an automatic one.
	if _, err := store.SaveSnapshot(ctx, &snapshot.Snapshot{TakenAt: at.Add(-5 * time.Minute), Quote: "THB", TotalValue: 7, Label: "daily"}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := snapshotValuations{store: store}.AutoValuation(ctx, at, 2*time.Hour)
	if err != nil || !ok || v.Total != 110 || v.Quote != "USDT" || !v.At.Equal(at.Add(-30*time.Minute)) {
		t.Fatalf("valuation = %+v ok %v err %v, want the 110 USDT snapshot", v, ok, err)
	}
	if _, ok, _ := (snapshotValuations{store: store}).AutoValuation(ctx, at.Add(-5*time.Hour), 2*time.Hour); ok {
		t.Fatal("a snapshot was found where there is none within the tolerance")
	}
}
