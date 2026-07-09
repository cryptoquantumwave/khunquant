# Webull US-Stock Integration — Progress Tracker

Head of Engineering: Opus 4.8 (integrator/reviewer). Implementation fanned out to Sonnet 5 subagents.
Scope: **read-only + market data** (Portfolio + MarketData providers; no trading yet). Minimal wiring.
Plan: `~/.claude/plans/let-s-design-the-technical-reflective-wozniak.md`.

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

## Outstanding (before live trading / production)
1. **Verify Webull endpoint paths + US host** in `pkg/exchanges/webull/endpoints.go` against developer.webull.com (currently best-guess, marked TODO) — needs live API access.
2. **Verify the signing canonicalization** (URL-encoding, host value) against a real Webull response once keys exist; httptest proves shape, not server acceptance.
3. `TradingProvider` (order placement) — deferred; add once keys verify order/fees.
4. Optional lint nits (`interface{}`→`any`, `strings.Cut`, QF1012 Fprintf) — cosmetic, outside `make check`.

## Review log
- **T1 approved** — faithful Settrade mirror (`WebullExchangeAccount`/`Config`, `ResolveAccount`, `webullSecEntry` + Marshal/Unmarshal, no PIN). Scope contained. Independently ran `gofmt -l` clean, `go build` ok, `go test ./pkg/config/` → ok. Env note: run go via `env -u GOROOT` (goenv GOROOT 1.24.1 vs go.mod 1.25.11).
- **T2 approved after 1 review cycle** — manual review caught two defects the passing tests missed: (1) `randomNonce()` set all 16 bytes from `time.Now().UnixNano()` in a tight loop → near-zero entropy; fixed to `crypto/rand`. (2) canonical-string `host` hard-coded → wrong signature for any non-default host; fixed to thread the real host from the client. Re-verified: fmt clean, `go build ./...` ok, `ok webull` (17 tests). Minor lint nits (`interface{}`→`any`, `strings.Cut`, one redundant nil-check) deferred to T6; match existing style, don't affect `make check`. NOTE: Webull endpoint paths + host are best-guess `// TODO: verify against developer.webull.com` — confirm when live keys arrive.
