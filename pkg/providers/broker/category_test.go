package broker

import "testing"

func TestProviderCategory(t *testing.T) {
	cases := []struct {
		id       string
		want     AssetCategory
		wantOK   bool
		wantStck bool
	}{
		{"webull", CategoryStock, true, true},
		{"settrade", CategoryStock, true, true},
		{"WEBULL", CategoryStock, true, true}, // case-insensitive
		{"binance", CategoryCrypto, true, false},
		{"okx", CategoryCrypto, true, false},
		{"bitkub", CategoryCrypto, true, false},
		{"unknown", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, ok := ProviderCategory(tc.id)
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Errorf("ProviderCategory(%q) = (%q, %v), want (%q, %v)", tc.id, got, ok, tc.want, tc.wantOK)
			}
			if IsStockProvider(tc.id) != tc.wantStck {
				t.Errorf("IsStockProvider(%q) = %v, want %v", tc.id, IsStockProvider(tc.id), tc.wantStck)
			}
		})
	}
}
