package settrade

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// A client built while the sandbox is on never makes the NTP query (a raw UDP dial that
// leaves the machine); one built with it off still does.
func TestNewSettradeClient_NoNTPUnderTheSandbox(t *testing.T) {
	calls := 0
	old := ntpSync
	ntpSync = func() int64 { calls++; return 0 }
	t.Cleanup(func() { ntpSync = old; sandbox.SetGlobalState(false, "") })

	acc := config.SettradeExchangeAccount{BrokerID: sandbox.PlaceholderBrokerID, AppCode: sandbox.PlaceholderAppCode, AccountNo: sandbox.PlaceholderAccountNo}
	acc.APIKey = *config.NewSecureString(sandbox.PlaceholderKey)
	acc.Secret = *config.NewSecureString(sandbox.PlaceholderSettradeSecret)

	sandbox.SetGlobalState(true, "http://127.0.0.1:1")
	if _, err := NewSettradeClient(acc); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("NTP was queried %d times under the sandbox", calls)
	}
	sandbox.SetGlobalState(false, "")
	if _, err := NewSettradeClient(acc); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("NTP was queried %d times with the sandbox off, want 1: production is unchanged", calls)
	}
}
