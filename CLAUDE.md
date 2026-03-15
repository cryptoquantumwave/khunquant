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

## Dependencies

Key direct dependencies: `github.com/anthropics/anthropic-sdk-go`, `github.com/openai/openai-go`, `github.com/spf13/cobra`, `github.com/rs/zerolog`, `github.com/modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `github.com/gorilla/websocket`, plus platform-specific SDKs for each chat channel.
