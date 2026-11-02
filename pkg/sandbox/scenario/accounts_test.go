package scenario

import (
	"strings"
	"testing"
)

// instance.accounts asks for a placeholder account on a venue with a simulator; a venue
// without one, a venue twice, and a config patch that would replace the account (in the
// instance block or in a config.patch step) are refused.
func TestValidate_InstanceAccounts(t *testing.T) {
	cases := []struct {
		name, src, path, want string
	}{
		{"binance and okx", "instance: {accounts: [binance, okx]}\n", "", ""},
		{"no simulator", "instance: {accounts: [bitkub]}\n", "/instance/accounts", "bitkub"},
		{"twice", "instance: {accounts: [okx, okx]}\n", "/instance/accounts", "twice"},
		{"patch replaces", "instance: {accounts: [okx], config: {exchanges: {okx: {accounts: [{name: main}]}}}}\n", "/instance/config/exchanges", "placeholder"},
		{"patch elsewhere", "instance: {accounts: [okx], config: {exchanges: {binance: {accounts: [{name: main}]}}}}\n", "", ""},
		{"patch toggles", "instance: {accounts: [okx], config: {exchanges: {okx: {testnet: true}}}}\n", "", ""},
		{"step replaces", "instance: {accounts: [okx]}\ntimeline:\n  - config.patch: {exchanges: {okx: {accounts: []}}}\n", "/timeline/0/config.patch/exchanges", "placeholder"},
		{"credential in the scenario", "instance: {accounts: [okx], config: {exchanges: {okx: {accounts: [{api_key: x}]}}}}\n", "/instance/config/exchanges/okx/accounts/0/api_key", "credential"},
	}
	for _, c := range cases {
		sc := parseString(t, "version: 1\nscenario: t\nclock: {start: \"2026-10-20T09:00:00+07:00\"}\n"+withExpect(c.src))
		issues := Validate(sc, nil)
		if c.want == "" {
			if len(issues) != 0 {
				t.Errorf("%s: issues %v", c.name, issues)
			}
			continue
		}
		if got := issueAt(issues, c.path); got == "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: issue at %s = %q, want one with %q (all: %v)", c.name, c.path, got, c.want, issues)
		}
	}
}
