package skills

import (
	"strings"
	"testing"
)

func TestValidateSkillName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "market-analysis", false},
		{"valid alphanumeric", "dca2", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"absolute path", "/etc/passwd", true},
		{"too long", strings.Repeat("a", MaxNameLength+1), true},
		{"underscore not allowed", "delta_neutral", true},
		{"leading hyphen", "-bad", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSkillName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSkillName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
