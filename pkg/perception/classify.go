package perception

import "strings"

// AssetClass is the allocation class of a holding.
type AssetClass string

// The classes. ClassUnknown is a class of its own: a holding the rules do not
// recognise is never counted as crypto (or anything else) by default.
const (
	ClassCrypto   AssetClass = "crypto"
	ClassTHEquity AssetClass = "th_equity"
	ClassUSEquity AssetClass = "us_equity"
	ClassCash     AssetClass = "cash"
	ClassUnknown  AssetClass = "unknown"
)

// cashAssets are the currencies and USD stablecoins that count as cash wherever
// they are held.
var cashAssets = map[string]bool{
	// USD and the USD stablecoins.
	"USD": true, "USDT": true, "USDC": true, "BUSD": true, "FDUSD": true,
	"TUSD": true, "DAI": true, "USDP": true, "GUSD": true,
	// Fiat the supported venues hold.
	"THB": true, "EUR": true, "GBP": true, "JPY": true, "SGD": true, "HKD": true,
	"AUD": true, "CAD": true, "CNY": true, "KRW": true,
}

// ClassifyPosition maps a snapshot position to its allocation class from the
// venue it is held at (Position.Source), its wallet type or category
// (Position.Category) and its asset symbol.
//
// Cash is decided first, by category "cash" or by the asset (a fiat currency or a
// USD stablecoin, at any venue). Otherwise the venue decides: Bitkub, Binance, Binance
// TH and OKX hold crypto, Settrade holds Thai equities, Webull US equities. Any
// other venue is ClassUnknown. It is a pure function.
func ClassifyPosition(source, category, asset string) AssetClass {
	if strings.EqualFold(strings.TrimSpace(category), "cash") {
		return ClassCash
	}
	if cashAssets[strings.ToUpper(strings.TrimSpace(asset))] {
		return ClassCash
	}
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "bitkub", "binance", "binanceth", "okx":
		return ClassCrypto
	case "settrade":
		return ClassTHEquity
	case "webull":
		return ClassUSEquity
	}
	return ClassUnknown
}
