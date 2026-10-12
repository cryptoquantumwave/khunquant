package snapshot

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/exchanges"
)

// collectorPartialExchange adds PartialWalletExchange to the wallet fake: an
// "all" request returns what succeeded plus the wallet types that failed.
type collectorPartialExchange struct {
	collectorWalletExchange
	failures []exchanges.WalletFailure
}

func (s *collectorPartialExchange) GetWalletBalancesPartial(_ context.Context, walletType string) ([]exchanges.WalletBalance, []exchanges.WalletFailure, error) {
	bal, err := s.GetWalletBalances(context.Background(), walletType)
	return bal, s.failures, err
}

func webullCfgForAccounts(names ...string) *config.Config {
	var accounts []config.WebullExchangeAccount
	for _, n := range names {
		accounts = append(accounts, config.WebullExchangeAccount{ExchangeAccount: config.ExchangeAccount{Name: n}})
	}
	return &config.Config{Exchanges: config.ExchangesConfig{
		Webull: config.WebullExchangeConfig{Enabled: true, Accounts: accounts},
	}}
}

// TestCollect_ErrorsByAccount: every failure is attributed to its account, and
// an account with any failure never gets a total, so a partial valuation cannot
// be mistaken for a loss.
func TestCollect_ErrorsByAccount(t *testing.T) {
	const (
		good     = "cf-eba-good"
		walletEr = "cf-eba-wallet-error"
		unpriced = "cf-eba-unpriced"
		partial  = "cf-eba-partial-wallet"
		empty    = "cf-eba-empty"
	)
	usdt := func(free float64) []exchanges.WalletBalance {
		return []exchanges.WalletBalance{{Balance: exchanges.Balance{Asset: "USDT", Free: free}, WalletType: "spot"}}
	}
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		switch accountName {
		case good:
			return &collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{
					{Balance: exchanges.Balance{Asset: "USDT", Free: 400}, WalletType: "spot"},
					{Balance: exchanges.Balance{Asset: "AAPL", Free: 2}, WalletType: "stock"},
				},
				prices: map[string]float64{"USDT": 0, "AAPL": 100},
			}, nil
		case walletEr:
			return &collectorWalletExchange{walletTypes: []string{"all"}, allErr: errors.New("rate limited")}, nil
		case unpriced:
			return &collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: append(usdt(100), exchanges.WalletBalance{Balance: exchanges.Balance{Asset: "ZZZ", Free: 5}, WalletType: "stock"}),
				prices:      map[string]float64{"USDT": 0},
			}, nil
		case partial:
			return &collectorPartialExchange{
				collectorWalletExchange: collectorWalletExchange{
					walletTypes: []string{"all", "spot", "funding"},
					allBalances: usdt(250),
					prices:      map[string]float64{"USDT": 0},
				},
				failures: []exchanges.WalletFailure{{WalletType: "funding", Err: errors.New("no permission")}},
			}, nil
		case empty:
			return &collectorWalletExchange{walletTypes: []string{"all"}, prices: map[string]float64{}}, nil
		}
		return nil, errors.New("unexpected account " + accountName)
	})

	res, err := CollectFromExchanges(context.Background(),
		webullCfgForAccounts(good, walletEr, unpriced, partial, empty), CollectOptions{Source: "webull"})
	if err != nil {
		t.Fatalf("unexpected top-level error: %v", err)
	}

	// Succeeded accounts, including an empty one, carry a total.
	if got := res.AccountTotals["webull/"+good]; got != 600 {
		t.Errorf("good account total = %v, want 600 (400 USDT + 2 AAPL x 100)", got)
	}
	if got, ok := res.AccountTotals["webull/"+empty]; !ok || got != 0 {
		t.Errorf("an account with no balances should be sampled at 0, got %v (present=%v)", got, ok)
	}

	// Every failure is attributed; none of those accounts has a total.
	wantErr := map[string]string{
		"webull/" + walletEr: "rate limited",
		"webull/" + unpriced: "could not price: ZZZ",
	}
	for key, frag := range wantErr {
		msg, ok := res.ErrorsByAccount[key]
		if !ok || !strings.Contains(msg, frag) {
			t.Errorf("ErrorsByAccount[%q] = %q (present=%v), want it to contain %q", key, msg, ok, frag)
		}
		if _, sampled := res.AccountTotals[key]; sampled {
			t.Errorf("%s failed but still has a total (%v): a partial valuation would read as a loss", key, res.AccountTotals[key])
		}
	}
	for _, key := range []string{"webull/" + good, "webull/" + empty} {
		if msg, bad := res.ErrorsByAccount[key]; bad {
			t.Errorf("%s should have no error, got %q", key, msg)
		}
	}

	// A wallet type that fails inside an aggregate request does not exclude the
	// account: it keeps its total, the failure is listed per account separately,
	// and the wallet list says what the total covers.
	pk := "webull/" + partial
	if got, ok := res.AccountTotals[pk]; !ok || got != 250 {
		t.Errorf("the partially read account keeps its total, got %v (present=%v)", got, ok)
	}
	if _, bad := res.ErrorsByAccount[pk]; bad {
		t.Errorf("a missing wallet type is not an account failure, got %q", res.ErrorsByAccount[pk])
	}
	if msg := res.WalletErrorsByAccount[pk]; !strings.Contains(msg, "funding wallet: no permission") {
		t.Errorf("WalletErrorsByAccount[%q] = %q", pk, msg)
	}
	if got := res.AccountWallets[pk]; !reflect.DeepEqual(got, []string{"spot"}) {
		t.Errorf("AccountWallets[%q] = %v, want [spot]", pk, got)
	}
	if got := res.AccountWallets["webull/"+good]; !reflect.DeepEqual(got, []string{"all"}) {
		t.Errorf("a fully read account lists its wallets, got %v", got)
	}
	for _, key := range []string{"webull/" + walletEr, "webull/" + unpriced} {
		if _, ok := res.AccountWallets[key]; ok {
			t.Errorf("%s failed and must have no wallet list", key)
		}
	}
	found := false
	for _, e := range res.Errors {
		if strings.HasPrefix(e, pk+": ") {
			found = true
		}
	}
	if !found {
		t.Errorf("the wallet failure must still appear in Errors: %v", res.Errors)
	}

	// ErrorsByAccount is an index over Errors, not a replacement.
	if len(res.Errors) < len(wantErr) {
		t.Errorf("Errors should still list every failure, got %v", res.Errors)
	}
	for key := range wantErr {
		found := false
		for _, e := range res.Errors {
			if strings.HasPrefix(e, key+": ") {
				found = true
			}
		}
		if !found {
			t.Errorf("Errors has no entry for %s: %v", key, res.Errors)
		}
	}
}

