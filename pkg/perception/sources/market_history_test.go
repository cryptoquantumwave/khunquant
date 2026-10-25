package sources_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	ccxt "github.com/ccxt/ccxt/go/v4"

	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
)

// ReadBars goes through the source's reader, its read budget and its backoff, and
// cleans the bars as the poll does.
func TestMarketSource_ReadBars(t *testing.T) {
	r := newRig(t, sources.MarketOptions{ReadsPerMinute: 4})
	t0 := r.clock.Now().Truncate(time.Hour)
	r.reader.setBars("BTC/USDT|1h", bar(t0, 10), bar(t0.Add(-time.Hour), 9),
		ccxt.OHLCV{Timestamp: t0.Add(time.Hour).UnixMilli(), Close: math.NaN()}, bar(t0.Add(2*time.Hour), 0))
	ctx := context.Background()

	bars, err := r.src.ReadBars(ctx, "binance", "a", "BTC/USDT", "1h", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || bars[0].OpenTime != t0.Add(-time.Hour).UnixMilli() || bars[1].Close != 10 {
		t.Fatalf("bars = %+v, want the two good bars, oldest first", bars)
	}
	if got := r.reader.limits; len(got) != 1 || got[0] != 100 {
		t.Fatalf("limits = %v, want one read of 100 bars", got)
	}
	if st := r.src.Stats(); st.OHLCVRequests != 1 || st.DroppedBad != 2 {
		t.Fatalf("stats = %+v, want one OHLCV request and two dropped bars", st)
	}

	// The budget is the poll's, and a caller that is not a trigger leaves half of it to the
	// poll: of four a minute it gets two, and the third is declined, not failed.
	if _, err := r.src.ReadBars(ctx, "binance", "a", "BTC/USDT", "1h", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := r.src.ReadBars(ctx, "binance", "a", "BTC/USDT", "1h", 100); !errors.Is(err, sources.ErrReadBudget) {
		t.Fatalf("err = %v, want ErrReadBudget", err)
	}
	if got := r.reader.requests(); got != 2 {
		t.Fatalf("%d requests, want 2: a declined read reaches no exchange", got)
	}
	// Another provider has its own budget.
	if _, err := r.src.ReadBars(ctx, "okx", "a", "BTC/USDT", "1h", 100); err != nil {
		t.Fatalf("okx: %v", err)
	}
}

func TestMarketSource_ReadBarsRespectsPermissionAndBackoff(t *testing.T) {
	r := newRig(t, sources.MarketOptions{})
	ctx := context.Background()
	if _, err := r.src.ReadBars(ctx, "binance", "nobody", "BTC/USDT", "1h", 10); !errors.Is(err, sources.ErrUnsupported) {
		t.Fatalf("err = %v, want the reader's refusal passed on", err)
	}
	if got := r.reader.requests(); got != 0 {
		t.Fatalf("%d requests for an account that cannot be read", got)
	}

	r.reader.setFailure(ccxt.RateLimitExceeded("binance GET 429"))
	if _, err := r.src.ReadBars(ctx, "binance", "a", "BTC/USDT", "1h", 10); !errors.Is(err, sources.ErrRateLimited) {
		t.Fatalf("err = %v, want a rate-limit error", err)
	}
	// It reports pushback but does not start a backoff of its own.
	if st := r.src.Stats(); st.BackedOffProvider != 0 || st.RateLimited != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if got := r.reader.limits; len(got) != 0 {
		t.Fatalf("limits = %v", got)
	}
	var nilSrc *sources.MarketSource
	if _, err := nilSrc.ReadBars(ctx, "binance", "a", "BTC/USDT", "1h", 10); err == nil {
		t.Fatal("a nil source read something")
	}
}
