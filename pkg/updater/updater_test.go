package updater

import (
	"testing"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int // negative, zero, or positive
	}{
		// Equal versions
		{"1.2.0", "1.2.0", 0},
		{"v1.2.0", "1.2.0", 0},
		{"v1.2.0", "v1.2.0", 0},

		// Stable ordering
		{"1.2.0", "1.3.0", -1},
		{"1.3.0", "1.2.0", 1},
		{"1.2.0", "2.0.0", -1},
		{"1.10.0", "1.9.0", 1},

		// Pre-release ordering (critical fix: rc < stable)
		{"1.2.0-rc1", "1.2.0", -1},
		{"1.2.0-alpha", "1.2.0", -1},
		{"1.2.0-rc1", "1.2.0-rc2", -1},

		// Patch versions
		{"1.2.3", "1.2.4", -1},
		{"1.2.4", "1.2.3", 1},
	}

	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		// Normalise to sign only: -1, 0, +1
		gotSign := 0
		if got < 0 {
			gotSign = -1
		} else if got > 0 {
			gotSign = 1
		}
		if gotSign != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d (sign %d), want sign %d", tt.a, tt.b, got, gotSign, tt.want)
		}
	}
}
