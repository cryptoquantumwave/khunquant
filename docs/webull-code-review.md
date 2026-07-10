# Webull Feature — Code Review Report

- **Date:** 2026-07-10
- **Scope:** `feature/webull` vs `main` (59 files, ~7,900 added lines)
- **Effort:** medium (8 finder angles → 36 candidates → verified, deduped)
- **Focus:** architecture, scalability, testability, maintainability, security
- **Result:** 8 findings survived verification (6 confirmed, 2 plausible), plus minor cleanups. Refuted candidates listed at the end.

---

## Overall assessment

The integration is well-structured: it follows the established OKX/Settrade layering (raw `client.go` + `broker_adapter.go` implementing `broker.Provider`, dual registration in `broker.RegisterFactory` and `exchanges.RegisterFactory`), has strong test coverage (~2,900 test lines including httptest-based client tests and a sandbox live-test file), guards error paths at call sites, and documents API quirks inline. The findings below are concentrated in three themes:

1. **Stale "read-only" assumptions** — the adapter gained `TradingProvider` during the branch's life, but code/comments written when it was read-only were not revisited (holiday calendar, test comments).
2. **N+1 API-call patterns** — several read paths make per-symbol or duplicate calls where the Webull API supports batching. This matters for rate limits and for the project's low-resource target.
3. **Parallel equity/option code paths** — validation and conversion logic is duplicated between the equity and option methods, and between the adapter and the tool layer.

---

## Findings (ranked)

### 1. `GetMarketStatus` ignores US market holidays while gating live trading — CONFIRMED

**File:** `pkg/exchanges/webull/broker_adapter.go:38`

The TODO says:

> "no US market holiday calendar — this will incorrectly report MarketOpen on holidays/half-days. Harmless while trading is deferred (webullAdapter does not implement broker.TradingProvider)…"

That claim is now false: the adapter implements the full `TradingProvider` surface (`CreateOrder` at line 506, `CancelOrder`, `FetchOrder`, `PlaceOptionOrder`, …), and `GetMarketStatus` is consumed as a pre-trade gate (`pkg/tools/order_create.go:127`) and by DCA execution.

**Failure scenario:** a DCA plan fires on Thanksgiving; the client-side gate reports `MarketOpen`, the order proceeds, and either fails at the Webull API with a confusing error or (on half-days) executes at an unexpected time.

**Solution:**
- Add a US market holiday/half-day calendar to `GetMarketStatus`. A static table of NYSE full/half holidays (updated yearly, ~15 entries/year) is sufficient and dependency-free; the DN/earn code base pattern of embedding small static tables applies here.
- Alternatively (deeper fix): query Webull's trade-calendar endpoint if the OpenAPI exposes one, and cache it for the day.
- Either way, delete the stale "harmless while trading is deferred" sentence and update the matching stale comment in `pkg/tools/dca_create_plan_test.go:49` (see finding 9).

### 2. `parseFloat` silently converts malformed API values to `0`, flowing into option quotes — CONFIRMED

**File:** `pkg/exchanges/webull/types.go:307`, consumed at `broker_adapter.go:390–409`

`parseFloat` returns `0` on any parse error. `FetchPrice` guards its result (`if price <= 0 { return error }`), but `FetchOptionSnapshot` appends `parseFloat(...)` results for price, bid, ask, and greeks directly into `OptionQuote` with no guard.

**Failure scenario:** Webull returns an empty/`"NaN"`/malformed price field → the option quote shows $0 price and zero greeks; anything downstream that sizes, values, or alerts off these quotes treats the contract as worthless — silently.

**Solution:**
- Change `parseFloat` to return `(float64, error)` (or add a `parseFloatStrict`) and propagate the error in `FetchOptionSnapshot`, mirroring the `FetchPrice` guard.
- Where a zero is legitimately possible (e.g. bid on an illiquid contract), distinguish "field present and 0" from "field absent/unparseable" — only the former should pass through.
- Minor related cleanup: this `parseFloat` duplicates `pkg/deltaneutral/fees/okx.go:170`; when touching it, consider extracting one shared helper.

### 3. No retry/backoff for 429/5xx — hard failure under rate limiting — CONFIRMED

**File:** `pkg/exchanges/webull/client.go:138` (`doRequest`)

`doRequest` retries only once, and only for auth errors (`isAuthError`: 401 / TOKEN / UNAUTHORIZED). There is no handling for `429 Too Many Requests` or transient 5xx, and no backoff — even though `pkg/utils/http_retry.go` already implements exactly this (`shouldRetry` = 429 or ≥500, `retryDelayForAttempt` honors `Retry-After`).

**Failure scenario:** a portfolio snapshot or `FetchTickers` burst trips Webull's rate limit → every call fails immediately; DCA execution and snapshot collection error out where a short backoff would have succeeded transparently. Combined with findings 4–6 (N+1 calls), the integration both *causes* and *cannot survive* rate limiting.

