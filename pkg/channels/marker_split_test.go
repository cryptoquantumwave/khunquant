package channels

import (
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
)

func TestSplitByMarker(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"no marker", "one message", []string{"one message"}},
		{"two parts", "first" + MessageSplitMarker + "second", []string{"first", "second"}},
		{"trims surrounding space", "  first  " + MessageSplitMarker + "  second  ", []string{"first", "second"}},
		{"drops empty parts", "first" + MessageSplitMarker + "   " + MessageSplitMarker + "second", []string{"first", "second"}},
		{"empty input", "", nil},
		// A message that is nothing but markers must not vanish: sending
		// nothing at all is worse than sending the original.
		{"only markers", MessageSplitMarker + MessageSplitMarker, []string{MessageSplitMarker + MessageSplitMarker}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitByMarker(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("SplitByMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("part %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// splitOutbound is where the two splits meet. The order matters: the marker is
// the model saying "separate messages", the length limit is a transport
// constraint, and applying length first could cut across a marker.
func TestSplitOutbound(t *testing.T) {
	enabled := &Manager{config: &config.Config{}}
	enabled.config.Agents.Defaults.SplitOnMarker = true
	disabled := &Manager{config: &config.Config{}}

	t.Run("disabled ignores the marker", func(t *testing.T) {
		got := disabled.splitOutbound("a"+MessageSplitMarker+"b", 0)
		if len(got) != 1 {
			t.Fatalf("got %d parts, want 1 when the feature is off: %q", len(got), got)
		}
		if !strings.Contains(got[0], MessageSplitMarker) {
			t.Error("the marker was consumed even though splitting is disabled")
		}
	})

	t.Run("enabled splits on the marker", func(t *testing.T) {
		got := enabled.splitOutbound("a"+MessageSplitMarker+"b", 0)
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("got %q, want [a b]", got)
		}
	})

	t.Run("length splitting still applies within a part", func(t *testing.T) {
		long := strings.Repeat("x", 25)
		got := enabled.splitOutbound("short"+MessageSplitMarker+long, 10)
		if len(got) < 3 {
			t.Fatalf("got %d parts, want the long part chunked as well: %q", len(got), got)
		}
		if got[0] != "short" {
			t.Errorf("first part = %q, want the short one intact", got[0])
		}
		for i, p := range got {
			if len([]rune(p)) > 10 {
				t.Errorf("part %d is %d runes, over the limit: %q", i, len([]rune(p)), p)
			}
		}
	})

	t.Run("no marker still length splits", func(t *testing.T) {
		got := enabled.splitOutbound(strings.Repeat("y", 25), 10)
		if len(got) < 3 {
			t.Errorf("got %d parts, want the message chunked: %q", len(got), got)
		}
	})

	t.Run("always returns at least one part", func(t *testing.T) {
		if got := enabled.splitOutbound("", 0); len(got) != 1 {
			t.Errorf("empty content produced %d parts, want 1", len(got))
		}
	})

	t.Run("nil config does not panic", func(t *testing.T) {
		bare := &Manager{}
		if got := bare.splitOutbound("a"+MessageSplitMarker+"b", 0); len(got) != 1 {
			t.Errorf("got %d parts, want 1 with no config", len(got))
		}
	})
}
