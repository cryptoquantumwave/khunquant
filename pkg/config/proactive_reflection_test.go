package config

import "testing"

// The nightly reflection is a budgeted cognition job: asked for without cognition it is
// switched off, and the downgrade says so.
func TestEffective_ReflectionNeedsCognition(t *testing.T) {
	c := defaultProactiveConfig()
	if c.Reflection.Enabled || c.Reflection.AtLocal != "21:00" {
		t.Fatalf("defaults = %+v: the reflection is off by default, at 21:00", c.Reflection)
	}
	c.Enabled = true
	c.Reflection.Enabled = true
	e := c.Effective()
	if e.Reflection || e.Cognition {
		t.Errorf("reflection without cognition: %+v", e)
	}
	want := "proactive: cognition is off; reflection.enabled true → false"
	if !contains(e.Downgrades, want) {
		t.Errorf("downgrades %q lack %q", e.Downgrades, want)
	}
	c.Cognition.Enabled = true
	if e := c.Effective(); !e.Reflection || contains(e.Downgrades, want) {
		t.Errorf("reflection with cognition: %+v", e)
	}
	c.Reflection.Enabled = false
	if e := c.Effective(); e.Reflection || contains(e.Downgrades, want) {
		t.Errorf("reflection off: %+v", e)
	}
}
