package broker

import "strings"

// providerCategories is the static asset category for each known provider ID.
//
// It is the single source of truth for provider category in contexts that must
// NOT instantiate a live provider — notably config-mutation tools (e.g. DCA plan
// updates), where resolving a real client could fail for reasons unrelated to the
// edit (missing/invalid credentials, network). Live code paths that already hold a
// Provider should still prefer its Category() method; this table must agree with
// those methods. Keep it in sync when adding a broker.
var providerCategories = map[string]AssetCategory{
	"binance":   CategoryCrypto,
	"binanceth": CategoryCrypto,
	"bitkub":    CategoryCrypto,
	"okx":       CategoryCrypto,
	"settrade":  CategoryStock,
	"webull":    CategoryStock,
}

// ProviderCategory returns the static asset category for a provider ID and whether
// the ID is known. Lookup is case-insensitive.
func ProviderCategory(providerID string) (AssetCategory, bool) {
	c, ok := providerCategories[strings.ToLower(providerID)]
	return c, ok
}

// IsStockProvider reports whether the given provider ID is a stock (equity) broker
// per the static category table. Unknown providers are treated as non-stock.
func IsStockProvider(providerID string) bool {
	c, ok := ProviderCategory(providerID)
	return ok && c == CategoryStock
}
