package snapshot

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// TestCollect_UnpricedAssetsAreReportedWithoutChangingTheErrors: the collector tells a
// caller which assets it could not price, and every field that existed before
// (Errors, ErrorsByAccount, WalletErrorsByAccount, AccountTotals, TotalValue) is
// exactly what it was, because take_snapshot and the loss guard read them.
func TestCollect_UnpricedAssetsAreReportedWithoutChangingTheErrors(t *testing.T) {
	const (
		dust    = "cf-up-dust"
		dust2   = "cf-up-dust2"
		down    = "cf-up-down"
		partial = "cf-up-partial"
		clean   = "cf-up-clean"
	)
	bal := func(asset string, free float64, wt string) exchanges.WalletBalance {
		return exchanges.WalletBalance{Balance: exchanges.Balance{Asset: asset, Free: free}, WalletType: wt}
	}
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		switch accountName {
		case dust: // unpriced ZZZ then AAA, in the order the wallet lists them
			return &collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{bal("USDT", 100, "spot"), bal("ZZZ", 5, "spot"), bal("AAA", 1, "spot")},
				prices:      map[string]float64{"USDT": 0},
			}, nil
		case dust2:
			return &collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{bal("USDT", 50, "spot"), bal("OLD", 3, "spot")},
				prices:      map[string]float64{"USDT": 0},
			}, nil
		case down:
			return &collectorWalletExchange{walletTypes: []string{"all"}, allErr: errors.New("rate limited")}, nil
		case partial: // an unpriced asset AND a wallet type that failed
			return &collectorPartialExchange{
				collectorWalletExchange: collectorWalletExchange{
					walletTypes: []string{"all", "spot", "funding"},
					allBalances: []exchanges.WalletBalance{bal("USDT", 10, "spot"), bal("ZZZ", 1, "spot")},
					prices:      map[string]float64{"USDT": 0},
				},
				failures: []exchanges.WalletFailure{{WalletType: "funding", Err: errors.New("no permission")}},
			}, nil
		case clean:
			return &collectorWalletExchange{
				walletTypes: []string{"all"}, allBalances: []exchanges.WalletBalance{bal("USDT", 7, "spot")},
				prices: map[string]float64{"USDT": 0},
			}, nil
		}
		return nil, errors.New("unexpected account " + accountName)
	})
	collect := func(t *testing.T, accounts ...string) *CollectResult {
		t.Helper()
		res, err := CollectFromExchanges(context.Background(), webullCfgForAccounts(accounts...), CollectOptions{Source: "webull"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	t.Run("unpriced only", func(t *testing.T) {
		res := collect(t, dust, dust2, clean)
		// The existing fields, exactly as before.
		wantErrors := []string{
			"webull/" + dust + ": could not price: ZZZ, AAA",
			"webull/" + dust2 + ": could not price: OLD",
		}
		if !reflect.DeepEqual(res.Errors, wantErrors) {
			t.Errorf("Errors = %q, want %q", res.Errors, wantErrors)
		}
		if got := res.ErrorsByAccount["webull/"+dust]; got != "could not price: ZZZ, AAA" {
			t.Errorf("ErrorsByAccount = %q", got)
		}
		if len(res.WalletErrorsByAccount) != 0 {
			t.Errorf("WalletErrorsByAccount = %v", res.WalletErrorsByAccount)
		}
		if !reflect.DeepEqual(res.AccountTotals, map[string]float64{"webull/" + clean: 7}) {
			t.Errorf("AccountTotals = %v: an account with unpriced assets has no total", res.AccountTotals)
		}
		if res.Snapshot.TotalValue != 157 {
			t.Errorf("TotalValue = %v, want 157 (the unpriced assets are not in it)", res.Snapshot.TotalValue)
		}
		// The new fields.
		wantUnpriced := []string{"webull/" + dust2 + ":OLD", "webull/" + dust + ":AAA", "webull/" + dust + ":ZZZ"} // byte order
		if got := res.Unpriced(); !reflect.DeepEqual(got, wantUnpriced) {
			t.Errorf("Unpriced() = %q, want %q", got, wantUnpriced)
		}
		if !res.OnlyUnpricedErrors() {
			t.Error("OnlyUnpricedErrors() = false for a collection whose only errors are unpriced assets")
		}
	})

	t.Run("unpriced and a read failure", func(t *testing.T) {
		res := collect(t, dust, down)
		if res.OnlyUnpricedErrors() {
			t.Fatal("OnlyUnpricedErrors() = true although an account could not be read")
		}
	})

	t.Run("unpriced and a failed wallet in the same account", func(t *testing.T) {
		res := collect(t, partial)
		if got := res.Unpriced(); !reflect.DeepEqual(got, []string{"webull/" + partial + ":ZZZ"}) {
			t.Errorf("Unpriced() = %q", got)
		}
		if res.OnlyUnpricedErrors() {
			t.Fatal("OnlyUnpricedErrors() = true although a wallet type failed")
		}
	})

	t.Run("no errors", func(t *testing.T) {
		res := collect(t, clean)
		if res.OnlyUnpricedErrors() || len(res.Unpriced()) != 0 || len(res.Errors) != 0 {
			t.Fatalf("clean collection: %+v", res)
		}
	})

	t.Run("nil", func(t *testing.T) {
		var res *CollectResult
		if res.OnlyUnpricedErrors() || res.Unpriced() != nil {
			t.Fatal("nil result must report nothing")
		}
	})
}
