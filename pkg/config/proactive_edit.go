package config

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Edit-time checks for the proactive sub-system settings the launcher's
// Proactive page writes (perception, approvals, cognition, news).
//
// ValidateProactive, which the loaders run, only refuses a negative number: a value
// of 0, or one below a floor, is accepted there because the runtime substitutes a
// default or clamps it (news.go raises the interval to NewsMinIntervalMinutes,
// approval.Service treats 0 expiry as the default, market.go treats 0 as 60 s). A
// person who types such a value into a form should be told it is not valid, not be
// handed a silent substitute, so the web route calls these before saving. They are
// stricter than the loaders on purpose and are NOT run by Load or Effective: a
// config written by hand with an old value keeps loading exactly as before.
//
// The upper bounds below exist for the form only: they keep a typo from
// overflowing a time.Duration (seconds and minutes are multiplied into one).

// Upper bounds for the editable limits.
const (
	MaxEditableSeconds = 24 * 60 * 60 // a day
	MaxEditableMinutes = 7 * 24 * 60  // a week
	MaxEditableCount   = 1_000_000
	MaxEditableTokens  = 1_000_000_000

	// MaxProactiveModelLen bounds cognition.proactive_model, a model name.
	MaxProactiveModelLen = 200
)

func checkRange(field string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s must be between %d and %d (got %d)", field, lo, hi, v)
	}
	return nil
}

// ValidatePerceptionEdit judges proactive.perception for saving from the form.
func ValidatePerceptionEdit(p PerceptionConfig) error {
	return errors.Join(
		checkRange("proactive.perception.market_poll_seconds", p.MarketPollSeconds, 1, MaxEditableSeconds),
		checkRange("proactive.perception.reads_per_minute_limit", p.ReadsPerMinuteLimit, 1, MaxEditableCount),
		checkRange("proactive.perception.snapshot_minutes", p.SnapshotMinutes, 1, MaxEditableMinutes),
	)
}

// ValidateApprovalsEdit judges proactive.approvals for saving from the form.
func ValidateApprovalsEdit(a ApprovalsConfig) error {
	return checkRange("proactive.approvals.expiry_minutes", a.ExpiryMinutes, 1, MaxEditableMinutes)
}

// ValidateCognitionEdit judges proactive.cognition for saving from the form. A
// wake or token limit of 0 is allowed: it means "no budgeted work", not unlimited.
// proactive_model is a model name (empty = the routing light model), never a key.
func ValidateCognitionEdit(c CognitionConfig) error {
	errs := []error{
		checkRange("proactive.cognition.max_concurrent_llm", c.MaxConcurrentLLM, 1, 64),
		checkRange("proactive.cognition.wakes_per_hour", c.WakesPerHour, 0, MaxEditableCount),
		checkRange("proactive.cognition.tokens_per_day", c.TokensPerDay, 0, MaxEditableTokens),
	}
	m := c.ProactiveModel
	switch {
	case len(m) > MaxProactiveModelLen:
		errs = append(errs, fmt.Errorf("proactive.cognition.proactive_model must be at most %d characters", MaxProactiveModelLen))
	case strings.IndexFunc(m, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		errs = append(errs, errors.New("proactive.cognition.proactive_model must be a model name without spaces or control characters"))
	}
	return errors.Join(errs...)
}

// ValidateNewsEdit judges proactive.news for saving from the form.
func ValidateNewsEdit(n NewsConfig) error {
	return errors.Join(
		checkRange("proactive.news.interval_minutes", n.IntervalMinutes, NewsMinIntervalMinutes, MaxEditableMinutes),
		checkRange("proactive.news.max_assets", n.MaxAssets, 1, NewsMaxAssetsCap),
		checkRange("proactive.news.max_parses_per_run", n.MaxParsesPerRun, 0, MaxEditableCount),
		checkRange("proactive.news.max_parses_per_day", n.MaxParsesPerDay, 0, MaxEditableCount),
		checkRange("proactive.news.run_seconds", n.RunSeconds, 1, MaxEditableSeconds),
	)
}

// WebSearchAvailable reports whether the config gives the agent a working
// web_search and web_fetch: both tools enabled and one search provider usable.
// News reads the web through them and does not start without them. It mirrors the
// provider choice in pkg/agent/loop.go (tools.NewWebSearchTool); it looks at
// presence only and never returns a key.
func (c *Config) WebSearchAvailable() bool {
	t := &c.Tools
	if !t.IsToolEnabled("web") || !t.IsToolEnabled("web_fetch") {
		return false
	}
	w := &t.Web
	has := func(key string, keys []string) bool { return len(MergeAPIKeys(key, keys)) > 0 }
	switch {
	case w.Perplexity.Enabled && has(w.Perplexity.APIKey, w.Perplexity.APIKeys):
	case w.Brave.Enabled && has(w.Brave.APIKey, w.Brave.APIKeys):
	case w.SearXNG.Enabled && w.SearXNG.BaseURL != "":
	case w.Tavily.Enabled && has(w.Tavily.APIKey, w.Tavily.APIKeys):
	case w.DuckDuckGo.Enabled:
	case w.GLMSearch.Enabled && w.GLMSearch.APIKey != "":
	default:
		return false
	}
	return true
}

// DefaultProactiveConfig is the shipped default of the proactive section: what a
// fresh install has. The web route uses it as the neutral base its edit checks
// overlay the submitted fields on.
func DefaultProactiveConfig() ProactiveConfig { return defaultProactiveConfig() }
