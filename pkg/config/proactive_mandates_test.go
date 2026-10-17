package config

import "testing"

// Mandates are off by default, the price-move bound is unset (the code default of 5
// applies), and both are settable from the environment.
func TestProactive_MandatesDefaultsAndEnv(t *testing.T) {
	cfg, err := LoadConfig(writeRawConfig(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Proactive.Mandates; m.Enabled || m.PriceMovePct != 0 {
		t.Fatalf("default mandates = %+v", m)
	}
	t.Setenv("KHUNQUANT_PROACTIVE_MANDATES_ENABLED", "true")
	t.Setenv("KHUNQUANT_PROACTIVE_MANDATES_PRICE_MOVE_PCT", "3.5")
	cfg, err = LoadConfig(writeRawConfig(t, `{"proactive": {"mandates": {"price_move_pct": 8}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Proactive.Mandates; !m.Enabled || m.PriceMovePct != 3.5 {
		t.Fatalf("env mandates = %+v", m)
	}
}
