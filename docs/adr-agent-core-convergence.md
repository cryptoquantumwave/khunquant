# ADR: Converge the agent core onto upstream to enable cheap long-term syncing

**Status:** Accepted (strategy) · Step 1 implemented · remainder scheduled
**Date:** 2026-06
**Context source:** upstream sync of picoclaw v0.2.9 (see `upstream-sync-v0.2.9.md`)

## Problem

KhunQuant tracks upstream `sipeed/picoclaw` (proven: `pkg/seahorse` is byte-identical;
multiple `sync/*` branches). But our `pkg/agent` core diverged from upstream's: upstream
refactored the monolithic `loop.go` into a `pipeline_*` / `adapters` / `interfaces`
architecture and added hooks + an event bus, while we kept the monolith and evolved it
independently (~3770/592 lines of delta across non-test agent files). Result: a growing
**fork-drift tax** — agent-core fixes increasingly land in files we don't have and must be
hand-translated each release. We want future upstream syncs to stay cheap.

## Decision

Converge our agent core onto upstream's architecture so our `pkg/agent` delta trends toward
**near-zero**, making future syncs ordinary cherry-picks. Two non-obvious principles govern
how:

1. **The goal is "minimize our core delta," not "mimic their layout."** Upstream built
   hooks / event bus / `interfaces`+`adapters` precisely so downstream extensions don't fork
   the core. Re-express our add-ons through those **extension seams** instead of re-patching
   core files. Success metric: *lines of our delta remaining in `pkg/agent` core files*.
   - Crypto tools already ride the tool registry → effectively zero core delta. Keep it that way.
   - Trading-risk gating / observability / custom turn behavior → onto hooks/eventbus.
   - Session/context tweaks → through interfaces/adapters where possible.

2. **The recurring fix-tax is separable from architecture adoption.** Verified: blocked fixes
   like network-retry are self-contained logic that drop into our existing LLM call-site
   (`loop.go` `callLLM`/`Chat`) — they do *not* require the pipeline. So we hand-port fix
   *logic* as needed regardless, and treat full architecture adoption as its own decision,
   justified by wanting hooks/eventbus/turn-coord for KhunQuant's roadmap.

## Method — characterization-test-guarded refactor (green → red → green)

Refactoring a core with no spec → use characterization tests as the safety net, but:

- **Test at the STABLE SEAM, not internals.** Assertions go through the public
  `ProcessDirect` / message-bus API with provider/tool fakes (inbound → outbound, tool calls
  + args, session/history state, system prompt). These survive the architecture swap and
  *compile on both layouts* — tests bound to `runTurn`/`callLLM`/internal types would fail to
  compile after the swap and are worse than nothing.
- **Flow:** suite green on current core → adopt upstream's `pkg/agent` wholesale (red) →
  re-integrate our deltas via extension seams → green.
- **Re-integration is a triage, not a blind re-apply:** for each red, decide KEEP (genuine
  fork behavior → re-apply via a seam), DROP (our parallel impl now provided by upstream), or
  ADAPT (the assertion can't survive — change it consciously).

### Convergence target & timing
- Converge to the **latest stable upstream**, not v0.2.9 (upstream refactored the agent core
  twice in our window; v0.2.9's layout is already superseded — converging there risks
  re-paying soon).
- Prefer timing the wholesale adoption to when upstream's agent architecture has settled.
- Caveat: characterization can't capture everything (timing, concurrency, context-budget
  math); expect a few "can't preserve" decisions.

## Status / next steps

- ✅ **Step 1 (done):** characterization suite at the seam — `pkg/agent/characterization_test.go`
  (plain response, tool round-trip, session continuity, system prompt). Green on current core.
  This is the permanent guard for every future sync.
- ✅ **Step 2 (done):** expanded the suite with core turn-loop guards — tool-iteration cap,
  NoHistory isolation, multiple tool calls in one response, tool-error propagation, and
  context-cancellation aborting a stuck turn (the seam mechanism hard-abort relies on, tested
  via the public `ctx` not internal scope keys). 9 char tests; cancellation verified stable.
  - Scope note: **trading-risk gating (paper-trading / leverage) is in `pkg/tools`+`pkg/exchanges`,
    not `pkg/agent`** — it can't regress from an agent-core rebase, so it is guarded by its own
    tool tests and intentionally excluded here.
  - Still seam-testable but deferred to broaden later: context-budget/summarization triggers and
    multi-agent delegate/spawn routing (both need more setup — a summarizing provider / a
    multi-agent registry — and are lower-churn than the core turn loop).
