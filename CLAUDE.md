# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

KhunQuant is an ultra-lightweight personal AI assistant written in Go. It targets minimal hardware ($10 devices) with <10MB RAM and <1 second boot time. It supports multiple LLM providers (Anthropic, OpenAI, Azure, Ollama, local models) and multiple chat platforms (Telegram, Discord, WeChat, Feishu, Matrix, IRC, etc.).

## Common Commands

```bash
# Development
make deps              # Download and verify dependencies
make build             # Build khunquant binary for current platform
make run ARGS="..."    # Build and run with arguments
make test              # Run all tests
make check             # Full pre-commit check (deps + fmt + vet + test)

# Code quality
make fmt               # Format code
make lint              # Run golangci-lint
make vet               # Run go vet
make fix               # Auto-fix linting issues

# Single test
go test ./pkg/agent/... -run TestName

# Cross-platform builds
make build-all         # All platforms (linux/amd64, arm, arm64, mips, riscv64, darwin, windows)
make build-pi-zero     # Raspberry Pi Zero 2 W

# Web launcher
cd web && make dev     # Start frontend + backend dev servers
cd web && make build   # Build frontend and embed into Go binary

# Docker
make docker-build      # Alpine-based minimal image
make docker-build-full # Full image with Node.js 24 (MCP support)
make docker-run        # Run gateway in Docker
```

## Architecture

### Entry Points
- `cmd/khunquant/` — Main agent binary using Cobra CLI. Subcommands: `onboard`, `agent`, `auth`, `gateway`, `status`, `cron`, `migrate`, `skills`, `model`, `version`
- `cmd/khunquant-launcher-tui/` — Terminal UI launcher

### Core Packages (`pkg/`)

**Agent Core:**
- `pkg/agent/` — Core AI agent loop (`loop.go`), context management (`context.go`), instance management (`instance.go`), memory (`memory.go`)
- `pkg/providers/` — LLM provider abstraction with ~40 subdirectories. Each provider implements a common interface. Supports Anthropic, OpenAI, Azure, Ollama, and local models.
- `pkg/channels/` — Chat platform adapters (Discord, Telegram, WeChat, Feishu, Matrix, IRC, Line, OneBot, etc.)
- `pkg/tools/` — Agent tool implementations (filesystem, shell, editing, cron, search, MCP, I2C, etc.)
- `pkg/skills/` — Extensible skills framework
- `pkg/commands/` — Built-in agent commands (help, check, list, switch, etc.)

**Infrastructure:**
- `pkg/config/` — Centralized configuration with migration support (`config.go`, `migration.go`, `defaults.go`)
- `pkg/routing/` — Message routing between channels
- `pkg/session/` — User session handling
- `pkg/bus/` — Event bus for inter-component communication
- `pkg/mcp/` — Model Context Protocol support
- `pkg/memory/` — Agent memory and state persistence (SQLite via `modernc.org/sqlite`)
- `pkg/logger/` — Structured logging (zerolog-based)
- `pkg/cron/` — Scheduled task execution

**Web App (`web/`):** Vue.js/Vite frontend + Go backend for the web launcher UI.

### Key Architectural Patterns
- **Provider abstraction:** All LLM providers implement a common interface; swappable at runtime
- **Channel adapters:** Each chat platform is a pluggable adapter, decoupled from the agent core
- **Event-driven:** Components communicate via the event bus in `pkg/bus/`
- **Tool/skill registry:** Tools and skills are registered at startup and invoked by the agent loop
- **Config-driven:** Single config file drives provider selection, channel enablement, and feature flags

## Configuration

- `.env.example` — Template for environment variables (LLM API keys, channel tokens)
- `.golangci.yaml` — Linter config (many rules disabled; check before enabling new ones)
- `workspace/` — Default agent workspace and built-in configuration

## CI and Code Quality

`.github/workflows/pr.yml` runs five checks on every pull request. All of them must pass before
merge:

| Check | What it runs |
|---|---|
| **Linter** | `go generate ./...`, then `golangci-lint` **pinned to v2.10.1** |
| **Tests** | `go generate ./...`, then `go test ./...` (no flags) |
| **Security Check** | `govulncheck -C . -format text ./...` (govulncheck v1.1.4) |
| **Frontend Build** | in `web/frontend`: `pnpm install --frozen-lockfile`, `pnpm build`, a stale-`routeTree.gen.ts` diff check, `pnpm lint` |
| **Docker Build** | builds `docker/Dockerfile` without pushing |

