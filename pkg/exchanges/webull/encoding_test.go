package webull

import (
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

func TestOCCSymbol(t *testing.T) {
	tests := []struct {
		name     string
		contract broker.OptionContract
		expected string
	}{
		{
			name: "AAPL CALL 320.00 2026-08-21",
			contract: broker.OptionContract{
				Underlying: "AAPL",
				Expiry:     "2026-08-21",
				Strike:     320.00,
				OptionType: "CALL",
			},
			expected: "AAPL260821C00320000",
		},
		{
			name: "AAPL PUT 320.00 2026-08-21",
			contract: broker.OptionContract{
				Underlying: "AAPL",
				Expiry:     "2026-08-21",
				Strike:     320.00,
				OptionType: "PUT",
			},
			expected: "AAPL260821P00320000",
		},
		{
			name: "SPY CALL 450.50 (fractional)",
			contract: broker.OptionContract{
				Underlying: "SPY",
				Expiry:     "2026-07-17",
				Strike:     450.50,
				OptionType: "CALL",
			},
			expected: "SPY260717C00450500",
		},
		{
			name: "QQQ PUT 12.50 (low strike)",
			contract: broker.OptionContract{
				Underlying: "QQQ",
				Expiry:     "2026-12-31",
				Strike:     12.50,
				OptionType: "PUT",
			},
			expected: "QQQ261231P00012500",
		},
		{
			name: "TSLA CALL 1234.56 (high strike)",
			contract: broker.OptionContract{
				Underlying: "TSLA",
				Expiry:     "2026-01-15",
				Strike:     1234.56,
				OptionType: "CALL",
			},
			expected: "TSLA260115C01234560",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OCCSymbol(tt.contract)
			if got != tt.expected {
				t.Errorf("OCCSymbol() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestIsOCCOptionSymbol(t *testing.T) {
	tests := []struct {
		sym  string
		want bool
	}{
		{"AAPL260821C00320000", true},
		{"SPY260717C00450500", true},
		{"QQQ261231P00012500", true},
		{"T260115C01234560", true},     // 1-char underlying
		{"AAPL", false},                // plain equity ticker
		{"BRK.B", false},               // dotted ticker, too short
		{"", false},                    // empty
		{"AAPL260821X00320000", false}, // invalid type char
		{"AAPL2608_1C00320000", false}, // non-digit in expiry
		{"AAPL260821C0032000X", false}, // non-digit in strike
		{"1APL260821C00320000", false}, // underlying must start with a letter
	}

	for _, tt := range tests {
		if got := isOCCOptionSymbol(tt.sym); got != tt.want {
			t.Errorf("isOCCOptionSymbol(%q) = %v, want %v", tt.sym, got, tt.want)
		}
	}
}
