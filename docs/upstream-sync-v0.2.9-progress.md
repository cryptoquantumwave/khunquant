# Upstream Sync v0.2.9 — Progress Tracker

Lead: Opus 4.8 (orchestrator + reviewer). Workers: Sonnet 4.6 subagents, one per
theme batch, read-only `git show` analysis of curated commits.

- **Range:** `HEAD..v0.2.9` (merge-base `96fd4e05`, 2026-03-15 → `v0.2.9` 2026-05-22)
- **Curated set:** 127 commits (low file-overlap with our fork; lint/test-format/
  dead-code/build-deps excluded). See methodology in `upstream-sync-v0.2.9.md`.
- **Batch lists:** `/tmp/sync_batches/*.txt`

## Batch status

| # | Theme | Commits | Subagent | Status | Reviewed |
|---|-------|---------|----------|--------|----------|
| 1 | MCP | 5 | a45ee70e | returned | ☑ |
| 2 | Agent core | 19 | aef9c9fa | returned | ☑ |
| 3 | Multi-agent / delegate / stop | 10 | aa6311ed | returned | ☑ |
| 4 | Providers | 10 | a68fff8b | returned | ☑ (corrected) |
| 5 | Channels | 19 | a293424c | returned | ☑ |
| 6 | Security / secret-masking | 6 | ac0dd74c | returned | ☑ (verified) |
| 7 | Cron / gateway | 5 | aa43400f | returned | ☑ (verified) |
| 8 | Tools / infra | 14 | aebf8012 | returned | ☑ |
| 9 | Web / config | 17 | ad20fc3f | returned | ☑ |
| 0 | Misc / no-scope | 22 | aaefa6d8 | returned | ☑ |

Status: pending → running → returned → reviewed. (Continue a subagent with
SendMessage to its ID, e.g. `a68fff8b4cdb3ddbf`, to re-examine a batch.)

## Lead review log (Opus 4.8)

Findings folded into `upstream-sync-v0.2.9.md`. Key sanity checks and corrections:

1. **Providers (batch 4) — CORRECTED.** Subagent marked gemini/bedrock/deepseek
   fixes "cherry-pick -x, no conflict." Verified `ls pkg/providers/` — **no
   gemini/bedrock/deepseek provider exists in the fork.** A fix to a non-existent
   provider can't be cherry-picked. Reclassified `6fbd7e0a3`, `cbae69ad6`,
   `83e93ca57`, `ad5232ade`, `4f90909af`, `1722cfc28` → **DEFER**. Genuinely
   portable provider fixes are only anthropic (`54654d279`),
   anthropic_messages/GLM (`8d97896a0`), claude_cli (`56fb0dc4e`), extra_body
   (`c7544f7cb`).
2. **Security (batch 6) — VERIFIED already ported.** Confirmed `base.go:117`
   (open-by-default warning + `*`), `shell.go:38` (disk-wipe regex),
   `logger_3rd_party.go` (`maskSecrets`). All 6 → DONE, no action.
3. **Cron independent session (`36b9693d3`) — VERIFIED needed.** `pkg/tools/cron.go:383`
   still uses `cron-%s` (no per-execution timestamp). High value for our trading
   cron → TAKE.
4. **loop polling (`230942d23`) — VERIFIED already applied** (idleTicker present).
5. **Agent allowlist/discovery/hooks cluster** — subagent correctly flagged the
   AGENT.md tool-allowlist and discovery subsystems were removed in our fork;
   those commits → SKIP. Event-bus/hook foundations → DEFER per user decision.
6. **Stop-command chain** — subagent correctly identified dependence on scoped
   steering infra absent in the fork → DEFER.
7. **Seahorse FTS5 (`b8819bdbf`)** — our `pkg/seahorse` has `fts5_sanitize.go`
   but I did not confirm a schema file with FTS5 triggers → marked
   RESOLVE/INVESTIGATE (verify before porting).
8. **Symmetric SKIP verification** (SKIP is the costly direction — a wrong call
   silently drops a fix). Grep-verified the agent-core SKIP cluster instead of
   trusting the subagent's structural claim:
   - `96fd887ca` (MCP allowlist case-insensitive): no MCP-server allowlisting in
     `pkg/mcp/` or `loop_mcp.go` → fix is moot, **SKIP correct**.
   - `409251e69` / `765a16547` / `2ae25b103` (AGENT.md frontmatter/tool decls):
     fork uses config-driven `IsToolEnabled` (instance.go) but has **no AGENT.md
     frontmatter parser** → **SKIP correct**.
   - Re-verified the two DONE calls I'd taken on faith: `5a13616b5` (setInitErr
     present) and `51f8285f9` (`!(freebsd && arm)` constraint present) → both DONE.
   No beneficial fixes were wrongly dropped.

Outstanding to verify during execution: feishu/weixin/wecom fork layouts; web
backend `SecureString` API vs upstream model-test handler; whether the fork uses
systray (tray-fallback commits).

---

## Wave 1 execution log (branch `sync/upstream-v0.2.9`)

**Environment fix (blocker):** `goenv` exported stale `GOROOT`/`GOTOOLDIR` (go1.24.1)
while go.mod requires 1.25.11, so the re-exec'd toolchain used the 1.24.1
`compile` → "version go1.24.1 does not match go tool version go1.25.11". Workaround
for all builds/tests: `env -u GOTOOLDIR -u GOROOT go ...`.

**Key recalibration — dispositions were over-optimistic.** Cherry-pick almost never
applies: the subagents checked "did *we* modify this file" but not "does the base
file still exist / was it reorganized / is the fix already present." Three failure
modes found empirically:
1. **Already ported under a different sha** (prior sync work): `56fb0dc4e` claude_cli
   (= our `c9019be3`), `6d7d1b090` line QuoteToken, `bacb9aba7` line body-close,
   `61a899cfb` cron OutboundChan test. → reclassify **DONE**.
2. **Base file reorganized/absent** → cherry-pick conflicts, needs MANUAL port:
   `0f5207676` serial tool (fork flattened `pkg/tools/hardware/`); `34b9d5d6f`
   telegram OAuth (fork merged the parser into `telegram.go` with a *different*
   regex pipeline → real reimplementation, not a port).
3. **Genuinely portable** (done): `36b9693d3` cron independent session — manually
   applied + fork test `cron_test.go` updated to assert the `cron-{id}-{ts}` prefix;
   `go test ./pkg/tools -run TestCron` passes. Committed `c9d95571`.

**Implication:** real Wave-1 effort is per-commit engineering (presence-check →
port/reimplement → fix fork tests → build), not bulk cherry-pick. The "TAKE"
counts overstate clean ports; expect a meaningful fraction to collapse to DONE or
MANUAL after presence/base checks.

**Committed so far:**
- `ecbedb7a` docs(sync): tracking + progress
- `c9d95571` fix(cron): independent session per job execution (verified)
