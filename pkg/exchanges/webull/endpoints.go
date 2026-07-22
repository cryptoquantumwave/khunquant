package webull

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

const (
	Name = "webull"

	// Sandbox/UAT host, verified against the shared sandbox creds
	// (not api.sandbox.webull.com — that doesn't work with shared creds).
	// Only verified for the US region; other regions fall back to this same
	// host until a region-specific sandbox is confirmed.
	uatHost = "us-openapi-alb.uat.webullbroker.com"

	// Authentication endpoints
	endpointTokenCreate = "/openapi/auth/token/create"
	endpointTokenCheck  = "/openapi/auth/token/check"

	// Account and portfolio endpoints
	endpointAccountList = "/openapi/account/list"
	endpointBalance     = "/openapi/assets/balance"
	endpointPositions   = "/openapi/assets/positions"

	// Instrument endpoints
	endpointInstrumentStockList = "/openapi/instrument/stock/list"

	// Market data endpoints (equity/ETF)
	endpointSnapshot = "/openapi/market-data/stock/snapshot"
	endpointBars     = "/openapi/market-data/stock/bars"

	// Market data endpoints (options)
	endpointOptionSnapshot = "/openapi/market-data/option/snapshot"
	endpointOptionBars     = "/openapi/market-data/option/bars"

	// Trading endpoints (add as constants for future implementation)
	endpointOrderPlace   = "/openapi/trade/order/place"
	endpointOrderPreview = "/openapi/trade/order/preview"
	endpointOrderCancel  = "/openapi/trade/order/cancel"
	endpointOrderReplace = "/openapi/trade/order/replace"
	endpointOrderOpen    = "/openapi/trade/order/open"
	endpointOrderHistory = "/openapi/trade/order/history"
	endpointOrderDetail  = "/openapi/trade/order/detail"
)

// prodHostForRegion maps a Webull region to its production API host. Webull
// operates entirely separate regional brokers (US, HK, JP, SG, TH, AU, MY,
// UK, EU, ...) each with their own signup portal, host, and app credentials
// — an app key registered on Webull Thailand is NOT valid against
// api.webull.com (US), and vice versa. Table verified against the official
// webull-openapi-python-sdk's region_mapping
// (webull/core/data/endpoints.json).
//
// An empty region resolves to the Thailand host: TH is the only region this
// integration supports today (see NormalizeRegion), so defaulting to
// api.webull.com would send the only credentials anyone can configure to a
// broker that always rejects them with 401.
func prodHostForRegion(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "th", "":
		return "api.webull.co.th"
	case "hk":
		return "api.webull.hk"
	case "jp":
		return "api.webull.co.jp"
	case "sg":
		return "api.webull.com.sg"
	case "au", "za": // za shares au's host per the SDK's region_mapping
		return "api.webull.com.au"
	case "my":
		return "api.webull.com.my"
	case "uk":
		return "api.webull-uk.com"
	case "eu":
		return "api.webull.eu"
	case "us", "br", "mx":
		return "api.webull.com"
	default:
		return "api.webull.com"
	}
}

// knownRegions lists every region prodHostForRegion recognizes, for
// validation and error messages. Keep in sync with the switch above.
var knownRegions = []string{"us", "hk", "jp", "sg", "th", "au", "my", "uk", "eu", "br", "mx", "za"}

// supportedRegions lists the regions this integration is actually verified
// against today. The host table above covers every Webull regional broker,
// but only Thailand has been exercised end-to-end (app registration, token
// approval, trading), so the rest are surfaced as "not available yet"
// instead of being silently attempted — see NormalizeRegion.
var supportedRegions = []string{"th"}

// DefaultRegion is the region assumed when none is configured. It is the
// only supported region, not a neutral fallback — config entry points seed
// new accounts with it so a fresh account is never written pointing at a
// broker that cannot accept its credentials.
const DefaultRegion = "th"

// legacyDefaultRegion is the region every entry point used to default to
// before Thailand became the default. Configs written by those versions
// carry it without the user ever having chosen it, so NormalizeRegion
// rewrites it rather than failing an install that was never misconfigured
// on purpose.
const legacyDefaultRegion = "us"

// KnownRegions returns every region the host table recognizes. Callers must
// not mutate the result.
func KnownRegions() []string { return slices.Clone(knownRegions) }

// SupportedRegions returns the regions that can actually be configured
// today. Used by the launcher UI to build the region picker.
func SupportedRegions() []string { return slices.Clone(supportedRegions) }

// NormalizeRegion resolves a configured region to the canonical value the
// client should use, or explains why it can't be used.
//
//   - empty            → "th" (the default and only supported region)
//   - "th"             → "th"
//   - "us"             → "th", with a warning: "us" was the default every
//     entry point wrote before TH support landed, so a config carrying it
//     almost certainly never had a region chosen at all. Failing here would
//     break existing installs over a value the user never picked.
//   - another known region (hk, jp, …) → error, not available yet
//   - anything else    → the unknown-region error from ValidateRegion
func NormalizeRegion(region string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(region))
	if r == "" {
		return DefaultRegion, nil
	}
	if slices.Contains(supportedRegions, r) {
		return r, nil
	}
	if err := ValidateRegion(r); err != nil {
		return "", err
	}
	if r == legacyDefaultRegion {
		logger.Warn(fmt.Sprintf("webull: region %q is the pre-Thailand default and is not supported — using %q instead; set region explicitly to silence this", r, DefaultRegion))
		return DefaultRegion, nil
	}
	return "", fmt.Errorf("webull: region %q is not available yet — only Thailand (%s) is supported", region, strings.Join(supportedRegions, ", "))
}

// ValidateRegion returns an error when region is a non-empty string not in
// the known table. A silent fallback to the US host here has already cost a
// multi-hour debugging session (a typo'd region sends valid credentials to
// the wrong regional broker, which answers with an indistinguishable 401
// "Invalid credentials"), so unknown regions are rejected at client
// construction instead. Empty region intentionally remains valid and
// defaults to the US host.
func ValidateRegion(region string) error {
	r := strings.ToLower(strings.TrimSpace(region))
	if r == "" || slices.Contains(knownRegions, r) {
		return nil
	}
	return fmt.Errorf("webull: unknown region %q (valid regions: %s) — credentials only work against their own region's host, so a wrong region always fails with 401", region, strings.Join(knownRegions, ", "))
}

// HostForEnvironment returns the API host for the given environment and
// region. Sandbox/UAT is only verified for the US region today; region is
// otherwise ignored for uat/sandbox and always uses the shared sandbox host.
func HostForEnvironment(environment, region string) string {
	switch environment {
	case "uat", "sandbox":
		return uatHost
	default:
		return prodHostForRegion(region)
	}
}

// BaseURLForEnvironment returns the full base URL (https://) for the given
// environment and region.
func BaseURLForEnvironment(environment, region string) string {
	host := HostForEnvironment(environment, region)
	return "https://" + host
}
