package sources_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// unpricedResult is a collection in which two assets, on two accounts, could not be
// priced and that is the only thing wrong with it.
func unpricedResult() *snapshot.CollectResult {
	r := completeResult()
	r.Errors = []string{"binance/main: could not price: ZZZ", "okx/main: could not price: OLD"}
	r.ErrorsByAccount = map[string]string{"binance/main": "could not price: ZZZ", "okx/main": "could not price: OLD"}
	r.UnpricedByAccount = map[string][]string{"binance/main": {"ZZZ"}, "okx/main": {"OLD"}}
	return r
}

func TestPortfolioSource_UnpricedAssetsAloneDoNotSuppressTheSnapshot(t *testing.T) {
	r := newPortfolioRig(t, ok(unpricedResult()))
	waitFor(t, "the snapshot", func() bool { return len(r.out.events()) == 1 })
	ev := r.out.events()[0]
	if ev.Kind != events.KindPerceptionPortfolioSnapshot {
		t.Fatalf("event = %s, want a snapshot: a dust token must not suppress every snapshot", ev.Kind)
	}
	p := ev.Payload.(sources.PortfolioSnapshotPayload)
	if want := []string{"binance/main:ZZZ", "okx/main:OLD"}; !reflect.DeepEqual(p.Unpriced, want) {
		t.Fatalf("payload Unpriced = %q, want %q", p.Unpriced, want)
	}
	if p.TotalValueUSD != 1036 || p.SnapshotID != 1 {
		t.Errorf("payload = %+v", p)
	}
	saved, _ := r.store.counts()
	if saved != 1 {
		t.Fatalf("saved = %d, want 1", saved)
	}
	if note := r.store.saved[0].Note; !reflect.DeepEqual(snapshot.UnpricedFromNote(note), p.Unpriced) {
		t.Errorf("the saved note %q does not carry the list", note)
	}
	if h := r.src.Health(); h.ConsecutiveErrors != 0 {
		t.Errorf("health = %+v: unpriced assets alone are not a failure", h)
	}
	if st := r.src.Stats(); st.WithUnpriced != 1 || st.Saved != 1 || st.Partial != 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestPortfolioSource_UnpricedPlusAReadFailureIsDiscarded(t *testing.T) {
	res := unpricedResult()
	res.Errors = append(res.Errors, "bitkub/1: get wallet balances: timeout")
	res.ErrorsByAccount["bitkub/1"] = "get wallet balances: timeout"
	r := newPortfolioRig(t, ok(res))
	waitFor(t, "the degraded signal", func() bool { return len(r.out.events()) == 1 })
	if k := r.out.events()[0].Kind; k != events.KindPerceptionSourceHealth {
		t.Fatalf("event = %s, want the degraded signal", k)
	}
	if saved, _ := r.store.counts(); saved != 0 {
		t.Fatal("a collection with a failed account was saved")
	}
}

func TestPortfolioSource_UnpricedPlusAMissingRateIsDiscarded(t *testing.T) {
	res := unpricedResult()
	res.Errors = append(res.Errors, "bitkub/1: no USDT conversion rate for THB")
	res.ErrorsByAccount["bitkub/1"] = "no USDT conversion rate for THB"
	r := newPortfolioRig(t, ok(res))
	waitFor(t, "the degraded signal", func() bool { return len(r.out.events()) == 1 })
	if k := r.out.events()[0].Kind; k != events.KindPerceptionSourceHealth {
		t.Fatalf("event = %s", k)
	}
	if saved, _ := r.store.counts(); saved != 0 {
		t.Fatal("saved")
	}
}

func TestPortfolioSource_TheUnpricedListRoundTripsThroughTheStore(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	src := sources.NewPortfolio(sources.PortfolioOptions{
		Cfg: &config.Config{}, Collect: (&fakeCollect{steps: []func() (*snapshot.CollectResult, error){ok(unpricedResult())}}).collect,
		Store: store, Clock: perceptiontest.NewClock(epoch), InitialDelay: -1,
	})
	out := &collector{}
	if err := src.Reconcile(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	payload := out.events()[0].Payload.(sources.PortfolioSnapshotPayload)

	rows, err := store.QuerySnapshots(context.Background(), snapshot.QueryFilter{Label: snapshot.LabelAuto})
	if err != nil || len(rows) != 1 {
		t.Fatalf("auto rows = %+v, %v", rows, err)
	}
	if got := snapshot.UnpricedFromNote(rows[0].Note); !reflect.DeepEqual(got, payload.Unpriced) || len(got) != 2 {
		t.Fatalf("the stored list %q differs from the event's %q", got, payload.Unpriced)
	}
	if !snapshot.IsAuto(rows[0].Label, rows[0].Note) {
		t.Fatal("the row with an unpriced list is not recognised as automatic")
	}
	if n, err := store.PruneAuto(context.Background(), 30); err != nil || n != 0 {
		t.Fatalf("a fresh row was pruned (%d, %v)", n, err)
	}
	if other, _ := store.QuerySnapshots(context.Background(), snapshot.QueryFilter{}); len(other) != 0 {
		t.Fatalf("the row shows in the owner's listing: %+v", other)
	}
}
