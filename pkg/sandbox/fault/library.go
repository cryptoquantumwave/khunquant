package fault

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// libEntry is a venue's error as it travels on the wire.
type libEntry struct {
	status int
	body   string
}

// The codes are the ones the pinned ccxt (v4.5.85) turns into exception classes, so the real
// classifier and broker.PlaceOrderIdempotent run on them; TestLibrary_CCXTClassifies pins
// every row. The messages are neutral on purpose: ccxt also matches on message text.
//
// Two Binance facts the pinned ccxt imposes (both pinned by that test, both for SBX-14a):
//   - HTTP 429 and 418 are DDoSProtection whatever the body says, never RateLimitExceeded.
//   - ccxt picks Binance's per-market tables (-2019 insufficient margin is InsufficientFunds
//     only in "linear") from the request URL's real host. The lab rewrites that host, so only
//     the common table applies and -2019 is a plain ExchangeError, an unknown outcome to the
//     broker. The insufficient_funds row uses -2010 with Binance's exact message, which the
//     common table maps; the simulator's -2015 (AuthenticationError) is wrong either way.
var library = map[string]map[string]libEntry{
	"binance": {
		"rate_limit": {429, `{"code":-1003,"msg":"Too many requests."}`},
		"backend_timeout": {503, `{"code":-1007,"msg":"Timeout waiting for response from backend server. ` +
			`Send status unknown; execution status unknown."}`},
		"invalid_order":      {400, `{"code":-2010,"msg":"New order rejected."}`},
		"order_not_found":    {400, `{"code":-2013,"msg":"Order does not exist."}`},
		"auth":               {401, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`},
		"bad_symbol":         {400, `{"code":-1121,"msg":"Invalid symbol."}`},
		"insufficient_funds": {400, `{"code":-2010,"msg":"Account has insufficient balance for requested action."}`},
	},
	"okx": {
		"rate_limit":      {429, `{"code":"50011","msg":"Rate limit reached.","data":[]}`},
		"backend_timeout": {200, `{"code":"50004","msg":"Endpoint request timeout.","data":[]}`},
		"insufficient_funds": {200, `{"code":"1","msg":"","data":[{"clOrdId":"","ordId":"","sCode":"51008",` +
			`"sMsg":"Order failed.","tag":""}]}`},
		"invalid_order": {200, `{"code":"1","msg":"","data":[{"clOrdId":"","ordId":"","sCode":"51016",` +
			`"sMsg":"Duplicated client order ID.","tag":""}]}`},
		"order_not_found": {200, `{"code":"51603","msg":"Order does not exist","data":[]}`},
		"bad_symbol":      {200, `{"code":"51001","msg":"Instrument ID does not exist","data":[]}`},
	},
	"bitkub": generic(func(status int, name string) string {
		return fmt.Sprintf(`{"error":%d,"message":"sandbox fault: %s"}`, status, name)
	}),
	"settrade": generic(func(_ int, name string) string {
		return fmt.Sprintf(`{"code":"SANDBOX","message":"sandbox fault: %s"}`, name)
	}),
	"webull": generic(func(_ int, name string) string {
		return fmt.Sprintf(`{"message":"sandbox fault: %s","error_code":"SANDBOX"}`, name)
	}),
}

// libraryAlias lets a venue that speaks another's API use that venue's errors.
var libraryAlias = map[string]string{"binanceth": "binance"}

// servedVenue says whether the sandbox serves venue: a venue of the library, or an alias.
func servedVenue(venue string) bool {
	_, ok := library[libVenue(venue)]
	return ok
}

// generic is the three errors of the venues whose clients are untyped: the status alone
// drives them (429 and 5xx are retried, and a 5xx on an order has an unknown outcome).
func generic(body func(status int, name string) string) map[string]libEntry {
	out := map[string]libEntry{}
	for name, status := range map[string]int{"rate_limit": 429, "server_error": 503, "bad_request": 400} {
		out[name] = libEntry{status, body(status, name)}
	}
	return out
}

var rawCode = regexp.MustCompile(`^-?[0-9]{1,6}$`)

func libVenue(v string) string {
	if a, ok := libraryAlias[v]; ok {
		return a
	}
	return v
}

// libraryHas says whether venue (a concrete one) has an error of that name or, when rawOK,
// a raw numeric code that it can write in its own shape.
func libraryHas(venue, name string, rawOK bool) bool {
	v := libVenue(venue)
	if _, ok := library[v][name]; ok {
		return true
	}
	return rawOK && rawCode.MatchString(name) && (v == "binance" || v == "okx")
}

// libraryResponse is the venue's answer for a library name or, for a reject, a raw code.
func libraryResponse(venue, name string) (*sandbox.Response, bool) {
	v := libVenue(venue)
	e, ok := library[v][name]
	if !ok {
		if !rawCode.MatchString(name) {
			return nil, false
		}
		switch v {
		case "binance":
			e = libEntry{400, `{"code":` + name + `,"msg":"Order rejected."}`}
		case "okx":
			e = libEntry{200, `{"code":"1","msg":"","data":[{"clOrdId":"","ordId":"","sCode":"` + name + `","sMsg":"Order rejected.","tag":""}]}`}
		default:
			return nil, false
		}
	}
	return &sandbox.Response{Status: e.status, Body: json.RawMessage(e.body), Headers: map[string]string{"Content-Type": "application/json"}}, true
}
