package bitkub

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox/market"
)

func repoDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// sandboxAdapter is the real adapter talking to a sandbox server whose market stage is
// the book, with the repo's recorded Bitkub fixtures behind it.
func sandboxAdapter(t *testing.T, book *market.Book) *BitkubBrokerAdapter {
	t.Helper()
	store := sandbox.NewStore()
	if err := store.Load(filepath.Join(repoDir(t), "workspace", "sandbox")); err != nil {
		t.Fatal(err)
	}
	if len(store.GetFixtures("bitkub")) == 0 {
		t.Fatal("no bitkub fixtures loaded: the fixtures path is wrong")
	}
	chain := sandbox.NewChain(store)
	chain.Set(sandbox.StageMarket, book.Middleware())
	srv := sandbox.NewServer(store)
	srv.SetResponder(chain)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	a, err := newBrokerAdapter(config.ExchangeAccount{})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The recorded ticker fixture must be found under the symbol the adapter asks for.
func TestSandboxFixture_TickerUsesAdapterSymbol(t *testing.T) {
	a := sandboxAdapter(t, market.NewBook(market.Options{}))
	tk, err := a.FetchTicker(context.Background(), "BTC/THB")
	if err != nil || tk.Last == nil || *tk.Last != 1234567.89 {
		t.Fatalf("FetchTicker(BTC/THB) from the fixture = %+v (%v)", tk, err)
	}
	if px, err := a.FetchPrice(context.Background(), "BTC", "THB"); err != nil || px != 1234567.89 {
		t.Errorf("FetchPrice(BTC, THB) = %v (%v)", px, err)
	}
}

func TestRender_Bitkub_TVHistory(t *testing.T) {
	book := market.NewBook(market.Options{})
	btcthb := market.Pair{Base: "BTC", Quote: "THB"}
	if err := book.Set("", btcthb, 3_000_000); err != nil {
		t.Fatal(err)
	}
	if err := book.Path("bitkub", btcthb, 3_300_000, 10*time.Minute, market.ShapeLinear, nil); err != nil {
		t.Fatal(err)
	}
	a := sandboxAdapter(t, book)
	ctx := context.Background()

	tk, err := a.FetchTicker(ctx, "BTC/THB")
	want, _ := book.Price("bitkub", btcthb, time.Now())
	if err != nil || tk.Last == nil || *tk.Last < 3_000_000 || *tk.Last > want+1000 || *tk.Last == 1234567.89 {
		t.Fatalf("FetchTicker = %+v (%v), want the book's price (about %v), not the fixture's", tk, err, want)
	}
	bars, err := a.FetchOHLCV(ctx, "BTC/THB", "1m", nil, 5)
	if err != nil || len(bars) < 5 {
		t.Fatalf("FetchOHLCV = %d bars (%v), want at least 5", len(bars), err)
	}
	last := bars[len(bars)-1]
	if last.Timestamp != time.Now().Truncate(time.Minute).UnixMilli() && last.Timestamp != time.Now().Add(-time.Second).Truncate(time.Minute).UnixMilli() {
		t.Errorf("last bar opens at %d, want the minute now falls in", last.Timestamp)
	}
	if last.Close < 3_000_000 || last.Close > 3_300_000 || last.High < last.Low {
		t.Errorf("last bar = %+v", last)
	}
	for i := 1; i < len(bars); i++ {
		if bars[i].Timestamp-bars[i-1].Timestamp != 60000 {
			t.Errorf("bars %d..%d are not consecutive minutes", i-1, i)
		}
	}

	// A delisted pair is omitted from the ticker array and has no history.
	if err := book.Delist("bitkub", "BTC_THB"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FetchTicker(ctx, "BTC/THB"); err == nil {
		t.Error("a delisted symbol still has a ticker")
	}
	if _, err := a.FetchOHLCV(ctx, "BTC/THB", "1m", nil, 5); err == nil {
		t.Error("a delisted symbol still has history")
	}
}