// TestCollect_UnconvertibleQuoteWithholdsTotal: a position that cannot be
// converted to the snapshot quote would be silently dropped from the total.
func TestCollect_UnconvertibleQuoteWithholdsTotal(t *testing.T) {
	const acct = "cf-eba-noconv"
	exchanges.RegisterAccountFactory("webull", func(_ *config.Config, accountName string) (exchanges.Exchange, error) {
		return &crossRateQuoteLister{
			collectorWalletExchange: collectorWalletExchange{
				walletTypes: []string{"all"},
				allBalances: []exchanges.WalletBalance{{Balance: exchanges.Balance{Asset: "XYZ", Free: 10}, WalletType: "spot"}},
				prices:      map[string]float64{"XYZ": 3},
			},
			quotes: []string{"THB"},
		}, nil
	})
	res, err := CollectFromExchanges(context.Background(), webullCfgForAccounts(acct), CollectOptions{Source: "webull"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	key := "webull/" + acct
	if _, ok := res.AccountTotals[key]; ok {
		t.Errorf("an account with an unconvertible quote must not get a total, got %v", res.AccountTotals)
	}
	if !strings.Contains(res.ErrorsByAccount[key], "conversion rate") {
		t.Errorf("expected a conversion-rate error, got %q", res.ErrorsByAccount[key])
	}
}
