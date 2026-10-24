package triggers

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex"
	"github.com/cryptoquantumwave/khunquant/pkg/worldmodel"
)

// BenchmarkRegistry_EvaluateNoMatchingTrigger is the cost of one event of a kind that
// no trigger lists, with triggers of another kind present: a market tick on a gateway
// whose triggers are all about portfolios. The global mode function is deliberately
// one that would cost something (it is not called).
func BenchmarkRegistry_EvaluateNoMatchingTrigger(b *testing.B) {
	db, err := sqlitex.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	r, err := NewRegistry(context.Background(), db, func() string { return ModeLive },
		map[string]Factory{testKind: factoryOf(testKind, []events.Kind{kindPort}, fires(testKind), nil)})
	if err != nil {
		b.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := r.Add(context.Background(), Spec{ID: id, Kind: testKind, Mode: ModeLive, Origin: "bench"}); err != nil {
			b.Fatal(err)
		}
	}
	var v worldmodel.View = fakeView{now: noon}
	ctx, d := context.Background(), worldmodel.Diff{Event: events.Event{Kind: kindTick}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cs, _ := r.Evaluate(ctx, v, d)
		r.Committed(ctx, nil)
		if len(cs) != 0 {
			b.Fatal("a trigger ran")
		}
	}
}
