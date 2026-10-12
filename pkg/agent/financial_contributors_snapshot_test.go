package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// The portfolio section of a user turn's prompt shows the latest snapshot the owner
// or the model took. Switching perception on adds automatic snapshots (always USDT,
// newer, every 15 minutes); the section must not change content or quote because of
// them.
func TestPortfolioContributor_IgnoresAutomaticSnapshots(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := store.SaveSnapshot(ctx, &snapshot.Snapshot{
		TakenAt: now.Add(-3 * time.Hour), Quote: "THB", TotalValue: 4321, Label: "daily",
		Positions: []snapshot.Position{{Source: "bitkub", Asset: "BTC", Quantity: 1, Quote: "THB", Value: 4321}},
	}); err != nil {
		t.Fatal(err)
	}
	c := &portfolioContributor{store: store, maxAssets: 5}
	before, err := c.Section(ctx)
	if err != nil || !strings.Contains(before, "THB") || !strings.Contains(before, "4321.00") {
		t.Fatalf("section = %q, %v", before, err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.SaveAutoSnapshot(ctx, &snapshot.Snapshot{
			TakenAt: now.Add(-time.Duration(i) * time.Minute), Quote: "USDT", TotalValue: 99999,
			Positions: []snapshot.Position{{Source: "binance", Asset: "ETH", Quantity: 1, Quote: "USDT", Value: 99999}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := c.Section(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("the portfolio section changed when automatic snapshots appeared:\nbefore: %q\nafter:  %q", before, after)
	}
}

// With only automatic snapshots the section stays empty, as it was before them.
func TestPortfolioContributor_EmptyWithOnlyAutomaticSnapshots(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.SaveAutoSnapshot(context.Background(), &snapshot.Snapshot{TakenAt: time.Now(), Quote: "USDT", TotalValue: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := (&portfolioContributor{store: store, maxAssets: 5}).Section(context.Background())
	if err != nil || got != "" {
		t.Fatalf("section = %q, %v", got, err)
	}
}
