# Upstream Sync Log

khunquant is a fork of [picoclaw](https://github.com/sipeed/picoclaw).

## Sync Status Overview

Our fork point is `96fd4e05` (2026-03-15), before upstream added OS-level process isolation (`pkg/isolation`, commit `51eecde0`, 2026-04-08).

### Upstream Coverage (commit-subject overlap measured against our `main`)

The following table shows how many upstream non-merge commits in each range have equivalent subjects in our repository:

| upstream range | non-merge commits | present in our `main` | coverage |
|---|---|---|---|
| merge-base `96fd4e05` .. v0.2.6 | 457 | 130 | 28% |
| v0.2.6 .. v0.2.7 | 164 | 38 | 23% |
| v0.2.7 .. v0.2.8 | 86 | 6 | 6% |
| v0.2.8 .. v0.2.9 | 172 | 4 | 2% |
| v0.2.9 .. v0.3.0 | 137 | 4 | 2% |
| v0.3.0 .. v0.3.1 | 52 | 0 | 0% |

Roughly **453 of 621** non-merge commits below v0.2.7 never landed, and that range remains **unexamined** for selective cherry-pick opportunities.

### Sync Waves

#### Wave 1: Foundation & Hardening (PRs #49-#52)

These four PRs landed critical fixes and features from upstream's post-fork development (v0.2.7 onwards):

| PR | Commit | Subject | Notes |
|---|---|---|---|
| #49 | `a822230b` | fix(deps): bump Go to 1.25.13 for 6 stdlib CVEs | Address multiple stdlib security issues |
| #50 | `4e3ea97d` | fix(tools): harden exec guard against PowerShell encoding bypass | Shell-guard improvements: encoding bypass, scheme-less URLs, workspace-relative paths |
| #51 | `ce4ca2af` | fix(channels,seahorse,web): port upstream correctness fixes from picoclaw v0.2.8-v0.3.1 | Correctness fixes across channels, seahorse, web, and build |
| #52 | `34f03c34` | feat(voice): multi-backend voice transcription | Whisper, ElevenLabs, audio-model backends |

#### Wave 2: Access Control & Infrastructure (PRs #53-#56)

These four PRs add authorization and connectivity features:

| PR | Commit | Subject | Notes |
|---|---|---|---|
| #53 | `929e3ac3` | feat(agent,config): restrict which MCP servers an agent may load | Per-agent MCP server allowlist |
| #54 | `520dc287` | fix(tools): scope remote cron command access | Remote cron command authorization |
| #55 | `facff706` | fix(web): harden launcher IP allowlist and trusted-proxy handling | IP allowlist and trusted-proxy hardening |
| #56 | `3175061a` | feat(web,api): model catalog, provider model fetch, and connectivity test | Model catalog and provider connectivity API |

### Tag Namespace Hazard

**Our tags collide by name with upstream tags.** Specifically, our own tags `v0.3.0-rc.1`, `v0.3.1-rc.1`, `v0.3.2-rc.1`, and `v0.3.3-rc.*` (April 2026) share the same names as upstream's release versions `v0.3.0`, `v0.3.1`, `v0.3.2`, and `v0.3.3`.

When comparing commits against upstream, **always use 8-character SHAs instead of tag names** to avoid ambiguity. Tag-based comparisons will pull the wrong commit.

## v0.2.6 → v0.2.7 (synced 2026-04-25)

Base: picoclaw tag `v0.2.6`  
Synced to: picoclaw tag `v0.2.7` (selective cherry-pick)

### Strategy

Selective cherry-pick — 21 functional commits applied individually. The two
massive structural refactors (loop.go split + provider/tool reorganization,
commits `12d5421c`, `329e68e0`, `ee634dc8`, `4c133dc2`) were intentionally
skipped to preserve khunquant's monolithic `pkg/agent/loop.go` structure.

### Commits Applied (chronological)

| Upstream hash | Subject |
|---------------|---------|
| `7bd11181` | fix(agent): preserve reused tool call IDs across turns |
| `e22b4e1e` | feat(agent): support btw side questions |
| `f5e779e2` | refactor: make agent loop support parallel |
| `c3f40008` | feat(network): implement network error classification and fallback handling |
| `7aa2d672` | fix(network): classify timeout errors as FailoverTimeout |
| `ab019d3f` | feat(auth): add no-browser option for OAuth login |
| `ffd30d7d` | fix(auth): improve no-browser OAuth login |
| `9c3dc0ee` | fix(auth): canonicalize Google Antigravity provider and enhance credentials |
| `7fdc9c7b` | fix(web): support proxies in SearXNG and web fetch |
| `f32b303d` | fix(web): avoid resetting web search draft on config refetch |
| `a8d0b035` | fix(web): save channel configs with nested channel_list patches |
| `2784223a` | Make web search auto-switch with UI language |
| `743cd360` | fix(tools): centralize shared LLM note constants |
| `2708c834` | build(deps): patch gomarkdown and upgrade shadcn |
| `8461c996` | chore(web): update linting and router dependencies |
| `2b844778` | refactor(tests): extract common logic for fallback error handling |
| `5a2e7795` | refactor(web): improve theme style element management |
| `dcb4b67e` | fix(web): clean up restored chat transcripts and optimize chat UI |
| `ba699223` | feat(web): support list editing for channel array fields |
| `6ca73112` | feat(agent): add context usage ring indicator and /context command |
| `e77c4eba` | build(deps): bump maunium.net/go/mautrix 0.26.4→0.27.0 |

### Commits Skipped

**Structural refactors (intentionally deferred):**

| Upstream hash | Subject | Reason |
|---------------|---------|--------|
| `12d5421c` | refactor(agent): split loop.go into 12 focused sub-packages | Preserving monolithic loop.go |
| `329e68e0` | refactor(agent): rename loop_*.go → agent_*.go, add pipeline.go split | Same |
| `ee634dc8` | refactor(providers): reorganize provider packages and facades | Large structural change |
| `4c133dc2` | refactor(tools): reorganize tool packages and facades | Large structural change |
| `9b4efddd` | fix(providers,tools): address linter issues after reorg | Depends on above |

**picoclaw launcher / pico-specific:**

| Upstream hash | Subject |
|---------------|---------|
| `4b76196e` | refactor(web): secure Pico websocket access behind launcher auth |
| `d002e151` | fix(web): improve Pico URL and origin handling behind proxies |
| `f8190f04` | fix(web): stop pinning Pico WebSocket origins during setup |
| `71c877a6` | refactor(web): switch dashboard auth from tokens to passwords |
| `2bf842e4` | feat(gateway): add service log level controls |

**Other skipped (docs, reverted, feishu-only, minor deps):**

`a5379d5f`, `f1b659e5`, `6421f146`, `e556a816`, `9fe67824`, `b798fa4b`,
`b0d3f19a`, `4e1ceee6`, `de3d042d`, `610f68ad`, `16d174e1`, `4e2f80b7`,
`72f30c58`, `235cb11b`, `74856d37`, `c36a48cf`, `d73897da`, `9c97442f`,
`63754401`, `7f56ca8c`

### Post-sync Adaptations

- `pkg/agent/context_usage.go`: fixed module import path, added `tokenizer` package calls
- `pkg/agent/context.go`: added `buildActiveSkillsContext` (method from skipped structural refactor)
- `pkg/auth/store_test.go`: fixed picoclaw module imports → khunquant, `.picoclaw` → `.khunquant`
- `cmd/khunquant/internal/auth/status_test.go`: same import/path fixes
- `pkg/config/config.go`: added null JSON handling in `FlexibleStringSlice.UnmarshalJSON`
- `pkg/providers/fallback_test.go`: `NewFallbackChain` arg count (2→1)
- `pkg/tools/result_test.go`: unexported → exported LLM note constants
- `pkg/tools/web_test.go`: `NewWebFetchToolWithProxy` arg count (5→3)
- `web/backend/api/config_test.go`: removed gateway log-level tests (require unapplied `2bf842e4`)
- `web/backend/api/config.go`: added regex validation + `test-command-patterns` endpoint (from `ba699223`)
- `pkg/skills/installer_test.go`: fixed `wantOwner` for `cryptoquantumwave` org URLs
