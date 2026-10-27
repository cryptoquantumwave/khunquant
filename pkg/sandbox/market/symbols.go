package market

import (
	"fmt"
	"strings"
)

// Pair is a canonical "BTC/USDT". A spot market and its USDT-margined swap share one model.
type Pair struct{ Base, Quote string }

func (p Pair) String() string { return p.Base + "/" + p.Quote }

// ParsePair reads "BTC/USDT" (a ccxt swap symbol "BTC/USDT:USDT" is the same pair).
func ParsePair(s string) (Pair, error) {
	s, _, _ = strings.Cut(strings.ToUpper(strings.TrimSpace(s)), ":")
	base, quote, ok := strings.Cut(s, "/")
	if !ok || base == "" || quote == "" || strings.ContainsAny(s, " -_") {
		return Pair{}, fmt.Errorf("symbol %q is not BASE/QUOTE (for example BTC/USDT)", s)
	}
	return Pair{base, quote}, nil
}

// Venues the book models. "*" is the wildcard a venue falls back to.
const Wildcard = "*"

func knownVenue(v string) bool { return v == Wildcard || v == "binance" || v == "okx" }

// binanceQuotes are the quote assets ParseWire recognises (the longest suffix wins).
var binanceQuotes = []string{"FDUSD", "USDT", "USDC", "BUSD", "BTC", "ETH", "BNB", "TRY", "EUR"}

// Wire is the venue's symbol for p: Binance BTCUSDT (spot and futures alike), OKX
// BTC-USDT or, for the swap, BTC-USDT-SWAP.
func Wire(venue string, p Pair, swap bool) string {
	switch venue {
	case "binance":
		return p.Base + p.Quote
	case "okx":
		if swap {
			return p.Base + "-" + p.Quote + "-SWAP"
		}
		return p.Base + "-" + p.Quote
	}
	return ""
}

// ParseWire is the inverse of Wire. ok is false for a symbol it cannot read.
func ParseWire(venue, wire string) (p Pair, swap, ok bool) {
	switch venue {
	case "binance":
		for _, q := range binanceQuotes {
			if base, found := strings.CutSuffix(wire, q); found && base != "" && wire == strings.ToUpper(wire) {
				return Pair{base, q}, false, true
			}
		}
	case "okx":
		w, isSwap := strings.CutSuffix(wire, "-SWAP")
		if parts := strings.Split(w, "-"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return Pair{parts[0], parts[1]}, isSwap, true
		}
	}
	return Pair{}, false, false
}
