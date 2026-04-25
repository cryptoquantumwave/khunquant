# Upstream Sync Log

khunquant is a fork of [picoclaw](https://github.com/sipeed/picoclaw).

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
