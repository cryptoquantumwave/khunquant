package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
	"github.com/cryptoquantumwave/khunquant/pkg/snapshot"
)

// usdtWallet is an exchange holding a fixed amount of USDT: the minimum take_snapshot
// needs to produce a priced total.
type usdtWallet struct{ free float64 }

func (usdtWallet) Name() string { return "webull" }
func (usdtWallet) GetBalances(context.Context) ([]exchanges.Balance, error) {
	return nil, nil
}
func (usdtWallet) SupportedWalletTypes() []string { return []string{"all"} }
func (w usdtWallet) GetWalletBalances(context.Context, string) ([]exchanges.WalletBalance, error) {
	return []exchanges.WalletBalance{{Balance: exchanges.Balance{Asset: "USDT", Free: w.free}, WalletType: "spot"}}, nil
}
func (usdtWallet) FetchPrice(context.Context, string, string) (float64, error) { return 0, nil }

func autoTestSave(t *testing.T, store *snapshot.Store, at time.Time, total float64, auto bool, label string) int64 {
	t.Helper()
	snap := &snapshot.Snapshot{TakenAt: at, Quote: "USDT", TotalValue: total, Label: label}
	var id int64
	var err error
	if auto {
		id, err = store.SaveAutoSnapshot(context.Background(), snap)
	} else {
		id, err = store.SaveSnapshot(context.Background(), snap)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// take_snapshot's percent change is against the previous snapshot the owner or the
// model took, never an automatic one (which is newer, in a different cadence).
func TestTakeSnapshot_PercentChangeIgnoresAutomaticSnapshots(t *testing.T) {
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, _ string) (exchanges.Exchange, error) {
		return usdtWallet{free: 110}, nil
	})
	cfg := &config.Config{Exchanges: config.ExchangesConfig{Webull: config.WebullExchangeConfig{
		Enabled:  true,
		Accounts: []config.WebullExchangeAccount{{ExchangeAccount: config.ExchangeAccount{Name: "snap-auto"}}},
	}}}
	store := newTestSnapshotStore(t)
	now := time.Now().UTC()
	autoTestSave(t, store, now.Add(-48*time.Hour), 100, false, "daily")
	autoTestSave(t, store, now.Add(-time.Hour), 1000, true, "")

	res := NewTakeSnapshotTool(cfg, store).Execute(context.Background(), map[string]any{})
	if res.IsError {
		t.Fatalf("take_snapshot: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "total value 110.00 USDT (+10.00%)") {
		t.Fatalf("the change is not against the owner's snapshot (100 -> 110): %s", res.ForLLM)
	}
}

func TestTakeSnapshot_RefusesTheReservedLabel(t *testing.T) {
	store := newTestSnapshotStore(t)
	res := NewTakeSnapshotTool(config.DefaultConfig(), store).Execute(context.Background(), map[string]any{"label": " Auto "})
	if !res.IsError || !strings.Contains(res.ForLLM, "reserved") {
		t.Fatalf("result = %+v", res)
	}
}

func TestQuerySnapshotsAndSummary_ShowAutomaticRowsOnlyForTheAutoLabel(t *testing.T) {
	store := newTestSnapshotStore(t)
	now := time.Now().UTC()
	ownerID := autoTestSave(t, store, now.Add(-2*time.Hour), 111, false, "daily")
	autoID := autoTestSave(t, store, now.Add(-time.Hour), 999, true, "")
	ctx := context.Background()

	q := NewQuerySnapshotsTool(store)
	res := q.Execute(ctx, map[string]any{})
	if !strings.Contains(res.ForLLM, "111.00") || strings.Contains(res.ForLLM, "999.00") || strings.Contains(res.ForLLM, "auto") {
		t.Errorf("default listing: %s", res.ForLLM)
	}
	res = q.Execute(ctx, map[string]any{"label": "auto"})
	if !strings.Contains(res.ForLLM, "999.00") || strings.Contains(res.ForLLM, "111.00") {
		t.Errorf("label=auto listing: %s", res.ForLLM)
	}
	_, _ = ownerID, autoID

	s := NewSnapshotSummaryTool(store)
	res = s.Execute(ctx, map[string]any{})
	if !strings.Contains(res.ForLLM, "Snapshots:    1") || !strings.Contains(res.ForLLM, "111.00") || strings.Contains(res.ForLLM, "999.00") {
		t.Errorf("default summary: %s", res.ForLLM)
	}
	res = s.Execute(ctx, map[string]any{"label": "auto"})
	if !strings.Contains(res.ForLLM, "Snapshots:    1") || !strings.Contains(res.ForLLM, "999.00") {
		t.Errorf("label=auto summary: %s", res.ForLLM)
	}

	// The parameter descriptions say so.
	for _, tool := range []Tool{q, s} {
		desc := tool.Parameters()["properties"].(map[string]any)["label"].(map[string]any)["description"].(string)
		if !strings.Contains(desc, `"auto"`) {
			t.Errorf("%s: label description %q does not mention the auto label", tool.Name(), desc)
		}
	}
}
