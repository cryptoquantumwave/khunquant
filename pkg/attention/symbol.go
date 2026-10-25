package attention

// AssetOf is the upper-case base asset of a Subject that is a market symbol by the
// same rule attention uses to tie an alert to an asset ("BTC/USDT", "BTC/USDT:USDT",
// "BTC-USDT" and "BTC-USDT-SWAP" give "BTC"), or "" when the Subject is not one
// ("plan:12", "portfolio", a bare "BTC"). Other packages use it to tell a market
// subject from any other without keeping a second list of quote currencies.
func AssetOf(subject string) string { return assetFromSymbol(subject) }
