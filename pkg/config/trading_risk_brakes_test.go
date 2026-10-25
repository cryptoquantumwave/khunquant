package config

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The policy engine's brakes are off by default: a fresh config carries none of them.
func TestTradingRiskBrakes_OffByDefault(t *testing.T) {
	d := DefaultConfig().TradingRisk
	if d.MaxOrdersPer10Min != 0 || d.MaxSymbolSharePct != 0 || len(d.SymbolAllowlist) != 0 {
		t.Fatalf("a brake is on by default: %+v", d)
	}
	if err := ValidateTradingRisk(d); err != nil {
		t.Fatalf("the defaults are invalid: %v", err)
	}
	if got := DefaultConfig().Proactive.Effective().PolicyMode; got != ProactiveModeOff {
		t.Fatalf("policy mode defaults to %q, want off", got)
	}
}

func TestTradingRiskBrakes_JSONKeys(t *testing.T) {
	var c TradingRiskConfig
	raw := `{"max_orders_per_10min": 6, "max_symbol_share_pct": 25.5, "symbol_allowlist": ["BTC/USDT", "ETH/USDT"]}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if c.MaxOrdersPer10Min != 6 || c.MaxSymbolSharePct != 25.5 || len(c.SymbolAllowlist) != 2 {
		t.Fatalf("decoded %+v", c)
	}
}

func TestValidateTradingRisk(t *testing.T) {
	for _, tt := range []struct {
		name string
		c    TradingRiskConfig
		want string // substring of the error, "" for valid
	}{
		{"valid", TradingRiskConfig{MaxOrdersPer10Min: 6, SymbolAllowlist: []string{"BTC/USDT"}}, ""},
		{"share cap while no portfolio source is wired", TradingRiskConfig{MaxSymbolSharePct: 25}, "not supported yet"},
		{"share cap of 100 is still a cap", TradingRiskConfig{MaxSymbolSharePct: 100}, "max_symbol_share_pct"},
		{"negative rate", TradingRiskConfig{MaxOrdersPer10Min: -1}, "max_orders_per_10min"},
		{"negative share", TradingRiskConfig{MaxSymbolSharePct: -5}, "max_symbol_share_pct"},
		{"share over 100", TradingRiskConfig{MaxSymbolSharePct: 101}, "max_symbol_share_pct"},
		{"NaN share", TradingRiskConfig{MaxSymbolSharePct: math.NaN()}, "max_symbol_share_pct"},
		{"empty symbol", TradingRiskConfig{SymbolAllowlist: []string{"BTC/USDT", " "}}, "symbol_allowlist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTradingRisk(tt.c)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}

// live is accepted by validation and, with proactive on and a valid owner, reported
// as configured; without them it runs as shadow and says so.
func TestPolicyMode_LiveIsAcceptedAndReported(t *testing.T) {
	c := DefaultConfig().Proactive
	c.Policy.Mode = ProactiveModeLive
	if err := ValidateProactive(c); err != nil {
		t.Fatalf("live was rejected: %v", err)
	}
	if e := c.Effective(); e.PolicyMode != ProactiveModeShadow || !contains(e.Downgrades, "proactive: proactive disabled; policy.mode live → shadow") {
		t.Fatalf("live without proactive: %q %q", e.PolicyMode, e.Downgrades)
	}
	c.Enabled = true
	if e := c.Effective(); e.PolicyMode != ProactiveModeShadow || !contains(e.Downgrades, "proactive: owner not configured; policy.mode live → shadow") {
		t.Fatalf("live without an owner: %q %q", e.PolicyMode, e.Downgrades)
	}
	c.Owner = validOwner()
	if e := c.Effective(); e.PolicyMode != ProactiveModeLive || contains(e.Downgrades, "policy.mode live → shadow") {
		t.Fatalf("live with proactive on and an owner: %q %q", e.PolicyMode, e.Downgrades)
	}
	c.Policy.Mode = ProactiveModeShadow
	c.Owner = OwnerConfig{}
	if e := c.Effective(); e.PolicyMode != ProactiveModeShadow {
		t.Fatalf("shadow stays shadow: %q", e.PolicyMode)
	}
}
