# Upstream Sync Tracking — picoclaw v0.2.9 → khunquant

**What this is:** the curated list of upstream commits worth porting from
[sipeed/picoclaw](https://github.com/sipeed/picoclaw) tag `v0.2.9` into our fork,
with **what each does, the benefit, conflict risk, and how to port it**.
Companion progress log: [`upstream-sync-v0.2.9-progress.md`](./upstream-sync-v0.2.9-progress.md).

## Context

KhunQuant is a hard fork of picoclaw with crypto/trading additions
(`pkg/exchanges`, `pkg/deltaneutral`, `pkg/dca`, `pkg/seahorse`, `cmd/membench`)
and a `picoclaw → khunquant` rename. We diverged at merge-base **`96fd4e05`**
(2026-03-15). `v0.2.9` (`2992eccb`, 2026-05-22) is **1107 commits ahead**.

## Methodology

Conflict-risk is the discriminating axis — our fork churned the same subsystems
upstream did. Triage:

1. `git log HEAD..v0.2.9` → 1107 commits (228 merges, 879 non-merge).
2. Fork-modified file set: `git diff --name-only 96fd4e05..HEAD` → 922 files.
3. Scored every commit by **file overlap** with the fork set: 0 (clean port),
   1 (minor merge), ≥2 (conflict-prone, deprioritized).
4. Kept `feat|fix|perf|security|refactor` with overlap ≤1; dropped lint/gci/
   test-format churn, dead-code removals, and (per Dependabot) all `build(deps)`.
   → **127 curated commits**, reviewed by 10 Sonnet subagents (one per theme),
   then lead-reviewed (corrections noted inline and in the progress log).

> Tooling note: the RTK hook mangles `git log` hash output — run git analysis
> inside `bash -c '...'` or `rtk proxy git ...`.

> **Coverage boundary:** the **545 commits with ≥2-file overlap** against our fork
> were deprioritized and **not** individually reviewed — they are conflict-prone
> (web/agent rewrites) but may still contain valuable fixes. This document covers
> the 127 low-overlap candidates only; a future pass could mine the high-overlap
> set for security/correctness fixes worth a manual port.

## Disposition legend

- **TAKE** — `git cherry-pick -x <sha>` should apply cleanly (only import-path fixups).
- **RESOLVE** — cherry-pick, resolve the named conflicting file by hand.
- **MANUAL** — port the change by hand (our layout/paths differ; no clean pick).
- **DEFER** — valuable but blocked on a subsystem we don't have yet (tracked below).
- **SKIP** — not applicable / low value / already moot.
- **DONE** — already present in our fork (verified).

---

## 1. Security / secret-masking — **all already in fork (DONE)**

Lead-verified: `pkg/channels/base.go:117` (open-by-default warning + `*`),
`pkg/tools/shell.go:38` (disk-wipe deny regex), `pkg/logger/logger_3rd_party.go`
(`maskSecrets`), web/frontend masking all present. **No action.**

| sha | subject | status |
|-----|---------|--------|
| 8d5fc736d | security: open-by-default warning + `*` allow_from | DONE |
| ae021ef84 | fix: more accurate disk-wipe deny pattern | DONE |
| f1ac1a107 | fix(web): ≥40% api-key masking | DONE |
| ce1619051 | fix(chat): avoid full secret exposure for 7-char secrets | DONE |
| 64ceb5ab7 | fix(logger): show first/last 4 of bot token | DONE |
| 8fc36a4f9 | fix(logger): mask bot tokens in 3rd-party logger | DONE |

## 2. MCP

| sha | subject | what / benefit | risk | disposition |
|-----|---------|----------------|------|-------------|
| e70928cc6 | support DisableStandaloneSSE for HTTP transport | disables standalone SSE GET for `http` (keeps for `sse`); avoids breakage on servers w/o GET /mcp. **High** | Low — clean insert in `pkg/mcp/manager.go` connectServer() | **MANUAL** |
| 1ff8a418f | sanitize MCP tool schemas for Gemini | new shared `google_schema.go` w/ $ref deref + anyOf/oneOf flatten + depth limit. **High** if Gemini added | Med | **DEFER** (pairs with Gemini provider, not in fork) |
| 5a13616b5 | surface MCP init failures to handlers | — | None | **DONE** (present at `pkg/agent/loop_mcp.go`) |
| ffc8bdba3 | normalize streamable-http before config validation | needs cmd mcp subcommand | High | **SKIP** (no cmd mcp; EnvFile already supported) |
| 07032df03 | normalize local cmd paths + env-file docs | needs cmd mcp subcommand | High | **SKIP** (feature already in config) |

## 3. Agent core

| sha | subject | what / benefit | risk | disposition |
|-----|---------|----------------|------|-------------|
| 1a44752dc | prevent double-counting system msg tokens in estimator | treat SystemParts as alt (not additive) to Content → prevents premature pruning/summarization. **High** | Low — `pkg/agent/context_budget.go` | **RESOLVE** |
| 1245f2ddf | recover after image-input-unsupported failures | retry sending images as files when model rejects inline. **High** | Low — `loop.go` retry path | **MANUAL** |
| 06fad9571 | network error retry w/ configurable max/backoff | `isNetworkError()` + backoff + `max_llm_retries` config. **High** | Med — `loop.go` + `pkg/config/config.go` | **RESOLVE** |
| 9c65d78b0 | forceCompression must not assume history[0] is system prompt | compression correctness. **High** | Med — verify our compression impl | **MANUAL** (verify first) |
| 836220363 | transcribe queued voice follow-ups | extract `prepareInboundMessageForAgent`. Med | Low | **MANUAL** → loop.go |
| d601b7526 | send SVG attachments as files | Med | Low | **MANUAL** → loop.go media |
| 610e9e3fe | dismiss session tool feedback on skipped outbound | Low | None | **MANUAL** → loop.go outbound |
| 057683d94 | use runtime event kind for LLM retry | one-liner constant | None | **DEFER** (runtime-events) |
| 50cc7100c | event logs show event kind | logging | Med | **DEFER** (runtime-events) |
| af61d0bca / cf68c91ec / 9ca73b94b | event-bus / hook-manager foundation; prompt hook+cache | new subsystems | High | **DEFER** (see Deferred) |
| 2ae25b103, 96fd887ca, 27bd816b1, 765a16547, 409251e69, abeb2d8e0, 148583e7b | AGENT.md tool allowlist / discovery hardening | — | High | **SKIP** (allowlist+discovery subsystem removed in fork) |
| 12d5421c2 | split loop.go into sub-packages | refactor | High | **SKIP** (incompatible with our loop layout) |
| 38baf1ccd | normalize nil args / FormatArgsJSON (formatting) | Low | Low | SKIP (style) |

## 4. Multi-agent / delegate / stop

Delegate-tool chain is portable; **stop-command chain is blocked** on scoped
steering infra our fork lacks (single unscoped queue).

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 484ef399f | delegate tool (new `pkg/tools/delegate.go`) | **TAKE** | port first |
| 039f3556e + 6ee66123f | wire + simplify delegate registration | **RESOLVE** (`loop.go`) | apply as one unit, after 484ef399 |
| df486b993 | normalize agent_id before delegation | **TAKE** | fix import path picoclaw→khunquant |
| a0245c7b0, d63430ab3, a7e52e8a2 | stop command + fixes | **DEFER** | needs scoped steering (`clearSteeringMessagesForScope`, `pendingStops`) — not in fork |
| 01c2f8d6c, abeb2d8e0, 148583e7b | subturn cleanup / discovery | **SKIP** | moot in fork |

## 5. Providers

⚠️ **Lead correction:** gemini, bedrock, deepseek providers **do not exist** in
our fork — their fix commits are **not** cherry-pickable (subagent's "clean pick"
was wrong). Defer until/unless we import those providers wholesale.

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 54654d279 | anthropic: skip tool calls w/ empty names (prevents API error). **High** | **RESOLVE** | `pkg/providers/anthropic/provider.go` ~L180 |
| 8d97896a0 | handle nil input in GLM tool_use blocks. **High** | **RESOLVE** | `pkg/providers/anthropic_messages/provider.go` ~L221 |
| 56fb0dc4e | claude_cli: surface stdout on non-zero exit | **TAKE** | file matches fork |
| c7544f7cb | providers: `extra_body` config injection | **RESOLVE** | `pkg/config/config*.go` (verify vs fork config tests) |
| 6fbd7e0a3, cbae69ad6, 83e93ca57 | gemini fixes | **DEFER** | Gemini provider absent |
| ad5232ade, 4f90909af | bedrock streaming + SSO error | **DEFER** | Bedrock provider absent |
| 1722cfc28 | deepseek vision-unsupported detection | **DEFER/SKIP** | touches `pkg/agent/llm_media.go`; verify relevance |

## 6. Channels

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 412705783 | pico: preserve image media across attachments (new `pico/client.go`) | **TAKE** | new file |
| 34b9d5d6f | telegram: preserve raw OAuth links in HTML | **TAKE** | |
| 6801cc7ab | telegram: wrap long voice media (style) | TAKE | trivial |
| bacb9aba7, ad78ba06e | line: close response bodies | **TAKE** | resource leak fix |
| 6d7d1b090 | line: capture QuoteToken + location | **TAKE** | |
| 3e9b7ce9c | feishu: invalidate cached token on auth error (fixes 2h lockout). **High** | **RESOLVE** | new `token_cache.go` + `feishu_64.go` |
| 8b3e50269 | feishu: enrich reply context for card/file (new `feishu_reply.go`) | **RESOLVE/MANUAL** | High value, larger |
| 43095543a | feishu: skip empty reaction-emoji entries | **RESOLVE** | |
| b6951b692 | dingtalk: honor mention-only groups, strip mentions | **RESOLVE** | |
| 11dec0c80 | weixin: persist context tokens across restarts | **MANUAL** | investigate fork weixin layout |
| 5db008f38 | channels: dismiss tool-feedback animation on ResponseHandled (goroutine leak). **High** | **RESOLVE** | `pkg/channels/manager.go` |
| 1b9e7e32b, 9d4228267, d6b38c423, b73caebe6, b5e29ae50 | chat/web fixes (CRLF, dark mode, wrapping) | TAKE | frontend, low risk |
| 3b498d2e4, c3631d84b | wecom: streaming + media | **DEFER/SKIP** | fork wecom arch differs (aibot/app/bot split) |

## 7. Cron / gateway

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 36b9693d3 | cron: independent session per execution. **High** (our trading cron) | **TAKE** | `pkg/tools/cron.go:383` — key `cron-%s` → add timestamp; verified NOT yet applied |
| 61a899cfb | cron: test uses OutboundChan | **TAKE** | bus API already migrated |
| 230942d23 | loop: idleTicker polling | **DONE** | already in `loop.go` |
| e613258fa, 49141877f | gateway lifecycle events / startup error logging | **DEFER** | needs `pkg/gateway/gateway.go` (absent) |

## 8. Tools / infra

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 0f5207676 | cross-platform serial hardware tool. **High** (Pi/embedded mission) | **TAKE** | new, 9 files |
| 4a81f0e74 | edit_file unified-diff preview. **High** UX | **RESOLVE** | `pkg/tools/edit.go` (flat layout, signature differs) |
| e1863234f | launcher: hide Windows console flashes | **TAKE** | new platform files |
| 51f8285f9 | build: disable Matrix gateway on freebsd/arm | **DONE** | already present |
| 60d7ec20a | log prompt/completion/total tokens | **RESOLVE** | merge into existing usage log in loop.go |
| b4a596560 | harden copydir repo-root detection | **TAKE** | |
| e760cb737 | auth: wecom CLI QR login | **RESOLVE** | rename cmd paths picoclaw→khunquant |
| 743cd3602 | centralize shared LLM note constants | RESOLVE | minor |
| 3e33d1053, 174fbba14 | darwin/freebsd no-cgo tray fallback | **MANUAL/SKIP** | audit if fork uses systray |
| 5e44a9941 | docker: run self-built as root | **MANUAL** | review security posture first |
| ffb824372, 4edbc73b6, 7a8d7fb21 | integration docker-runner fixes | TAKE/SKIP | minimal integration suite in fork |

## 9. Web / config

| sha | subject | disposition | note |
|-----|---------|-------------|------|
| 6a8552a66 | derive WebSocket URL from browser location (reverse-proxy fix). **High** | **TAKE** | `features/chat/controller.ts` |
| 0bb9bedc4 | dual-stack (IPv4/IPv6) netbind correctness. **High** | **TAKE** | `pkg/netbind` + main.go |
| 604187e31 | model test-connection w/ real probing. **High** | **RESOLVE** | `web/backend/api/models.go` (SecureString differs) |
| f32b303d2 | preserve web-search draft on config refetch | **TAKE** | |
| 79f87d151, 24382271d | localhost/advertise-IP console logic | **TAKE** | |
| bd56e10b6 | logs panel scroll handling | **RESOLVE** | verify logs-panel.tsx |
| 93f4c4a84, 4d3070e84 | dark-mode skills page; HTTP copy fallback | TAKE | cosmetic |
| f53222f6a | POST /api/config/reset endpoint | **RESOLVE** | lower priority |
| 5a2e7795c | highlight-theme hook lifecycle | MANUAL | verify hook exists in fork |
| cd48c3bd5 | config: remove stale wecom security merge fields | TAKE | |
| b954e6b8d, b2249df3e, d9717b56d, eedebabbe | events refactor sequence | **DEFER** | see Deferred |

## 0. Misc / TUI

| sha | subject | disposition |
|-----|---------|-------------|
| 8c44597c3, 02da11719, 7b4d5d451, 545b7afe4, 119cc2e8e, 955d6e70f | TUI pages (chat/gateway/channels/model-sync) + refactor | **TAKE** (cmd/khunquant-launcher-tui) |
| ed47d5f7c | auto-run onboard when config dir missing | **MANUAL** → launcher main |
| 0fe058254 | Android multi-DNS fallback resolver | **MANUAL** → create `cmd/khunquant/dns_noresolv.go` |
| 520391643 | agent-browser skill + Dockerfile.heavy | **TAKE** |
| b8819bdbf | seahorse: drop/recreate FTS5 triggers | **RESOLVE/INVESTIGATE** (verify our seahorse schema has triggers) |
| e9f55d776 | gemini reasoning async + SSE parse | **DEFER** (gemini) |
| cbe6a0907 | tool/model restart feedback (web) | **RESOLVE** (models-page.tsx) |
| cbd0798a5 | avoid duplicate `v` in CLI banner | **MANUAL** → cmd/khunquant |
| e4f4afcd4 | goreleaser: ignore nightly tags | **RESOLVE** (.goreleaser diverged) |
| 48cba906c | restore assets + Baidu LimitReader | TAKE (assets) + MANUAL |
| 6dd30a0c7, 703f630f3, a9720daa4, 1984bb5bb, 2b844778f, 2a28198d0 | ci / test housekeeping | TAKE (low value) |
| d5c8bfffb | docs: Baidu quota | **SKIP** (docs; fork README separate) |

---

## Recommended execution order (waves)

Branch: `sync/upstream-v0.2.9`. One themed commit/PR per wave; `make check` after each.

1. **Wave 1 — zero-risk wins (TAKE):** serial tool, telegram OAuth, line fixes,
   pico media, WS-URL, dual-stack netbind, cron independent session, web-search
   draft, TUI pages, claude_cli stderr, agent-browser skill.
2. **Wave 2 — high-value RESOLVE:** token-estimator fix, network retry, anthropic
   empty-name, GLM nil-input, feishu token-cache, channel tool-feedback leak,
   edit_file diff, model test-connection, extra_body config.
3. **Wave 3 — MANUAL ports:** image-input recovery, voice follow-ups, SVG, weixin
   token persistence, DNS fallback, onboard auto-run, CLI banner, wecom QR auth.
4. **Wave 4 — delegate tool chain:** 484ef399 → 039f3556+6ee66123 → df486b993.

## Deferred (separate phases — blocked on absent subsystems)

- **Runtime event bus + hooks** (`eedebabbe`, `af61d0bca`, `b2249df3e`,
  `b954e6b8d`, `d9717b563`, `e613258fa`, `50cc7100c`, `057683d94`, `cf68c91ec`,
  `9ca73b94b`): per user decision, deferred — architectural, interdependent,
  high conflict with our agent loop.
- **Stop command** (`a0245c7b0`, `d63430ab3`, `a7e52e8a2`): needs scoped steering
  queue infra not in fork.
- **Gemini / Bedrock / DeepSeek providers** + their fixes: providers absent;
  importing them is a standalone feature decision, not a fix port.
- **Gateway lifecycle** (`e613258fa`, `49141877f`): needs `pkg/gateway/gateway.go`.
- **WeCom streaming/media** (`3b498d2e4`, `c3631d84b`): fork wecom architecture
  (aibot/app/bot split) differs.

## Explicitly skipped

- All `build(deps)` (Dependabot).
- AGENT.md tool-allowlist / discovery hardening (subsystem removed in fork).
- loop.go sub-package split (incompatible layout).
- Lint/gci/struct-align/test-format churn, dead-code removals.
- Docs-only changes.

## Verification (per wave & at end)

- `make check` (deps + fmt + vet + test) and `make build` must pass each wave.
- Targeted: `go test ./pkg/agent/... ./pkg/providers/... ./pkg/channels/... ./pkg/tools/...`.
- Fork-exclusive regression guard: `go test ./pkg/exchanges/... ./pkg/deltaneutral/... ./pkg/dca/... ./pkg/seahorse/...`.
- Cross-compile sanity: `make build-pi-zero`.
- Each row's port method is re-validated against the actual cherry-pick outcome
  and logged in the progress file.
