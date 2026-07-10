package webull

import (
	"fmt"
	"math"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/providers/broker"
)

// OCCSymbol builds the OCC-encoded options symbol for a contract.
// Format: underlying + YYMMDD + (C|P) + strike*1000 as 8-digit zero-padded.
// Example: AAPL, 2026-08-21, CALL, 320.00 -> AAPL260821C00320000
func OCCSymbol(c broker.OptionContract) string {
	// Parse expiry (yyyy-MM-dd)
	expiry, err := time.Parse("2006-01-02", c.Expiry)
	if err != nil {
		// Fallback to empty string if parse fails
		return ""
	}

	// Format: YYMMDD
	expiryStr := expiry.Format("060102")

	// Option type: C for CALL, P for PUT
	optionTypeChar := "C"
	if c.OptionType == "PUT" {
		optionTypeChar = "P"
	}

	// Strike: multiply by 1000 and format as 8-digit zero-padded
	strikeInt := int(math.Round(c.Strike * 1000))
	strikeStr := fmt.Sprintf("%08d", strikeInt)

	return fmt.Sprintf("%s%s%s%s", c.Underlying, expiryStr, optionTypeChar, strikeStr)
}
