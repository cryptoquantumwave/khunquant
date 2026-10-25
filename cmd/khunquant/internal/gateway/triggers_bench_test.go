package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// BenchmarkWorldModelStage_TickInsideABucket is the per-tick cost of the world model
// stage, which now returns the (empty) Diff boxed instead of nil.
func BenchmarkWorldModelStage_TickInsideABucket(b *testing.B) {
	store, err := worldmodel.NewStore(context.Background(), nil, perception.RealClock{})
	if err != nil {
		b.Fatal(err)
	}
	stage := worldModelStage{s: store}
	t0 := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		at := t0.Add(time.Duration(i) * time.Microsecond) // monotonic, inside one minute bucket
		_, _ = stage.Apply(ctx, events.Event{
			Kind: events.KindPerceptionMarketTick, Time: at,
			Payload: sources.MarketTick{Provider: "binance", Symbol: "BTC/USDT", Last: 62000, At: at},
		})
	}
}
