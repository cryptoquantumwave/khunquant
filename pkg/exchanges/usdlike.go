package exchanges

import "strings"

// usdLikeSet is the set of currencies treated as 1:1 with USD for valuation
// purposes: USD itself and the major USD stablecoins.
var usdLikeSet = map[string]bool{
	"USD": true, "USDT": true, "USDC": true, "BUSD": true,
	"FDUSD": true, "TUSD": true, "DAI": true, "USDP": true, "GUSD": true,
}

// USDLike reports whether sym is a currency valued 1:1 with USD.
// Used to convert between USD-quoted brokers (e.g. Webull) and
// stablecoin-quoted crypto exchanges without a market rate lookup.
func USDLike(sym string) bool {
	return usdLikeSet[strings.ToUpper(sym)]
}