**Solution:**
- Don't call `utils.DoRequestWithRetry` directly — Webull requests are HMAC-signed with a per-request timestamp/nonce, so a retried request must be **re-signed**. Instead, extend the existing retry loop in `doRequest` (signing already happens inside the loop, so each attempt gets fresh headers):
  - reuse `shouldRetry(statusCode)` and `retryDelayForAttempt(resp, attempt)` from `pkg/utils/http_retry.go` (export or mirror them),
  - cap attempts (e.g. 3) and respect `ctx` cancellation via the existing `sleepWithCtx` pattern.
- Keep the current 401 re-auth branch as-is; it composes with the new transient-error branch.

### 4. `FetchTickers` is N+1: one API call per symbol despite a batch-capable endpoint — CONFIRMED

**File:** `pkg/exchanges/webull/broker_adapter.go:285`

```go
for _, sym := range symbols { t, err := a.FetchTicker(ctx, sym) ... }
```

Each `FetchTicker` calls `client.FetchSnapshot(ctx, []string{sym})`, but `FetchSnapshot` already accepts a symbol slice and joins it into one request (`query.Set("symbols", strings.Join(syms, ","))`).

**Failure scenario:** a 100-symbol watchlist/portfolio → 100 HTTP round-trips (each also paying category resolution, finding 5) instead of 1–2 batched calls. Slow, and burns straight into the rate limit (finding 3).

**Solution:** implement `FetchTickers` as a single `a.client.FetchSnapshot(ctx, symbols)` call and map the results; keep the per-symbol loop only as a fallback for symbols missing from the batch response. Respect any API max-symbols-per-request limit by chunking (e.g. 50/call).

### 5. `FetchSnapshot` resolves instrument category per symbol — up to 2 probe calls each — CONFIRMED

**File:** `pkg/exchanges/webull/client.go:456` (`resolveCategoryForSymbol` at ~283)

For every uncached symbol, the client probes `US_STOCK` and falls back to `US_ETF` — sequentially, one symbol at a time — before making the final grouped snapshot request.

**Failure scenario:** 50 uncached mixed symbols → up to 100 probe calls + the real requests. On first run (cold cache) this is the dominant cost of any portfolio-wide operation.

**Solution:** batch the probe: request all unresolved symbols as `US_STOCK` in one call, collect the misses, retry only those as `US_ETF` in a second call (2 calls total, any N). Keep the existing per-symbol cache so subsequent calls stay free. This also collapses naturally into the finding-4 fix since `FetchSnapshot` already receives the full list.

### 6. `GetWalletBalances("all")` fetches positions twice and duplicates filter branches — CONFIRMED

**File:** `pkg/exchanges/webull/broker_adapter.go:200` (branches at ~132–142 and ~167–177)

The `"all"` case recursively calls `GetWalletBalances("cash"|"stock"|"option")`; the stock and option branches each call `client.FetchPositions(ctx)` and then filter with near-identical loops (`InstrumentType != "EQUITY"` vs `!= "OPTION"`).

**Failure scenario:** every `"all"` query (the common case for snapshots/PnL) costs 2 `FetchPositions` API calls plus duplicate slice allocations where 1 call + in-memory partition suffices; the twin filter loops are also a divergence risk when position mapping changes.

**Solution:** restructure so positions are fetched once:

```go
positions, err := a.client.FetchPositions(ctx)
// ...
stock  := balancesFromPositions(positions, "EQUITY")
option := balancesFromPositions(positions, "OPTION")
```

with a single `balancesFromPositions(positions, instrumentType)` helper used by both the individual wallet types and the `"all"` case.

### 7. `FetchInstruments` claims "pagination required" but never paginates — PLAUSIBLE

**File:** `pkg/exchanges/webull/client.go:438` (docstring at ~423)

The docstring states "If symbols is empty, fetches all tradable instruments (pagination required)", but the implementation issues a single `doRequest` with no page/cursor loop.