- ✅ **Step 3 (spike — done, measured & discarded):** dropped upstream v0.2.9's *entire* `pkg/agent`
  onto a throwaway branch (import paths rewritten), kept our seam characterization test, measured
  the blast radius, then discarded. Findings (decision-grade):
  1. **Caller blast radius is tiny:** only **3 packages** outside `pkg/agent` import it
     (`cmd/khunquant/internal/agent`, `cmd/khunquant/internal/gateway`, `pkg/devmcp`). The core is
     well-encapsulated — re-integration touches a narrow surface.
  2. **Adopting upstream's agent is dependency-closure work, not logic-untangling:** the wholesale
     overlay produced **12 compile errors, all "missing package/symbol"** — upstream sibling
     packages our fork lacks (`pkg/audio`, `pkg/events`, `pkg/isolation`, `pkg/evolution`, + 2
     `pkg/providers` symbols). **Zero** were semantic conflicts inside the agent logic. So the
     rebase is "bring ~5 sibling packages + wire," which is far more tractable than feared.
  3. **The seam methodology is validated:** 8 of our 9 characterization tests rely on public API
     that survives the refactor **unchanged** — `NewAgentLoop` (even stays backward-compatible via
     new variadic opts), `ProcessDirect` (identical), `RegisterTool`, `toolLimitResponse`. The
     **one** drift: upstream removed the fork-specific `noHistory` param from
     `ProcessDirectWithChannel`, so `CHAR-6 (NoHistoryIsolation)` is the single test needing a
     conscious ADAPT (re-add `noHistory` as our delta, or adopt upstream's no-history mechanism).
     This is exactly the kind of decision point the guard exists to surface.
  - **Net:** the spike *lowered* the risk estimate. The agent rebase looks like a bounded
    bring-the-deps + thin-reintegration effort (encapsulated, conflict-free logic), not a
    multi-week untangle. Recommend: bring the ~5 sibling packages first, then the wholesale
    `pkg/agent` adoption, re-greening this suite (adapting CHAR-6 consciously).
- ✅ **Step 3b (done):** sized the **real** target. After `git fetch upstream`, the latest stable
  tag is **v0.3.0** (upstream/main is +283 from v0.2.9). Findings:
  1. **The agent architecture has STABILIZED.** `pkg/agent` is essentially unchanged v0.2.9→v0.3.0
     (only a new `turn_state_test.go`; no source/structural changes). The "moving target" risk is
     resolved — v0.3.0 is a settled convergence point.
  2. **Dependency closure is clean and small.** Upstream's v0.3.0 agent needs only 4 sibling
     packages we lack; none pull further missing `pkg/*`. By *actual agent coupling*:
     - **`pkg/events`** — 19 agent files / 45 symbols, core to the new architecture → **must bring**
       (1174 LOC, pure stdlib, no external deps — easy).
     - **`pkg/isolation`** — 2 files / 2 symbols (`Configure`, `Start`) → trivial: bring (949 LOC)
       or a 2-func stub.
     - **`pkg/audio`** — **0 agent files / 0 symbols at v0.3.0** → **skip** (avoids pulling
       `pion/webrtc`+`pion/rtp`; keeps footprint).
     - **`pkg/evolution`** — 1 agent file but 30 symbols (self-evolution) → **product decision**:
       bring (5874 LOC, clean, no ext deps) or exclude that one feature file.
  - **Revised estimate:** the mandatory dependency cost collapses to **one small clean package
    (`pkg/events`)** + a trivial isolation shim. Audio drops out; evolution is opt-in. Combined
    with the tiny caller blast radius (3 importers) and conflict-free agent logic, the convergence
    is a **bounded, well-scoped project**, not a fork-threatening rewrite.

## Recommended execution (updated by the spikes)

Target **v0.3.0** (stable, settled architecture). On a branch:
1. Bring `pkg/events` (clean) and decide isolation (bring/stub) + evolution (adopt/exclude) + skip audio.
2. Overlay upstream v0.3.0 `pkg/agent`; rewrite import paths.
3. Re-apply our deltas via extension seams (tools already via registry; trading observability via
   `pkg/events`/hooks; re-add `noHistory` to `ProcessDirectWithChannel` as our documented delta).
4. Re-green the characterization suite (consciously ADAPT CHAR-6 for the `noHistory` signature).
5. Update the 3 caller packages (`cmd/.../agent`, `cmd/.../gateway`, `pkg/devmcp`) for any API drift.
6. Crypto end-to-end validation (a real paper-trade flow) before merge.
- ▶ **Step 4:** full wholesale adoption + seam-based re-integration, suite as the gate, agent-core
  feature-freeze during the rebase, plus crypto end-to-end validation (a real paper-trade flow).
- Meanwhile: keep hand-porting fix *logic* (network-retry etc.) to stop the bleeding — independent
  of the rebase.

## Alternative considered (rejected as the default)

Stay diverged and only hand-port fixes forever. Rejected because the tax compounds and we
already demonstrably want upstream's agent work (seahorse) — but the fix-logic hand-porting is
retained as the interim measure, and full adoption is gated on wanting the new subsystems.
