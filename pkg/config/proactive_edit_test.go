package config

import (
	"strings"
	"testing"
)

func TestProactiveEditValidators(t *testing.T) {
	d := defaultProactiveConfig()

	// The shipped defaults are what a fresh form would save: all valid.
	for name, err := range map[string]error{
		"perception": ValidatePerceptionEdit(d.Perception),
		"approvals":  ValidateApprovalsEdit(d.Approvals),
		"cognition":  ValidateCognitionEdit(d.Cognition),
		"news":       ValidateNewsEdit(d.News),
	} {
		if err != nil {
			t.Errorf("%s defaults refused: %v", name, err)
		}
	}

	bad := func(name string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, want)
		}
	}
	p := d.Perception
	p.MarketPollSeconds = 0
	bad("poll 0", ValidatePerceptionEdit(p), "market_poll_seconds")
	p = d.Perception
	p.ReadsPerMinuteLimit = MaxEditableCount + 1
	bad("reads cap", ValidatePerceptionEdit(p), "reads_per_minute_limit")

	n := d.News
	n.IntervalMinutes = NewsMinIntervalMinutes - 1
	bad("news floor", ValidateNewsEdit(n), "interval_minutes")
	n = d.News
	n.IntervalMinutes = NewsMinIntervalMinutes
	n.MaxAssets = NewsMaxAssetsCap
	if err := ValidateNewsEdit(n); err != nil {
		t.Errorf("floor and cap are valid: %v", err)
	}
	n.MaxAssets = NewsMaxAssetsCap + 1
	bad("news cap", ValidateNewsEdit(n), "max_assets")

	c := d.Cognition
	c.WakesPerHour, c.TokensPerDay = 0, 0
	if err := ValidateCognitionEdit(c); err != nil {
		t.Errorf("0 means no budgeted work and is valid: %v", err)
	}
	c.ProactiveModel = "a b"
	bad("model space", ValidateCognitionEdit(c), "proactive_model")
	c.ProactiveModel = "model\x00name"
	bad("model control", ValidateCognitionEdit(c), "proactive_model")
}

func TestWebSearchAvailable(t *testing.T) {
	cfg := DefaultConfig()
	w := &cfg.Tools.Web
	w.Brave.Enabled, w.Tavily.Enabled, w.DuckDuckGo.Enabled = false, false, false
	w.Perplexity.Enabled, w.SearXNG.Enabled, w.GLMSearch.Enabled = false, false, false
	cfg.Tools.Web.Enabled, cfg.Tools.WebFetch.Enabled = true, true
	if cfg.WebSearchAvailable() {
		t.Error("no provider enabled, yet available")
	}
	w.Brave.Enabled = true
	if cfg.WebSearchAvailable() {
		t.Error("brave without a key counts as available")
	}
	w.Brave.APIKey = "k"
	if !cfg.WebSearchAvailable() {
		t.Error("brave with a key is not available")
	}
	cfg.Tools.WebFetch.Enabled = false
	if cfg.WebSearchAvailable() {
		t.Error("available with web_fetch off")
	}
	cfg.Tools.WebFetch.Enabled = true
	cfg.Tools.Web.Enabled = false
	if cfg.WebSearchAvailable() {
		t.Error("available with web off")
	}
}