### `make check` does not run the linter

`make check` is `deps fmt vet test` — note the absence of `lint`. A green `make check` therefore
does **not** mean CI will pass. Run `make lint` separately before pushing, or you will discover
lint failures only after CI does.

### `gofmt` is not this repo's formatter

`make fmt` is `golangci-lint fmt`, not `gofmt`. Plain `gofmt -l ./...` reports ~39 files that CI
is perfectly happy with, so it is a misleading signal — a clean `gofmt` proves nothing and a dirty
one is usually noise. Judge formatting by `golangci-lint` only.

### Match CI's linter version exactly

Lint findings differ between versions. If `golangci-lint` is not installed locally, run the same
version CI does rather than guessing:

```bash
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1 run
```

`.golangci.yaml` disables many rules; check it before enabling new ones. Rules that are enabled and
commonly trip people up: `unconvert` (redundant type conversions) and `whitespace` (no blank line
immediately after a function's opening brace — easy to leave behind when deleting a guard clause).

### `go generate ./...` runs before lint and test in CI

If generated output is stale or generation fails, CI fails in a way that reproduces locally only
if you run `go generate ./...` first. The `make test` and `make vet` targets already depend on it.

## Testing Conventions

**Never hardcode machine-absolute paths in tests.** A test containing
`/Users/<someone>/...` passes only on that person's machine and fails on CI and on every other
developer's checkout. Resolve repo-relative paths by walking up to `go.mod` — see
`getFixturesDir` in `pkg/sandbox/e2e/e2e_test.go` for the established helper.

**Fail loudly when a fixture/data load comes back empty.** `sandbox.Store.Load` deliberately
treats a missing directory as "nothing to serve" and returns `nil`. That is correct for
production, but in a test it converts a one-line path bug into a confusing downstream error
several layers away (`no fixture configured for GET ...`). Assert non-empty immediately after
loading so the failure names its real cause.

**Bug reproducers**: when committing a test that documents an unfixed defect, gate it behind an
environment variable so the default suite stays green, and delete the gate in the same change that
fixes the defect. Do not leave a permanently-red test in the tree.

## Git and PR Conventions

- Conventional commit subjects with a scope: `feat(xxx):`, `fix(yyy):`, `test(zzz):`.
- Split a large change into commits by scope rather than one omnibus commit.
- A PR body should state what the change adds, its safety-relevant properties, how it was tested,
  and its known limitations — reviewers should not have to infer any of these from the diff.

## Security Conventions

See SECURITY.md for the full posture. The rules that new code most often breaks:

- **Every new tool must be classified in `pkg/tools/classification.go`** (`ClassOf`: read,
  notify, local state, automation, propose, trade, funds, system), choosing the more restrictive class when
  in doubt. Autonomous turns (heartbeat, scheduled jobs, and anything spawned from them) may call
  read, notify and local-state tools only, with two named openings (the second, `remember`, only
  suggests a memory and is described in the memory bullet below). The first: `propose_order` (class `propose`),
  open to a candidate-driven proactive turn (`TurnProactive`, never a heartbeat, sweep, cron or
  sub-turn) whose candidates are all of at least `TrustExchange`, that attention decided to
  deliver now, and that has read no outside text (`web_search`, `web_fetch`, files, skills, news,
  MCP: any tool not in `trustedDataTools` lowers the turn's trust floor for the rest of the turn).
  It may only propose a risk-reducing order, and it places nothing: the owner, and only the
  owner, approves exactly the stored order before it expires, and it then runs as the journal
  workflow `approval.order` (`pkg/approval`), which re-checks every gate. Proposal cards are a
  second delivery path (`notify.Deliver` directly) that bypasses the attention outbox on purpose.
  The grant is `proposeOrderOpen` in `pkg/tools/proposal_scope.go`; do not widen it by class.
  An unclassified tool — every MCP tool included — is denied there and hidden from its tool
  definitions.
  `TestClassOf_AllRegisteredToolsClassified` (and the gateway variant) fail on a registered tool
  with no class. Exits are trade tools too, so autonomous turns cannot call them. A turn-starting call
  site must tag its origin (`tools.WithToolOrigin`); a context built from `context.Background()`
  that runs a model-driven turn must carry the origin and `AutomationScope` over.
- **A new read-class tool is visible to proactive turns automatically.** A read tool that sends model-chosen
  data to a third party (as `web_fetch` does with its URL) must be excluded in `proactiveTurnMayCall`
  (`pkg/tools/policy.go`). A turn the proactive pipeline starts goes through `agent.RunProactiveTurn` and never
  tags `OriginProactive` itself.
- **A new workspace state directory or prompt file goes in `autonomousProtectedPaths`**
  (`pkg/tools/autonomous_paths.go`): autonomous turns may write workspace files with
  `write_file`/`edit_file`/`append_file`, except under that list, so anything that acts as state,
  code or system prompt must be on it. A new tool that writes a file at a model-chosen path must
  call `guardAutonomousWrite`. Job-name prefixes that select a handler are defined once in
  `pkg/tools/reserved_jobs.go`; a new one is added there so the `cron` tool refuses it.
- **Memory writes from autonomous turns go through `remember`, never a file tool.** `memory/` (all of it) and
  `HEARTBEAT.md` are on `autonomousProtectedPaths`; `memory/` is also `OwnerOnly`, so while an owner is
  configured a person's turn other than the owner's own root turn is refused too. `remember`
  (`pkg/tools/remember.go`) saves directly only in the owner's root turn; everywhere else it creates an
  `approval.Proposal` of kind `memory` that reaches the owner in the next digest (the fact travels only as
  `Candidate.Quote`, never in a title, summary or tool result) and is appended by code on Save. A new
  code path that puts model-written text into `MEMORY.md` or a prompt file needs the same shape.

- **A tool that moves money gets a row in `defaultToolPolicies()`** (`pkg/tools/policy.go`),
  keyed by its `Name*` constant: `BlockedOrigins: {OriginCron}`, and `RefuseUnderPaperMode` unless
  the tool simulates paper mode itself. `TestPolicyCoversKnownFinancialTools` lists them
  explicitly — add the new tool there. A tool that only *reduces* exposure gets no row, so a
  person is never gated from getting out (the class ceiling above is what keeps autonomous turns
  away from it). `execute_dca_order` is the deliberate exception: it has no row because a `dca:`
  job must be able to call it, and the ceiling and its own plan-ID check bound it instead.
- **Paper mode** (`trading_risk.paper_trading_mode`): spot placement goes through
  `broker.TradingProviderFor`, which returns a simulation in paper mode while every gate runs as
  it does live; `execute_dca_order` simulates itself; a tool with no simulation is refused
  (`RefuseUnderPaperMode`); exits stay real, and the close, reduce, flatten and unwind tools mark
  their result with `markRealExit`.
- **Trading tools check `CheckPermission`, `CheckRiskNotional` and — on a risk-increasing path
  only — `broker.GlobalLossTracker.CheckDailyLossFor(limit, broker.AccountKey(cfg, provider,
  account))`.** That is per account and fails closed with `ErrEquityUnknown` when the equity
  guard is armed and the account's sample is stale. `CheckDailyLoss` is the total-only form: do
  not call it on a new order path. In `create_order` the risk-increasing orders are buys, sells
  of a contract symbol (`:` in the symbol) and sells carrying margin/borrow/leverage params; in
  `execute_dca_order`, buys and sells of a contract symbol. Risk-increasing futures mutations
  also call `CheckLeverage(cfg, action, broker.RiskIncrease)`; exits pass `broker.RiskReduce` and
  skip the daily-loss check — a safety brake must never block getting out of a position. If an action
  cannot be classified before it runs, treat it as `RiskIncrease`. `DefaultLimiter.Allow` goes
  after the dry-run return, right before the first mutation. Reject non-finite numbers yourself
  — the generic validator does not.
- **USD caps are enforced in USD.** `max_order_value_usd` and `max_dca_amount_per_order_usd` are
  USD, but `amount × price` is in the symbol's quote currency (THB on Bitkub). Value the order
  with `broker.EstimateNotionalUSDWithFallback` and refuse it when the USD value is unknown while
  a cap is set; never compare `amount × price` to a cap.
- **A mandate (`pkg/dca/mandate.go`) is created only by `Store.CreateMandateFromProposal`, from an approved, owner-decided, hash-verified proposal of kind `mandate`** (the approval executor `tools.mandateSink`); never by a tool argument, never for an inferred intent. The `dca.execute` workflow (`pkg/dca/workflow.go`) places a buy only through `dcaExchange.PlaceMandateBuy`, only from a live `dca_due` candidate of a plan the gateway sync promoted, and every cap in it is USD (`EstimateNotionalUSDWithFallback`; unknown refuses). A new caller of `StartExecute` or `PlaceMandateBuy`, or a new place that names `approval.OriginMandate`, needs a deliberate edit of `TestOrderExecutionCallGraph`.
- **A new `ToolConfig` field needs an `IsToolEnabled` switch arm**, which is default-allow for
  unknown names. `TestCatalogToolsHaveEnableArms` catches a missing arm for catalog tools.
- **Credentials under a `yaml:"-"` subtree** (`Providers`, `Tools`, `Debug`, `Gateway`, most
  channels) must be plain strings named in `config.CredentialFieldNames`, never `SecureString` —
  a `SecureString` there marshals as withheld with nothing to restore it and blanks on the next
  save. The name list is what keeps them out of API responses and logs.
- **Anything that builds a context from `context.Background()` and then runs tools** must carry
  `tools.WithToolOrigin` / `tools.WithToolSender` over from the parent. Untagged turns fail closed.
- **New tools are covered by the policy hook automatically.** `ToolRegistry.ExecuteWithContext` runs
  the hooks (`Hook`, `AddHook`; the live policy hook is `tools.SetPolicyLive`) for every tool, MCP
  and runtime-discovered ones included; a trade/funds/propose class tool needs nothing extra. A new
  trade path must go through `ExecuteWithContext`: one that calls a tool's `Execute` directly
  bypasses the hook (as the executors `approval.order`, `dca.execute` and `dn.*` do, which keep
  their own gates). The hook only adds refusals and never creates a proposal.
- **Package-level state armed from config must be re-armed on reload**, not just at startup —
  see `armToolPolicy`, called from both `NewAgentLoop` and `ReloadProviderAndConfig`.

## Exchange API Pitfalls

### Futures order `amount` is in **contracts**, not base currency

On OKX and Binance, `CreateOrder` for perpetual swaps expects the `amount` parameter to be the **number of contracts**, not the base-currency quantity.

Each market has a `contractSize` field (e.g. CHZ/USDT:USDT on OKX = 10 CHZ per contract). Passing raw base-currency units (e.g. `notionalUSDT / markPrice = 1351 CHZ`) instead of contract count (e.g. `1351 / 10 = 135`) inflates the order size by `contractSize`×, causing OKX error **51008 InsufficientFunds** or Binance equivalent even when the account has sufficient margin.

**Always use `contractsFromNotional(notionalUSD, markPrice, contractSize, minAmount)` in `pkg/tools/futures_helpers.go` to convert a USDT notional to a contract count.** Load the market with `validateActiveSwapMarket` to get `ContractSize` and `Limits.Amount.Min`.

### Futures order `side` vs position `side`

Exchange order APIs use `side = "buy" | "sell"` (order direction), not `"long" | "short"` (position direction). Passing `"short"` as `side` causes OKX error **51000 Parameter side error**.

Use `futuresPositionSide(positionSide string)` in `pkg/tools/futures.go` which maps:
- `"long"` / `"buy"` → order side `"buy"`, position side `"long"`
- `"short"` / `"sell"` → order side `"sell"`, position side `"short"`

The `posSide` / `PositionSide` field in the order request is separate and correctly takes `"long"` / `"short"`.

### A position's `Contracts` is a contract count, not a base quantity

Fetched positions report `Contracts` in the same unit as the order `amount` above. Valuing one as `contracts × markPrice` is wrong by `contractSize`× (10× for CHZ/USDT:USDT on OKX, 0.01× for BTC/USDT:USDT), which understates or overstates exposure, notional caps and funding estimates.

**Always use `positionNotionalUSD(contracts, contractSize, markPrice)` in `pkg/tools/futures_helpers.go`**, and get the size from a `positionContractSizeResolver` (the position's own `ContractSize`, else the market's, loaded once).

## Dependencies

Key direct dependencies: `github.com/anthropics/anthropic-sdk-go`, `github.com/openai/openai-go`, `github.com/spf13/cobra`, `github.com/rs/zerolog`, `github.com/modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `github.com/gorilla/websocket`, plus platform-specific SDKs for each chat channel.
