package config

import (
	"encoding/json"
	"testing"
)

// TestWebullExchangeAccount_JSONRoundTrip guards the non-secret fields
// (account_id, region, environment) against being dropped on config
// persistence. These carry yaml:"-" (they are not secrets, so they must NOT
// go to .security.yml) and are persisted through the JSON config instead —
// so a JSON round-trip is the contract that matters for them.
func TestWebullExchangeAccount_JSONRoundTrip(t *testing.T) {
	acc := WebullExchangeAccount{
		ExchangeAccount: ExchangeAccount{Name: "main"},
		AccountID:       "acct-1",
		Region:          "us",
		Environment:     "uat",
	}

	data, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got WebullExchangeAccount
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if got.AccountID != "acct-1" {
		t.Errorf("AccountID dropped: got %q, want acct-1 (json: %s)", got.AccountID, data)
	}
	if got.Region != "us" {
		t.Errorf("Region dropped: got %q, want us (json: %s)", got.Region, data)
	}
	if got.Environment != "uat" {
		t.Errorf("Environment dropped: got %q, want uat (json: %s)", got.Environment, data)
	}
}
