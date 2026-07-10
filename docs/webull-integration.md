# Webull US-Stock Integration — Progress Tracker

Head of Engineering: Opus 4.8 (integrator/reviewer). Implementation fanned out to Sonnet 5 subagents.
Scope: **read-only + market data** (Portfolio + MarketData providers; no trading yet). Minimal wiring.
Plan: `~/.claude/plans/let-s-design-the-technical-reflective-wozniak.md`.

## Wave 5 — Code-review remediation (all 15 findings) — ✅ COMPLETE

Source: `docs/webull-code-review.md` (fable5 review of `feature/webull`, 10 CONFIRMED + 5 PLAUSIBLE).
Branch is unmerged, so all tiers were done "before the PR." Every fix has test coverage; full suite
`go test ./...` → 81 pkg ok; gofmt/vet clean on changed files; signer KAT unchanged in value.

| # | Finding | Fix | Key files |
|---|---|---|---|
| F1 | Holiday-blind `GetMarketStatus` gating live trades | **Computed NYSE calendar** (fixed + floating holidays with Sat/Sun observance, Good Friday via Easter computus, Mon–Thu half-days) — works for any year, no yearly maintenance; pure `usMarketStatusAt` unit-tested incl. 2028+/observance edges; stale comment deleted | `broker_adapter.go`, `adapter_test.go` |
| F2 | `parseFloat` silently zeroes malformed option prices | Added `parseFloatStrict`; **Price** guarded strictly in `FetchOptionSnapshot`; bid/ask/greeks keep 0-legal | `types.go`, `broker_adapter.go` |
| F3 | No 429/5xx retry in `doRequest` | Dual-policy loop: auth-retry once (immediate) + transient-retry ≤3 (backoff, re-signed); exported `utils.ShouldRetry/RetryDelayForAttempt/SleepWithCtx` | `client.go`, `pkg/utils/http_retry.go` |
| F4 | `FetchTickers` N+1 | Single batched `FetchSnapshot`, map by symbol, per-symbol fallback for misses | `broker_adapter.go` |
| F5 | Per-symbol category probe (N+1) | Batched US_STOCK probe → single US_ETF retry for omissions; **defensive per-symbol fallback if the batch errors** (sandbox can't disambiguate: AAPL-only). Chunked at 100 | `client.go` |
| F6 | `GetWalletBalances("all")` double `FetchPositions` | `balancesFromPositions` helper; one fetch, in-memory partition (asserted in test) | `broker_adapter.go` |
| F7 | `FetchInstruments` claims paging, doesn't | Cursor loop on `last_instrument_id` (page_size 1000) for full listing; single call when symbol-scoped | `client.go` |
| F8 | Two sources of truth for stock `amount_unit` | Static `broker.ProviderCategory`/`IsStockProvider` (no live client); `dca_update_plan` uses it; `isStockProvider` deleted | `pkg/providers/broker/category.go`, `dca_{update,create}_plan.go` |
| F9 | Stale "Webull is read-only" test comment | Reworded | `dca_create_plan_test.go` |
| F10 | Per-broker helper triplication | Generic `removeAccount[T]`, `accountNames[T]`, `appendIfNoMain[T]` via new `ExchangeAccount.GetName()` | `exchange.go`, `portfolio.go`, `config.go` |
| F11 | Equity/option method duplication | Shared `barsToOHLCV`, `cancelByClientOrderID`, `fetchOpenOrdersFiltered`, and `broker.OptionOrderRequest.Validate()` (used by adapter **and** tool; policy gates stay in tool) | `broker_adapter.go`, `provider.go`, `option_create_order.go` |
| F12 | Predictable nonce fallback on `crypto/rand` failure | `randomNonce`/`SignRequest` now return an error (propagated through `doRequest`); no weak fallback | `signer.go`, `client.go` |
| F13 | Silent host fallback masks misconfig | `WithBaseURL` validates host at construction; `doRequest` errors on empty host | `client.go` |
| F14 | Inconsistent option `Symbol` | OCC-encoded symbol for option orders (place + fetched, via parsed response legs); documented on `OptionTradingProvider` | `broker_adapter.go`, `types.go`, `provider.go` |
| F15 | Hand-rolled `contains()` in test | Replaced with `strings.Contains` | `client_test.go` |

Notable design call (F5): the plan proposed a sandbox probe to decide omit-vs-error behavior, but
the sandbox is AAPL-only (`403 INVALID_SYMBOL` for anything else), so the probe is unresolvable —
the implementation is written to be correct under **both** behaviors, making it a non-issue.

## Wave 4 — ETF coverage + Options (read + single-leg trading) — ✅ COMPLETE

Scope correction: crypto/futures were planned but **dropped** — they are US-only Webull products
(Webull Crypto LLC / Webull Futures), unavailable on the user's **Webull Thailand** account, which
trades US equities, ETFs, and options. Sandbox recon confirmed: single-leg option **trading** works
(place→open→cancel verified); option **market data (greeks) is subscription-gated** (US_OPTION);
**ETF market data** isn't sandbox-testable (sandbox = AAPL-only) — implemented per docs, prod-verify.

| Phase | Owner | Status | Notes |
|---|---|---|---|
| **E — ETF market-data category** | sonnet | ✅ approved | `FetchSnapshot`/`FetchBars` resolve US_STOCK→US_ETF fallback (cached per symbol). ETF orders/positions already covered (instrument_type=EQUITY). |
| **O1 — OptionsProvider read** | sonnet | ✅ approved | `broker.OptionMarketDataProvider`/`OptionTradingProvider` + option DTOs; OCC encoded-symbol builder; option snapshot/bars; `option` wallet (positions filtered by instrument_type; **`stock` wallet now filters EQUITY**). Subscription-401 excluded from token-retry. |
| **O2 — single-leg trading + tools** | sonnet + opus | ✅ approved | `PlaceOptionOrder`/Cancel/Get/OpenOrders (single-leg, no MARKET, GTC buy-only, ×100 multiplier); tools `option_quote`/`option_create_order`/`option_cancel_order`/`option_get_order`/`option_open_orders` (registered in instance.go, safety gates). **Integrator added the sandbox option lifecycle the agent skipped** → verified place→open→cancel via the real Go client. `snapshot` positional→by-encoded-symbol match hardened. |
| **O3 — skills + docs** | opus | ✅ done | trading/market-data/portfolios SKILL.md updated (option tools, `option` wallet, ETF, subscription note); this tracker; `docs/webull-api-spec.md` recon section; memory. |

Verified live (build-tagged `sandbox_live_test.go`): equity + **option** place→open→cancel through
the compiled adapter; option wallet shows real OPTION positions; snapshot returns the clean
subscription-gated error. Full suite `go test ./...` → 81 pkg ok. Signer KAT unchanged.

## Status legend
todo · in-progress · in-review · approved · blocked

## Task board

| Task | Owner | Status | Files | Notes |
|---|---|---|---|---|
| **T1 — Config foundation** | sonnet | ✅ approved | `pkg/config/config.go`, `config_struct.go`, `*_test.go` | Mirrors Settrade; `ok pkg/config`, fmt+build clean. 5 files, +233. |
| **T2 — Webull adapter package** | sonnet | ✅ approved | `pkg/exchanges/webull/*`, `pkg/agent/instance.go`, `cmd/.../gateway/helpers.go` | Portfolio+MarketData providers, injectable httptest client, HMAC-SHA1 signer. 1 review cycle (weak nonce + host coupling fixed). 17 tests, build ok. |
| **T3 — Portfolio/market wiring** | sonnet | ✅ approved | `pkg/providers/broker/{catalog,permissions}.go`, `pkg/tools/{list_portfolios,exchange_total_value,exchange_balance,pnl_summary}.go`, `pkg/snapshot/collector.go` | webull enumeration + USD quote + stock wallet + permissions case (mirrors OKX, keeps Permissions). Agent stalled during self-verify but edits complete; I verified build/vet/gofmt + diffs. |
| **T4 — Onboarding wiring** | sonnet | ✅ approved | `cmd/khunquant/internal/onboard/{portfolio,helpers}.go` | Wizard entry + enable switch + `appendIfNoMainWebull` + dev-portal URL. |
| **T5 — TUI launcher wiring** | sonnet | ✅ approved | `cmd/khunquant-launcher-tui/internal/ui/exchange.go` | Menu + `webullAccountForm` (AccountID+Region, no PIN) + helpers + hasEnabled/countExchanges. |
| **T6 — Integration & verify** | opus | ✅ approved | (whole repo) | gofmt fixed gateway/helpers.go; `go build ./...`, `go vet ./...`, `go test ./...` all green; registration smoke test passed then removed. |

## Wave 2 — coverage extension (tools, skills, web UI)

| Task | Owner | Status | Files | Notes |
|---|---|---|---|---|
| **W1 — DCA/PnL tools** | sonnet | ✅ approved | `pkg/tools/{dca_create_plan,dca_update_plan,pnl_detail}.go` | webull in enums/docs; `isStockProvider` helper; quote-guard applies to webull, whole-share guard verified settrade-only (Webull allows fractional). build/vet/test green. |
| **W2 — Skills docs** | sonnet | ✅ approved | `workspace/skills/{portfolios,market-data,pnl,dca,trading,technical-analysis}/SKILL.md` | Webull (US equities, USD, cash/stock); trading marked read-only. Integrator fix: corrected portfolios stock-wallet field names to match adapter (`current_price`/`percent_pnl`, not `market_price`/`percent_profit`). |
| **W3 — Web UI config** | sonnet (2 legs) + opus fix | ✅ approved | `web/frontend/.../portfolio-config-page.tsx`, `app-sidebar.tsx`, i18n `en.json`/`th.json` | Account form (App Key/Secret/Account ID/Region, no PIN), nav entry, 10 `portfolios.webull.*` keys in en+th (reconciled, no orphans), load from `exchanges.webull`. **Integrator caught+fixed a real gap:** save handler had no `webull` branch (`serializeWebullAccount` defined but never called → saves silently dropped); added the branch. `tsc --noEmit` exit 0, `go build ./web/...` ok. form-model.ts: no per-exchange enum, no change. Backend: pass-through, no change. |

## Final verification (T6)
- `gofmt -l` on all changed files → clean (fixed pre-existing import order in `cmd/.../gateway/helpers.go`).
- `go build ./...` → ok. `go vet ./...` → clean. `go test ./...` → 0 failures.
- Registration smoke (temporary test, run + removed): `broker.ListConfiguredAccounts` lists `webull/main`; `broker.CreateProviderForAccount("webull","main")` resolves (init ran via blank import); provider `ID=webull`, `Category=stock`, implements PortfolioProvider + MarketDataProvider, does NOT implement TradingProvider (deferred as designed).
- `khunquant` binary builds and runs.

## Wave 3 — SDK-ground-truth rebuild + full trading (2026-07-10)

Trigger: user obtained the Webull developer docs + a public **sandbox with shared test creds**, and set the goal to **integrate full feature using the SDK/docs as ground truth**. Decision (advisor-confirmed): keep the native Go adapter; **do NOT** adopt Cloud MCP as the runtime (read-only, OAuth-interactive, remote, wrong DTO shape, doesn't generalize to Dime) — MCP is at most an optional future connector.

### 🔬 Ground-truth verification — DONE, EMPIRICALLY PROVEN
Ran a stdlib Go probe (`scratchpad/webull_probe.go`) against the live sandbox with shared cred #1:
- `POST /openapi/auth/token/create` → **HTTP 200** (token, status PENDING but usable)
- `GET /openapi/account/list` → **HTTP 200** (2 real accounts: cash + margin)
- `GET /openapi/assets/balance` → **HTTP 200** (real balance JSON)

**Corrections this forced (vs the T1/T2 code we shipped):**
| # | Was (wrong) | Is (verified) |
|---|---|---|
| 1 | Signer percent-encodes each key/value component, skips whole-string encode | **str1 PLAIN, then URL-encode the ENTIRE str3** (Go `url.QueryEscape` on the whole thing). Our T2 "fix" was a spec regression. |
| 2 | `endpoints.go` guessed paths (`/v2/trading/...`) | Real base `/openapi/...`: `/openapi/account/list`, `/openapi/assets/balance`, `/openapi/assets/positions`, `/openapi/trade/order/place`, `/openapi/auth/token/create` |
| 3 | No token flow; assumed signing alone | **`x-access-token` REQUIRED on all trade-api endpoints** incl. balance/positions. Must create+cache+refresh token. |
| 4 | Single flat `defaultHost = api.webull.com` | Env axis: sandbox=`us-openapi-alb.uat.webullbroker.com` (NOT `api.sandbox.webull.com`), prod=`api.webull.com`. Needs Environment(uat/prod) config. |
| 5 | Nonce 16 alphanum | 32 hex (uuid4().hex-style) |

Full DTO spec being harvested to `docs/webull-api-spec.md` (research agent).

### Wave 3 task board
| Task | Owner | Status | Notes |
|---|---|---|---|
| #10 Harvest full API spec → `docs/webull-api-spec.md` | sonnet | ✅ done | 898-line spec; all 15 endpoint families + timeframe M* enums. **All verified HTTP 200 vs sandbox** (incl. market data + order preview). |
| #11 Core webull package rewrite (signer/token/client/config/DTOs/adapter) | sonnet | ✅ approved (1 review cycle) | All gates green; **both signer KAT literals match my independent vectors** (non-circular). Review caught + fixed 1 functional gap: `invalidateToken` was dead code → token-retry-on-401 not wired; now retry-once loop (fresh sig+body per attempt, TestClient401Retry passes). Host==signing-host confirmed. Remaining lint = cosmetic modernization nits (any/rangeint/QF1003), outside `make check`. |
| #12 Config env axis + correct DTOs + adapter mapping | sonnet | todo | Environment uat/prod; real balance/position DTOs |
| #13 TradingProvider (equities: place/cancel/query) | sonnet | ✅ approved | Built on settrade template; Id=client_order_id, CORE session, fractional→MARKET-only, TIF DAY/GTC, take_profit rejected. 63 pkg tests pass; matches live order-lifecycle oracle. Review: independent full-gate green. |
| #14 Sandbox live verification of the **real Go client** | opus | ✅ done | `pkg/exchanges/webull/sandbox_live_test.go` (`//go:build webullsandbox`, env-gated, out of `make check`). Ran vs UAT: wallets(all)+FetchPrice+FetchTicker+FetchOHLCV(oldest-first)+CreateOrder→FetchOpenOrders→CancelOrder all pass. |
| #15 Final wave — skills/docs + web UI env selector | opus | ✅ done | Flipped trading/dca/portfolios skills to "trading supported (equities)". Added uat/prod Environment selector to web UI webull form + i18n en/th (parity verified). tsc clean. Onboarding/TUI env left defaulting to prod (minor deferral). |

## Wave 3 — COMPLETE ✅ (full feature, SDK-verified)
Webull is now a **full-feature** provider: portfolio + market data + **equity trading**, every layer verified against the live sandbox with the real compiled Go client. Signer pinned to non-circular KAT literals. Token flow (create/cache/refresh/retry-on-401) implemented. All guessed paths/DTOs replaced with `/openapi/...` string-money DTOs. `make check`-equivalent gate (gofmt/build/vet/test) green across webull/tools/config/broker; frontend tsc clean.
Deferred (explicit): options/futures/crypto/event/combo/algo trading; MQTT/gRPC streaming; US market-holiday calendar in GetMarketStatus (Webull rejects closed-market orders server-side as backstop); onboarding/TUI uat/prod prompt (defaults prod); DRY registry refactor for Dime. Cosmetic lint nits (any/rangeint/strings.Cut/QF1003) outside `make check`.

### P2 (#13) prep — TradingProvider mapping (interface is the spec, not the 40-field order body)
`broker.TradingProvider` methods → Webull:
- `CreateOrder(symbol, orderType, side, amount, price, params)` → POST `/openapi/trade/order/place`. Build `new_orders[0]`: combo_type=NORMAL, instrument_type=EQUITY, market=US, entrust_type=QTY, **support_trading_session=CORE (REQUIRED — omitting = 417)**, time_in_force=DAY default. orderType→order_type (limit→LIMIT, market→MARKET), side buy/sell→BUY/SELL, amount→quantity, price→limit_price. Generate a unique `client_order_id` (≤32 chars). **Fractional (amount not whole) → force MARKET** (Webull rejects fractional LIMIT). stock TIF ∈ {DAY,GTC} only.
- **ccxt.Order.Id = client_order_id** (Webull cancel/detail key on client_order_id, NOT order_id); stash order_id in Info.
- `CancelOrder(id,symbol)` → POST `/openapi/trade/order/cancel` {account_id, client_order_id=id}.
- `FetchOrder(id)` → GET `/openapi/trade/order/detail` (account_id, client_order_id=id).
- `FetchOpenOrders` → GET `/openapi/trade/order/open`; flatten combo→orders.
- `FetchClosedOrders` → GET `/openapi/trade/order/history` (account_id, dates); filter FILLED/CANCELLED.
- `FetchMyTrades` → no trades endpoint in spec; return clear "not supported via OpenAPI" (equities v1).
- **Scope P2 to EQUITIES only.** Defer options/futures/crypto/event/combo(OTO/OCO)/algo(TWAP/VWAP/POV).
- **Wiring: NONE needed.** Verified `order_create.go`/`order_cancel.go`/etc. are fully capability-driven (dispatch via `broker.CreateProviderForAccount` + type-assert `broker.TradingProvider`, no per-exchange enum), and `permissions.go` already resolves webull (T3). So P2 = pure adapter TradingProvider impl inside `pkg/exchanges/webull` (coupled to P1; runs sequentially after). Webull fractional → whole-share guard stays settrade-only (already true from W1). Balance preflight in order_create Gate 4 splits symbol on "/" → skipped for bare "AAPL" (fine).

## Outstanding (superseded items kept for history)
1. ~~Verify endpoint paths + host~~ → **DONE** (Wave 3 probe).
2. ~~Verify signing canonicalization~~ → **DONE** (Wave 3 probe, HTTP 200).
3. `TradingProvider` → now in scope (#13).
4. Optional lint nits (`interface{}`→`any`, `strings.Cut`, QF1012 Fprintf) — cosmetic, outside `make check`.

## Review log
- **T1 approved** — faithful Settrade mirror (`WebullExchangeAccount`/`Config`, `ResolveAccount`, `webullSecEntry` + Marshal/Unmarshal, no PIN). Scope contained. Independently ran `gofmt -l` clean, `go build` ok, `go test ./pkg/config/` → ok. Env note: run go via `env -u GOROOT` (goenv GOROOT 1.24.1 vs go.mod 1.25.11).
- **T2 approved after 1 review cycle** — manual review caught two defects the passing tests missed: (1) `randomNonce()` set all 16 bytes from `time.Now().UnixNano()` in a tight loop → near-zero entropy; fixed to `crypto/rand`. (2) canonical-string `host` hard-coded → wrong signature for any non-default host; fixed to thread the real host from the client. Re-verified: fmt clean, `go build ./...` ok, `ok webull` (17 tests). Minor lint nits (`interface{}`→`any`, `strings.Cut`, one redundant nil-check) deferred to T6; match existing style, don't affect `make check`. NOTE: Webull endpoint paths + host are best-guess `// TODO: verify against developer.webull.com` — confirm when live keys arrive.
