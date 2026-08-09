package e2e

import (
	"fmt"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// assetHolding is one mocked position in the roster.
type assetHolding struct {
	Asset  string
	Free   float64
	Locked float64
}

// manyAssets is the roster used to mock a whale account, and the variable the
// fixture builders read. Tests that need a different size swap it out and
// restore it (see syntheticAssets).
var manyAssets = manyAssetsBase

// manyAssetsBase deliberately mixes magnitudes: ordinary holdings, sub-satoshi
// dust, and meme-coin bags in the trillions, because those are what a real
// "many assets" account looks like and they are what stresses the fixed-width
// balance table.
var manyAssetsBase = []assetHolding{
	{"BTC", 12.53419876, 0.5},
	{"ETH", 341.9982, 12.004},
	{"USDT", 1284933.44, 25000},
	{"USDC", 903412.19, 0},
	{"BNB", 1893.4471, 100.5},
	{"SOL", 24019.883, 500},
	{"XRP", 1904221.5, 0},
	{"ADA", 883412.75, 12000},
	{"DOGE", 19883412.25, 0},
	{"SHIB", 1482993771244.5, 12000000}, // trillions: overruns a %16s column
	{"PEPE", 884120993441.125, 0},       // trillions
	{"BONK", 44120993441772.0, 0},       // tens of trillions
	{"AVAX", 4412.98, 0},
	{"DOT", 88123.4, 1200},
	{"MATIC", 442199.75, 0},
	{"LINK", 12993.44, 0},
	{"UNI", 44120.5, 0},
	{"ATOM", 8812.25, 0},
	{"LTC", 992.4471, 12},
	{"BCH", 118.9932, 0},
	{"ETC", 4412.8, 0},
	{"FIL", 8821.44, 0},
	{"NEAR", 44120.9, 0},
	{"APT", 12993.1, 0},
	{"ARB", 88412.25, 0},
	{"OP", 44120.75, 0},
	{"SUI", 129934.5, 0},
	{"SEI", 441209.25, 0},
	{"TIA", 8821.9, 0},
	{"INJ", 4412.75, 0},
	{"RUNE", 88120.5, 0},
	{"AAVE", 441.98, 0},
	{"MKR", 12.4471, 0},
	{"CRV", 88412.25, 0},
	{"LDO", 44120.5, 0},
	{"SNX", 12993.75, 0},
	{"COMP", 441.25, 0},
	{"GRT", 884120.5, 0},
	{"SAND", 129934.25, 0},
	{"MANA", 441209.75, 0},
	{"AXS", 8821.5, 0},
	{"GALA", 4412099.25, 0},
	{"CHZ", 884120.75, 0},
	{"ENJ", 129934.5, 0},
	{"FLOW", 44120.25, 0},
	{"ALGO", 884120.5, 0},
	{"VET", 8841209.75, 0},
	{"ICP", 4412.5, 0},
	{"HBAR", 8841209.25, 0},
	{"XLM", 4412099.5, 0},
	{"TRX", 8841209.75, 0},
	{"EOS", 88412.25, 0},
	{"XTZ", 44120.5, 0},
	{"THETA", 129934.75, 0},
	{"EGLD", 4412.25, 0},
	{"FTM", 884120.5, 0},
	{"ONE", 8841209.25, 0},
	{"ZIL", 4412099.75, 0},
	{"DUST", 0.0000000012, 0},  // sub-satoshi: %.8f + trim renders "0"
	{"WEI", 0.000000004999, 0}, // rounds to 0.00000000 → "0"
	{"MICRO", 0.00000001, 0},   // exactly 1e-8: the smallest renderable
}

// binanceManyAssetFixtures builds the whole binance venue: every endpoint the
// get_assets_list "all" path touches, each holding the full roster.
func binanceManyAssetFixtures() []sandbox.FixtureEntry {
	// Spot: GET /api/v3/account
	spotBalances := make([]map[string]any, 0, len(manyAssets))
	for _, a := range manyAssets {
		spotBalances = append(spotBalances, map[string]any{
			"asset":  a.Asset,
			"free":   fmt.Sprintf("%.12f", a.Free),
			"locked": fmt.Sprintf("%.12f", a.Locked),
		})
	}

	// Funding: POST /sapi/v1/asset/get-funding-asset
	funding := make([]map[string]any, 0, len(manyAssets))
	for _, a := range manyAssets {
		funding = append(funding, map[string]any{
			"asset":        a.Asset,
			"free":         fmt.Sprintf("%.12f", a.Free/4),
			"locked":       "0",
			"freeze":       fmt.Sprintf("%.12f", a.Locked/4),
			"withdrawing":  "0",
			"btcValuation": "0.0001",
		})
	}

	// Cross margin: GET /sapi/v1/margin/account
	marginAssets := make([]map[string]any, 0, len(manyAssets))
	for _, a := range manyAssets {
		marginAssets = append(marginAssets, map[string]any{
			"asset":    a.Asset,
			"free":     fmt.Sprintf("%.12f", a.Free/10),
			"locked":   fmt.Sprintf("%.12f", a.Locked/10),
			"borrowed": "0",
			"interest": "0",
			"netAsset": fmt.Sprintf("%.12f", a.Free/10),
		})
	}

	// Coin-M futures: GET /dapi/v1/account
	coinmAssets := make([]map[string]any, 0, 8)
	for _, a := range manyAssets[:8] {
		coinmAssets = append(coinmAssets, map[string]any{
			"asset":              a.Asset,
			"walletBalance":      fmt.Sprintf("%.12f", a.Free/20),
			"availableBalance":   fmt.Sprintf("%.12f", a.Free/20),
			"marginBalance":      fmt.Sprintf("%.12f", a.Free/20),
			"unrealizedProfit":   "0",
			"maintMargin":        "0",
			"initialMargin":      "0",
			"crossWalletBalance": fmt.Sprintf("%.12f", a.Free/20),
		})
	}

	// USDT-M futures: GET /fapi/v3/account. This fixture is intentionally
	// present so the shadowing test can prove the simulator wins.
	usdmAssets := make([]map[string]any, 0, 8)
	for _, a := range manyAssets[:8] {
		usdmAssets = append(usdmAssets, map[string]any{
			"asset":              a.Asset,
			"walletBalance":      fmt.Sprintf("%.12f", a.Free/2),
			"availableBalance":   fmt.Sprintf("%.12f", a.Free/2),
			"marginBalance":      fmt.Sprintf("%.12f", a.Free/2),
			"unrealizedProfit":   "0",
			"maintMargin":        "0",
			"initialMargin":      "0",
			"crossWalletBalance": fmt.Sprintf("%.12f", a.Free/2),
		})
	}

	return []sandbox.FixtureEntry{
		fixture("GET", "/api/v3/time", map[string]any{"serverTime": 1704067200000}),
		fixture("GET", "/api/v3/exchangeInfo", map[string]any{
			"timezone": "UTC", "serverTime": 1704067200000, "symbols": []any{},
		}),
		fixture("GET", "/fapi/v1/exchangeInfo", map[string]any{
			"timezone": "UTC", "serverTime": 1704067200000, "symbols": []any{},
		}),
		fixture("GET", "/dapi/v1/exchangeInfo", map[string]any{
			"timezone": "UTC", "serverTime": 1704067200000, "symbols": []any{},
		}),
		fixture("GET", "/sapi/v1/margin/allPairs", []any{}),
		fixture("GET", "/sapi/v1/margin/isolated/allPairs", []any{}),
		fixture("GET", "/sapi/v1/capital/config/getall", []any{}),
		fixture("GET", "/api/v3/account", map[string]any{
			"makerCommission": 10, "takerCommission": 10,
			"canTrade": true, "canWithdraw": true, "canDeposit": true,
			"balances": spotBalances,
		}),
		fixture("POST", "/sapi/v1/asset/get-funding-asset", funding),
		fixture("GET", "/sapi/v1/margin/account", map[string]any{
			"borrowEnabled": true, "tradeEnabled": true,
			"totalAssetOfBtc": "100", "totalLiabilityOfBtc": "0", "totalNetAssetOfBtc": "100",
			"userAssets": marginAssets,
		}),
		fixture("GET", "/dapi/v1/account", map[string]any{
			"assets": coinmAssets, "positions": []any{}, "canTrade": true,
		}),
		fixture("GET", "/fapi/v3/account", map[string]any{
			"assets": usdmAssets, "positions": []any{}, "canTrade": true,
			"totalWalletBalance": "1000000",
		}),
		fixture("GET", "/sapi/v1/simple-earn/flexible/position", earnFlexibleRows(len(manyAssets))),
		fixture("GET", "/sapi/v1/simple-earn/locked/position", earnLockedRows(len(manyAssets))),
	}
}

// earnFlexibleRows builds a Simple Earn flexible page. total is reported
// honestly as the number of rows so a correct pager stops after one page.
func earnFlexibleRows(n int) map[string]any {
	rows := make([]map[string]any, 0, n)
	for i, a := range manyAssets {
		if i >= n {
			break
		}
		rows = append(rows, map[string]any{
			"asset":                      a.Asset,
			"totalAmount":                fmt.Sprintf("%.12f", a.Free/8),
			"latestAnnualPercentageRate": "0.0523",
			"cumulativeTotalRewards":     "12.5",
			"collateralAmount":           "0",
			"canRedeem":                  true,
			"productId":                  a.Asset + "001",
		})
	}
	return map[string]any{"rows": rows, "total": len(rows)}
}

func earnLockedRows(n int) map[string]any {
	rows := make([]map[string]any, 0, n)
	for i, a := range manyAssets {
		if i >= n {
			break
		}
		rows = append(rows, map[string]any{
			"asset":             a.Asset,
			"amount":            fmt.Sprintf("%.12f", a.Free/16),
			"APY":               "0.1200",
			"status":            "HOLDING",
			"duration":          "90",
			"rewardAmt":         "3.25",
			"rewardAsset":       a.Asset,
			"canRedeemEarly":    true,
			"redeemAmountEarly": "1.0",
			"positionId":        i + 1,
		})
	}
	return map[string]any{"rows": rows, "total": len(rows)}
}

// okxManyAssetFixtures covers the OKX funding wallet and the metadata endpoints
// CCXT needs. The trading wallet (/api/v5/account/balance) is owned by the
// stateful simulator, not by fixtures — see the shadowing test.
func okxManyAssetFixtures() []sandbox.FixtureEntry {
	fundingBalances := make([]map[string]any, 0, len(manyAssets))
	currencies := make([]map[string]any, 0, len(manyAssets))
	for _, a := range manyAssets {
		fundingBalances = append(fundingBalances, map[string]any{
			"ccy":       a.Asset,
			"bal":       fmt.Sprintf("%.12f", a.Free/4),
			"availBal":  fmt.Sprintf("%.12f", a.Free/4),
			"frozenBal": fmt.Sprintf("%.12f", a.Locked/4),
		})
		currencies = append(currencies, map[string]any{
			"ccy": a.Asset, "name": a.Asset, "chain": a.Asset + "-" + a.Asset,
			"canDep": true, "canWd": true, "canInternal": true,
			"minWd": "0.001", "maxFee": "0.1", "minFee": "0.01",
		})
	}

	// A trading-wallet fixture that the simulator will shadow.
	tradingDetails := make([]map[string]any, 0, len(manyAssets))
	for _, a := range manyAssets {
		tradingDetails = append(tradingDetails, map[string]any{
			"ccy":       a.Asset,
			"availBal":  fmt.Sprintf("%.12f", a.Free),
			"frozenBal": fmt.Sprintf("%.12f", a.Locked),
			"eq":        fmt.Sprintf("%.12f", a.Free+a.Locked),
		})
	}

	return []sandbox.FixtureEntry{
		fixture("GET", "/api/v5/public/instruments", map[string]any{
			"code": "0", "msg": "", "data": []any{},
		}),
		fixture("GET", "/api/v5/asset/currencies", map[string]any{
			"code": "0", "msg": "", "data": currencies,
		}),
		fixture("GET", "/api/v5/asset/balances", map[string]any{
			"code": "0", "msg": "", "data": fundingBalances,
		}),
		fixture("GET", "/api/v5/account/balance", map[string]any{
			"code": "0", "msg": "", "data": []any{map[string]any{"details": tradingDetails}},
		}),
	}
}
