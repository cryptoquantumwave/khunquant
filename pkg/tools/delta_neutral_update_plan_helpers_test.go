package tools

import (
	"testing"
	"time"
)

func TestIsValidCooldownDuration(t *testing.T) {
	valid := []string{"1h", "4h", "8h", "1d", "3d"}
	for _, s := range valid {
		if !isValidCooldownDuration(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	invalid := []string{"", "2h", "1w", "5d", "bogus"}
	for _, s := range invalid {
		if isValidCooldownDuration(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestParseSilenceDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"4h", 4 * time.Hour},
		{"8h", 8 * time.Hour},
		{"1d", 24 * time.Hour},
		{"3d", 3 * 24 * time.Hour},
		{"", time.Hour},      // default
		{"bogus", time.Hour}, // default
	}
	for _, tc := range cases {
		if got := parseSilenceDuration(tc.in); got != tc.want {
			t.Errorf("parseSilenceDuration(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestToStringSlice(t *testing.T) {
	raw := []any{"a", "", "b", 42, true, "c"}
	got := toStringSlice(raw)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("toStringSlice length = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("toStringSlice[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestToStringSlice_Empty(t *testing.T) {
	got := toStringSlice(nil)
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %v", got)
	}
}
