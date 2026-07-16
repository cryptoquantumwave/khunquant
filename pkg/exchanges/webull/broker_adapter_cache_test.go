package webull

import (
	"sync"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

// TestNewBrokerAdapterCachesByAccountName verifies that repeated calls to
// newBrokerAdapter for the same account name return the exact same instance
// (and therefore the same underlying Client/token session), rather than
// minting a fresh, unapproved Webull login on every call. Market-data and
// trading tools go through broker.CreateProviderForAccount, which does not
// cache on its own — this memoization is what makes them reuse the same
// approved session as portfolio tools instead of triggering a new
// token/create (and burning through Webull's login-attempt rate limit) on
// every single call.
func TestNewBrokerAdapterCachesByAccountName(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "cache-test-shared",
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
	}

	a1, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter #1: %v", err)
	}
	a2, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter #2: %v", err)
	}
	if a1 != a2 {
		t.Fatalf("expected cached adapter to be reused for the same account name, got distinct instances")
	}
	if a1.client != a2.client {
		t.Fatalf("expected cached adapter to share the same Client (and token session)")
	}
}

// TestNewBrokerAdapterRebuildsOnConfigChange verifies the cache is
// invalidated when the account's config values change under the same name —
// the user edits api_key/region on the web UI and clicks Connect; returning
// the adapter built from the old config would keep failing against stale
// credentials with no indication why.
func TestNewBrokerAdapterRebuildsOnConfigChange(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "cache-test-fingerprint",
			APIKey: *config.NewSecureString("old-key"),
			Secret: *config.NewSecureString("old-secret"),
		},
	}

	a1, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter #1: %v", err)
	}

	cfg.APIKey = *config.NewSecureString("new-key")
	a2, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter #2: %v", err)
	}
	if a1 == a2 {
		t.Fatal("expected a rebuilt adapter after the api_key changed, got the stale cached one")
	}

	// Unchanged config after the rebuild hits the cache again.
	a3, err := newBrokerAdapter(cfg)
	if err != nil {
		t.Fatalf("newBrokerAdapter #3: %v", err)
	}
	if a2 != a3 {
		t.Fatal("expected the rebuilt adapter to be cached for subsequent identical configs")
	}
}

// TestNewBrokerAdapterCacheIsolatesDifferentAccounts verifies distinct
// account names get distinct adapters/clients (no cross-account bleed).
func TestNewBrokerAdapterCacheIsolatesDifferentAccounts(t *testing.T) {
	cfgA := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "cache-test-account-a",
			APIKey: *config.NewSecureString("test-app-key-a"),
			Secret: *config.NewSecureString("test-app-secret-a"),
		},
	}
	cfgB := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "cache-test-account-b",
			APIKey: *config.NewSecureString("test-app-key-b"),
			Secret: *config.NewSecureString("test-app-secret-b"),
		},
	}

	a, err := newBrokerAdapter(cfgA)
	if err != nil {
		t.Fatalf("newBrokerAdapter A: %v", err)
	}
	b, err := newBrokerAdapter(cfgB)
	if err != nil {
		t.Fatalf("newBrokerAdapter B: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct accounts to get distinct adapters")
	}
}

// TestNewBrokerAdapterCacheConcurrentSafe exercises the cache under
// concurrent access (run with -race) to confirm no data race and that all
// callers converge on a single shared instance.
func TestNewBrokerAdapterCacheConcurrentSafe(t *testing.T) {
	cfg := config.WebullExchangeAccount{
		ExchangeAccount: config.ExchangeAccount{
			Name:   "cache-test-concurrent",
			APIKey: *config.NewSecureString("test-app-key"),
			Secret: *config.NewSecureString("test-app-secret"),
		},
	}

	const n = 20
	results := make([]*webullAdapter, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			a, err := newBrokerAdapter(cfg)
			if err != nil {
				t.Errorf("newBrokerAdapter: %v", err)
				return
			}
			results[i] = a
		}(i)
	}
	wg.Wait()

	first := results[0]
	for i, a := range results {
		if a != first {
			t.Fatalf("goroutine %d got a different adapter instance than goroutine 0", i)
		}
	}
}
