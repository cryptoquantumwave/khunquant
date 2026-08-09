package sandbox

import (
	"encoding/json"
	"strconv"
)

// SeedMarketsFromFixtures populates a VenueState's Markets map from the recorded fixture payloads.
// For Binance USDM (/fapi/v1/exchangeInfo), parses contractSize and LOT_SIZE minQty.
// For OKX Swaps (/api/v5/public/instruments instType="SWAP"), parses ctVal and minSz.
// Symbols are normalized to CCXT format (e.g., "BTC/USDT").
func SeedMarketsFromFixtures(venue string, store *Store, state *VenueState) {
	if state == nil || venue == "" {
		return
	}

	// Retrieve the fixture for exchangeInfo / instruments.
	var fixtures []FixtureEntry
	if venue == "binance" {
		// Look for /fapi/v1/exchangeInfo fixture (for USDM futures)
		fixtures = store.GetFixtures(venue)
		seedBinancemarkets(fixtures, state)
	} else if venue == "okx" {
		// Look for /api/v5/public/instruments fixture (for Swaps)
		fixtures = store.GetFixtures(venue)
		seedOKXMarkets(fixtures, state)
	}
}

// seedBinanceMarkets extracts Market data from the /fapi/v1/exchangeInfo fixture.
func seedBinancemarkets(fixtures []FixtureEntry, state *VenueState) {
	for _, fixture := range fixtures {
		if fixture.PathPrefix != "/fapi/v1/exchangeInfo" {
			continue
		}

		// Parse the fixture response body (should be {"symbols": [...]})
		var exchangeInfo map[string]interface{}
		if err := json.Unmarshal(fixture.Response.Body, &exchangeInfo); err != nil {
			continue
		}

		symbols, ok := exchangeInfo["symbols"].([]interface{})
		if !ok {
			continue
		}

		// Iterate over symbols and extract market data
		for _, sym := range symbols {
			symbolMap, ok := sym.(map[string]interface{})
			if !ok {
				continue
			}

			symbol, _ := symbolMap["symbol"].(string)
			contractSizeRaw, _ := symbolMap["contractSize"].(float64)

			// Parse filters to extract minQty from LOT_SIZE
			var minQty float64 = 0.001 // default fallback
			if filters, ok := symbolMap["filters"].([]interface{}); ok {
				for _, f := range filters {
					filterMap, ok := f.(map[string]interface{})
					if !ok {
						continue
					}
					if filterType, ok := filterMap["filterType"].(string); ok && filterType == "LOT_SIZE" {
						if minQtyStr, ok := filterMap["minQty"].(string); ok {
							if parsed, err := strconv.ParseFloat(minQtyStr, 64); err == nil {
								minQty = parsed
							}
						}
					}
				}
			}

			// Store using the raw wire-format symbol (e.g., "BTCUSDT"), not CCXT format.
			// The simulator looks up symbols using r.FormValue("symbol"), which gives the raw wire format
			// that CCXT sends in HTTP requests. CCXT-to-wire format translation happens on the client side.
			state.Markets[symbol] = Market{
				Symbol:       symbol,
				ContractSize: contractSizeRaw,
				MinAmount:    minQty,
				MaxLeverage:  125, // Binance USDM default max leverage
			}
		}
		break // Found and processed the exchangeInfo fixture
	}
}

// seedOKXMarkets extracts Market data from the /api/v5/public/instruments fixture.
func seedOKXMarkets(fixtures []FixtureEntry, state *VenueState) {
	for _, fixture := range fixtures {
		if fixture.PathPrefix != "/api/v5/public/instruments" {
			continue
		}

		// Parse the fixture response body (should be {"code":"0", "data": [...]})
		var resp map[string]interface{}
		if err := json.Unmarshal(fixture.Response.Body, &resp); err != nil {
			continue
		}

		dataRaw, ok := resp["data"].([]interface{})
		if !ok {
			continue
		}

		// Iterate over instruments and extract swap market data
		for _, inst := range dataRaw {
			instMap, ok := inst.(map[string]interface{})
			if !ok {
				continue
			}

			// Only process SWAP instruments
			instType, _ := instMap["instType"].(string)
			if instType != "SWAP" {
				continue
			}

			instId, _ := instMap["instId"].(string)
			ctValRaw, _ := instMap["ctVal"].(string)
			minSzRaw, _ := instMap["minSz"].(string)

			// Parse ctVal and minSz (both are strings in OKX fixtures)
			var ctVal float64 = 1.0  // default
			var minSz float64 = 0.01 // default
			if parsed, err := strconv.ParseFloat(ctValRaw, 64); err == nil {
				ctVal = parsed
			}
			if parsed, err := strconv.ParseFloat(minSzRaw, 64); err == nil {
				minSz = parsed
			}

			// OKX instId is already in the normalized format (e.g., "BTC-USDT-SWAP")
			// Convert to CCXT format (BTC-USDT-SWAP -> BTC/USDT for swaps, or keep as-is)
			ccxtSymbol := instId

			state.Markets[ccxtSymbol] = Market{
				Symbol:       ccxtSymbol,
				ContractSize: ctVal,
				MinAmount:    minSz,
				MaxLeverage:  100, // OKX default max leverage for swaps
			}
		}
		break // Found and processed the instruments fixture
	}
}