**Failure scenario:** if the instruments endpoint pages (as the author's own docstring asserts), any result set beyond page 1 is silently truncated — missing instruments with no error. Marked plausible because the actual page size/behavior of the endpoint isn't verifiable from the code alone.

**Solution:** check the Webull OpenAPI spec (`docs/webull-api-spec.md`) for the endpoint's paging contract; implement a cursor/`last_instrument_id` loop with a sane page cap. If the endpoint genuinely doesn't page, fix the docstring so the next reader doesn't chase this.

### 8. `amount_unit` rule has two sources of truth: dynamic `Category()` at create, hardcoded provider list at update — PLAUSIBLE

**Files:** `pkg/tools/dca_create_plan.go:184` vs `pkg/tools/dca_update_plan.go:210` (+ `isStockProvider` at :427)

`dca_create_plan` decides stock-vs-crypto behavior from the resolved provider: `p.Category() == broker.CategoryStock`. `dca_update_plan` instead calls `isStockProvider(plan.Provider)`, which is a hardcoded string list: `"settrade" || "webull"`.

**Failure scenario:** no active bug today (both listed providers are `CategoryStock`), but the next stock broker added to the catalog gets correct `amount_unit` defaults at create time and is silently skipped at update time — plans mutate under crypto-style rules. This is exactly the "special case layered on shared infrastructure" pattern: the broker catalog already knows each provider's category.

**Solution:** delete `isStockProvider` and resolve the provider in `dca_update_plan` the same way create does (`broker.CreateProviderForAccount` → `p.Category()`). If resolving a live provider in the update path is undesirable, expose the category from the static catalog (`pkg/providers/broker/catalog.go`) so no network client is needed.

---

## Minor / hygiene (fix opportunistically)

9. **Stale test comment** — `pkg/tools/dca_create_plan_test.go:49` says `mockNonTradingProvider` "mirrors Webull's adapter (read-only … no order execution)". Webull now implements `TradingProvider`. Reword (e.g. "mirrors a hypothetical read-only market-data provider") so future readers don't conclude Webull can't trade. *(CONFIRMED, trivial)*

10. **Per-exchange helper triplication in TUI/onboard** — `cmd/khunquant-launcher-tui/internal/ui/exchange.go:442–462+` now has byte-identical `remove{OKX,Settrade,Webull}Account` and `{okx,settrade,webull}AccountNames` triples; `cmd/khunquant/internal/onboard/portfolio.go:126–160` has four `appendIfNoMain*` variants. Each new broker adds another copy. Replace with generics:
    ```go
    func removeAccount[T any](accounts []T, i int) []T
    func accountNames[T interface{ GetName() string }](accounts []T) []string
    func appendIfNoMain[T any](accounts []T, name func(T) string, placeholder T) []T
    ```
    *(CONFIRMED — worth doing before a 4th broker lands.)*

11. **Equity/option method-pair duplication in the adapter** — `CancelOrder`/`CancelOptionOrder`, `FetchOpenOrders`/`FetchOpenOptionOrders`, and the bar→OHLCV conversion in `FetchOHLCV`/`FetchOptionOHLCV` are near-identical pairs; option order validation is also repeated between `PlaceOptionOrder` and `pkg/tools/option_create_order.go`. Extract shared helpers (`cancelByClientOrderID`, `fetchOpenOrdersFiltered(pred)`, `barsToOHLCV`, `validateOptionOrderRequest`) so equity/option paths can't drift. *(CONFIRMED duplication; refactor, no behavior change.)*

12. **Weak nonce fallback** — `pkg/exchanges/webull/signer.go:144`: if `crypto/rand.Read` fails, the nonce falls back to `time.Now().UnixNano()%256 ^ byte(i)`. This does **not** weaken the HMAC itself (the key is secret) but degrades replay protection if the server checks nonce freshness. `crypto/rand` failure is effectively fatal on any supported platform — prefer returning an error (or panicking) over emitting a predictable nonce. *(PLAUSIBLE, low)*

13. **Host fallback masks misconfiguration** — `pkg/exchanges/webull/client.go:122`: if a custom `baseURL` parses to an empty host, signing silently falls back to `api.webull.com`, producing confusing mismatched-host failures. Validate the URL in `WithBaseURL` and return a construction error instead. *(PLAUSIBLE, low)*

14. **Symbol format inconsistency in returned orders** — `PlaceOptionOrder` returns `Symbol` as the bare underlying (`AAPL`), `CreateOrder` returns CCXT format (`AAPL/USD`), and `orderItemToCCXT` (~:954) appends `/USD` to option orders because `OrderItem` lacks strike/expiry for OCC encoding. Today `Symbol` is display-only in the option tools, so nothing breaks — but pick one convention (suggest: OCC symbol for options wherever the leg data exists, documented on the interface) before consumers start matching on it. *(PLAUSIBLE)*

15. **Test helper reimplements stdlib** — `pkg/exchanges/webull/client_test.go:561` defines a manual `contains()`; use `strings.Contains`. *(CONFIRMED, trivial)*

---

## Verified-and-refuted candidates (no action needed)

For transparency, these were raised by finders and **refuted** during verification:

- **OCC symbol empty-string on bad expiry** — guarded: both call sites check `if encoded == "" { return error }` before any API call (`broker_adapter.go:363, 425`).
- **Nil `Price` deref for STOP_LOSS option orders** — all consumers nil-check before dereferencing (`option_get_order.go:88`, `option_open_orders.go:94`).
- **Dual `init()` registration (broker + exchanges registries)** — matches the established OKX/Settrade wiring exactly; intentional pattern, not ambiguity.
- **PnL market-price key mismatch** — `parseExtraFloatAny(extra, "market_price", "current_price")` covers both Settrade (`market_price`) and Webull (`current_price`) keys.
- **`walletTypeForPnL` signature change breaking callers** — all call sites pass the new `broker.AssetCategory` argument; compile-time enforced.
- **CLAUDE.md convention violations** — none; the documented futures pitfalls (contract sizing, side mapping) don't apply to this equities/options-only integration.

---

## Suggested fix order

1. **Before merge:** findings 1 (holiday gate) and 2 (silent-zero quotes) — both put real orders/valuations at risk; plus the trivial comment fixes (9).
2. **Before scaling usage:** findings 3–6 together (retry/backoff + batching) — they share the rate-limit story and mostly touch `client.go`/`broker_adapter.go` read paths.
3. **Next touch of the area:** 7 (pagination check against the API spec), 8 (`isStockProvider` removal), 10–11 (dedup refactors), 12–15 (hygiene).
