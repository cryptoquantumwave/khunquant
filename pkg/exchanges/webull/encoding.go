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

// optionContractMultiplier is the number of shares controlled by one US
// option contract.
const optionContractMultiplier = 100

// isOCCOptionSymbol reports whether sym looks like an OCC-encoded option
// symbol (underlying + YYMMDD + C|P + 8-digit strike), e.g. AAPL260821C00320000.
// Parsed from the end because the underlying has variable length.
func isOCCOptionSymbol(sym string) bool {
	// Minimum: 1-char underlying + 6-digit date + C|P + 8-digit strike = 16.
	if len(sym) < 16 {
		return false
	}
	typeChar := sym[len(sym)-9]
	if typeChar != 'C' && typeChar != 'P' {
		return false
	}
	for _, c := range sym[len(sym)-8:] { // strike
		if c < '0' || c > '9' {
			return false
		}
	}
	for _, c := range sym[len(sym)-15 : len(sym)-9] { // expiry YYMMDD
		if c < '0' || c > '9' {
			return false
		}
	}
	underlying := sym[:len(sym)-15]
	return underlying[0] >= 'A' && underlying[0] <= 'Z'
}
