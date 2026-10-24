package config

import (
	"strings"
	"testing"
)

// News is off by default, and it needs cognition: reading an article is a budgeted
// cognition job, so with cognition off the switch is lowered and the reason recorded,
// never a silent no-op.
func TestProactiveNews_NeedsCognition(t *testing.T) {
	const line = "news.enabled true → false"
	on := func(mut func(*ProactiveConfig)) EffectiveModes {
		c := defaultProactiveConfig()
		c.Enabled = true
		c.News.Enabled = true
		mut(&c)
		return c.Effective()
	}

	if e := defaultProactiveConfig().Effective(); e.News || len(e.Downgrades) != 0 {
		t.Errorf("default: %+v", e)
	}
	if e := on(func(c *ProactiveConfig) { c.Cognition.Enabled = true }); !e.News || len(e.Downgrades) != 0 {
		t.Errorf("news with cognition: %+v", e)
	}
	for name, mut := range map[string]func(*ProactiveConfig){
		"cognition off, no owner": func(*ProactiveConfig) {},
		"cognition off, owner": func(c *ProactiveConfig) {
			c.Owner = OwnerConfig{Channel: "telegram", ChatID: "1001", SenderID: "telegram:1001"}
		},
	} {
		e := on(mut)
		if e.News {
			t.Errorf("%s: news is on without cognition", name)
		}
		if !strings.Contains(strings.Join(e.Downgrades, "\n"), line) {
			t.Errorf("%s: no downgrade line for news: %q", name, e.Downgrades)
		}
	}
	// A switch that was never asked for says nothing.
	c := defaultProactiveConfig()
	c.Enabled = true
	if e := c.Effective(); e.News || strings.Contains(strings.Join(e.Downgrades, "\n"), "news") {
		t.Errorf("news not requested: %+v", e)
	}
}

func TestProactiveNews_LimitsAreValidated(t *testing.T) {
	d := defaultProactiveConfig().News
	if d.Enabled || d.IntervalMinutes != 60 || d.MaxAssets != 8 || d.MaxParsesPerRun < 1 || d.MaxParsesPerDay < d.MaxParsesPerRun || d.RunSeconds != 300 {
		t.Errorf("defaults = %+v", d)
	}
	for field, mut := range map[string]func(*ProactiveConfig){
		"proactive.news.interval_minutes":   func(c *ProactiveConfig) { c.News.IntervalMinutes = -1 },
		"proactive.news.max_assets":         func(c *ProactiveConfig) { c.News.MaxAssets = -1 },
		"proactive.news.max_parses_per_run": func(c *ProactiveConfig) { c.News.MaxParsesPerRun = -1 },
		"proactive.news.max_parses_per_day": func(c *ProactiveConfig) { c.News.MaxParsesPerDay = -1 },
		"proactive.news.run_seconds":        func(c *ProactiveConfig) { c.News.RunSeconds = -5 },
	} {
		c := defaultProactiveConfig()
		mut(&c)
		if err := ValidateProactive(c); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", field, err)
		}
	}
	if err := ValidateProactive(defaultProactiveConfig()); err != nil {
		t.Errorf("defaults: %v", err)
	}
}
