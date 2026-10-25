package perception

import "testing"

func TestClassifyPosition(t *testing.T) {
	tests := []struct {
		name                    string
		source, category, asset string
		want                    AssetClass
	}{
		{"binance spot BTC", "binance", "spot", "BTC", ClassCrypto},
		{"binance futures wallet ETH", "binance", "futures_usdt", "ETH", ClassCrypto},
		{"binance earn token", "binance", "earn", "LDBNB", ClassCrypto},
		{"binance th", "binanceth", "spot", "BTC", ClassCrypto},
		{"bitkub spot", "bitkub", "spot", "BTC", ClassCrypto},
		{"okx funding", "okx", "funding", "SOL", ClassCrypto},
		{"upper-case source", "OKX", "trading", "SOL", ClassCrypto},
		{"usdt stablecoin on binance", "binance", "spot", "USDT", ClassCash},
		{"usdc on okx", "okx", "trading", "usdc", ClassCash},
		{"fdusd", "binance", "spot", "FDUSD", ClassCash},
		{"thb on bitkub", "bitkub", "spot", "THB", ClassCash},
		{"settrade cash wallet", "settrade", "cash", "THB", ClassCash},
		{"settrade stock", "settrade", "stock", "PTT", ClassTHEquity},
		{"webull stock", "webull", "stock", "AAPL", ClassUSEquity},
		{"webull cash wallet", "webull", "cash", "USD", ClassCash},
		{"webull option", "webull", "option", "AAPL260821C00320000", ClassUSEquity},
		{"unknown venue", "mt5", "positions", "EURUSD", ClassUnknown},
		{"empty source and category", "", "", "XYZ", ClassUnknown},
		{"unknown venue is not crypto", "somebroker", "spot", "BTC", ClassUnknown},
		{"whitespace", " settrade ", " stock ", " PTT ", ClassTHEquity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPosition(tc.source, tc.category, tc.asset); got != tc.want {
				t.Fatalf("ClassifyPosition(%q, %q, %q) = %q, want %q", tc.source, tc.category, tc.asset, got, tc.want)
			}
		})
	}
}
