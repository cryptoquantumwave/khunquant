package perceptiontest

import (
	"strings"
	"testing"
	"time"
)

func TestBoundary_CheckerCatchesBreaches(t *testing.T) {
	b := Boundary{
		Root:      "pkg/x",
		Allowed:   []string{"pkg/events", "pkg/proactive"},
		Forbidden: []string{"pkg/agent", "pkg/providers/broker"},
	}
	cases := []struct {
		name  string
		g     ImportGraph
		wants string // substring of a violation; "" = none expected
	}{
		{"clean", ImportGraph{"pkg/x": {"pkg/events"}, "pkg/events": {"pkg/proactive"}}, ""},
		{"own subpackage", ImportGraph{"pkg/x": {"pkg/x/sub"}}, ""},
		{"direct forbidden", ImportGraph{"pkg/x": {"pkg/agent"}}, "pkg/agent directly"},
		{"forbidden subpackage", ImportGraph{"pkg/x": {"pkg/providers/broker/okx"}}, "pkg/providers/broker/okx directly"},
		{"unlisted", ImportGraph{"pkg/x": {"pkg/skills"}}, "not on its allowed list"},
		{"transitive", ImportGraph{"pkg/x": {"pkg/events"}, "pkg/events": {"pkg/h"}, "pkg/h": {"pkg/agent"}}, "reaches pkg/agent through pkg/events"},
		{"cycle terminates", ImportGraph{"pkg/x": {"pkg/events"}, "pkg/events": {"pkg/proactive"}, "pkg/proactive": {"pkg/events"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := b.Violations(c.g)
			switch {
			case c.wants == "" && len(v) != 0:
				t.Fatalf("unexpected violations: %v", v)
			case c.wants != "" && !strings.Contains(strings.Join(v, "\n"), c.wants):
				t.Fatalf("violations %v do not mention %q", v, c.wants)
			}
		})
	}
}

func TestClock_FiresOnAdvance(t *testing.T) {
	c := NewClock(time.Unix(0, 0))
	ch := c.After(time.Second)
	select {
	case <-ch:
		t.Fatal("fired before the clock moved")
	default:
	}
	if !c.WaitForTimers(1, time.Second) {
		t.Fatal("timer not pending")
	}
	c.Advance(999 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("fired early")
	default:
	}
	c.Advance(time.Millisecond)
	select {
	case <-ch:
	default:
		t.Fatal("did not fire")
	}
	if got := c.Now(); !got.Equal(time.Unix(1, 0)) {
		t.Fatalf("now = %v", got)
	}
}
