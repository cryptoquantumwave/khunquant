.PHONY: all build install uninstall clean help test sync-workspace

# Build variables
BINARY_NAME=khunquant
BUILD_DIR=build
CMD_DIR=cmd/$(BINARY_NAME)
MAIN_GO=$(CMD_DIR)/main.go
EXT=

# Version
VERSION?=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT=$(shell git rev-parse --short=8 HEAD 2>/dev/null || echo "dev")
BUILD_TIME=$(shell date +%FT%T%z)
GO_VERSION=$(shell $(GO) version | awk '{print $$3}')
CONFIG_PKG=github.com/cryptoquantumwave/khunquant/pkg/config
LDFLAGS=-ldflags "-X $(CONFIG_PKG).Version=$(VERSION) -X $(CONFIG_PKG).GitCommit=$(GIT_COMMIT) -X $(CONFIG_PKG).BuildTime=$(BUILD_TIME) -X $(CONFIG_PKG).GoVersion=$(GO_VERSION) -s -w"

# Go variables
GO?=CGO_ENABLED=0 go
WEB_GO?=$(GO)
GO_BUILD_TAGS?=stdjson
GOFLAGS?=-v -tags $(GO_BUILD_TAGS)
comma:=,
empty:=
space:=$(empty) $(empty)
GO_BUILD_TAGS_NO_GOOLM:=$(GO_BUILD_TAGS)
GOFLAGS_NO_GOOLM?=-v -tags $(GO_BUILD_TAGS_NO_GOOLM)

# Patch MIPS LE ELF e_flags (offset 36) for NaN2008-only kernels (e.g. Ingenic X2600).
#
# Bytes (octal): \004 \024 \000 \160  →  little-endian 0x70001404
#   0x70000000  EF_MIPS_ARCH_32R2   MIPS32 Release 2
#   0x00001000  EF_MIPS_ABI_O32     O32 ABI
#   0x00000400  EF_MIPS_NAN2008     IEEE 754-2008 NaN encoding
#   0x00000004  EF_MIPS_CPIC        PIC calling sequence
#
# Go's GOMIPS=softfloat emits no FP instructions, so the NaN mode is irrelevant
# at runtime — this is purely an ELF metadata fix to satisfy the kernel's check.
# patchelf cannot modify e_flags; dd at a fixed offset is the most portable way.
#
# Ref: https://codebrowser.dev/linux/linux/arch/mips/include/asm/elf.h.html
define PATCH_MIPS_FLAGS
	@if [ -f "$(1)" ]; then \
		printf '\004\024\000\160' | dd of=$(1) bs=1 seek=36 count=4 conv=notrunc 2>/dev/null || \
		{ echo "Error: failed to patch MIPS e_flags for $(1)"; exit 1; }; \
	else \
		echo "Error: $(1) not found, cannot patch MIPS e_flags"; exit 1; \
	fi
endef

# Golangci-lint
GOLANGCI_LINT?=golangci-lint

# Installation
INSTALL_PREFIX?=$(HOME)/.local
INSTALL_BIN_DIR=$(INSTALL_PREFIX)/bin
INSTALL_MAN_DIR=$(INSTALL_PREFIX)/share/man/man1
INSTALL_TMP_SUFFIX=.new

# Workspace and Skills
KHUNQUANT_HOME?=$(HOME)/.khunquant
WORKSPACE_DIR?=$(KHUNQUANT_HOME)/workspace
WORKSPACE_SKILLS_DIR=$(WORKSPACE_DIR)/skills
BUILTIN_SKILLS_DIR=$(CURDIR)/skills

LSCMD=ln -sf

# OS detection
UNAME_S?=$(shell uname -s)
UNAME_M?=$(shell uname -m)

# Platform-specific settings
ifeq ($(UNAME_S),Linux)
	PLATFORM=linux
	ifeq ($(UNAME_M),x86_64)
		ARCH=amd64
	else ifeq ($(UNAME_M),aarch64)
		ARCH=arm64
	else ifeq ($(UNAME_M),armv81)
		ARCH=arm64
	else ifeq ($(UNAME_M),loongarch64)
		ARCH=loong64
	else ifeq ($(UNAME_M),riscv64)
		ARCH=riscv64
	else ifeq ($(UNAME_M),mipsel)
		ARCH=mipsle
	else
		ARCH=$(UNAME_M)
	endif
else ifeq ($(UNAME_S),Darwin)
	PLATFORM=darwin
	WEB_GO=CGO_ENABLED=1 go
	ifeq ($(UNAME_M),x86_64)
		ARCH=amd64
	else ifeq ($(UNAME_M),arm64)
		ARCH=arm64
	else
		ARCH=$(UNAME_M)
	endif
else
	PLATFORM=$(UNAME_S)
	ifeq ($(UNAME_M),x86_64)
		ARCH?=amd64
	else
	    ARCH?=$(UNAME_M)
	endif
	# Detect Windows (Git Bash / MSYS2)
    IS_WINDOWS:=$(if $(findstring MINGW,$(UNAME_S)),yes,$(if $(findstring MSYS,$(UNAME_S)),yes,$(if $(findstring CYGWIN,$(UNAME_S)),yes,no)))
	ifeq ($(IS_WINDOWS),yes)
	    EXT=.exe
	    LSCMD=cp
	else ifeq ($(UNAME_S),windows) // failsafe for force windows build in other OS
		EXT=.exe
	endif

endif

BINARY_PATH=$(BUILD_DIR)/$(BINARY_NAME)-$(PLATFORM)-$(ARCH)

# Default target
all: build

## generate: Run generate
generate:
	@echo "Run generate..."
	@rm -r ./$(CMD_DIR)/workspace 2>/dev/null || true
	@$(GO) generate ./...
	@echo "Run generate complete"

## build: Build the khunquant binary for current platform
build: generate
	@echo "Building $(BINARY_NAME)$(EXT) for $(PLATFORM)/$(ARCH)..."
	@mkdir -p $(BUILD_DIR)
	@GOARCH=${ARCH} $(GO) build $(GOFLAGS) $(LDFLAGS) -o $(BINARY_PATH)$(EXT) ./$(CMD_DIR)
	@echo "Build complete: $(BINARY_PATH)$(EXT)"
	@$(LSCMD) $(BINARY_NAME)-$(PLATFORM)-$(ARCH)$(EXT) $(BUILD_DIR)/$(BINARY_NAME)$(EXT)

## build-launcher: Build the khunquant-launcher (web console) binary
build-launcher:
	@echo "Building khunquant-launcher for $(PLATFORM)/$(ARCH)..."
	@mkdir -p $(BUILD_DIR)
	@if [ ! -f web/backend/dist/index.html ]; then \
		echo "Building frontend..."; \
		cd web/frontend && pnpm install && pnpm build:backend; \
	fi
	@$(GO) build $(GOFLAGS) -o $(BUILD_DIR)/khunquant-launcher-$(PLATFORM)-$(ARCH) ./web/backend
	@ln -sf khunquant-launcher-$(PLATFORM)-$(ARCH) $(BUILD_DIR)/khunquant-launcher
	@echo "Build complete: $(BUILD_DIR)/khunquant-launcher"

## build-whatsapp-native: Build with WhatsApp native (whatsmeow) support; larger binary
build-whatsapp-native: generate
## @echo "Building $(BINARY_NAME) with WhatsApp native for $(PLATFORM)/$(ARCH)..."
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./$(CMD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm ./$(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./$(CMD_DIR)
	GOOS=linux GOARCH=loong64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-loong64 ./$(CMD_DIR)
	GOOS=linux GOARCH=riscv64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-riscv64 ./$(CMD_DIR)
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat $(GO) build -tags $(GO_BUILD_TAGS_NO_GOOLM),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle ./$(CMD_DIR)
	$(call PATCH_MIPS_FLAGS,$(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle)
	GOOS=darwin GOARCH=arm64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./$(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build -tags $(GO_BUILD_TAGS),whatsapp_native -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./$(CMD_DIR)
## @$(GO) build $(GOFLAGS) -tags whatsapp_native -ldflags "$(LDFLAGS)" -o $(BINARY_PATH) ./$(CMD_DIR)
	@echo "Build complete"
##	@ln -sf $(BINARY_NAME)-$(PLATFORM)-$(ARCH) $(BUILD_DIR)/$(BINARY_NAME)

## build-linux-arm: Build for Linux ARMv7 (e.g. Raspberry Pi Zero 2 W 32-bit)
build-linux-arm: generate
	@echo "Building for linux/arm (GOARM=7)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm ./$(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)-linux-arm"

## build-linux-arm64: Build for Linux ARM64 (e.g. Raspberry Pi Zero 2 W 64-bit)
build-linux-arm64: generate
	@echo "Building for linux/arm64..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./$(CMD_DIR)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64"

## build-linux-mipsle: Build for Linux MIPS32 LE
build-linux-mipsle: generate
	@echo "Building for linux/mipsle (softfloat)..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat $(GO) build $(GOFLAGS_NO_GOOLM) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle ./$(CMD_DIR)
	$(call PATCH_MIPS_FLAGS,$(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle)
	@echo "Build complete: $(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle"

## build-pi-zero: Build for Raspberry Pi Zero 2 W (32-bit and 64-bit)
build-pi-zero: build-linux-arm build-linux-arm64
	@echo "Pi Zero 2 W builds: $(BUILD_DIR)/$(BINARY_NAME)-linux-arm (32-bit), $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 (64-bit)"

## build-all: Build khunquant for all platforms
build-all: generate
	@echo "Building for multiple platforms..."
	@mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-amd64 ./$(CMD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm ./$(CMD_DIR)
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-arm64 ./$(CMD_DIR)
	GOOS=linux GOARCH=loong64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-loong64 ./$(CMD_DIR)
	GOOS=linux GOARCH=riscv64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-riscv64 ./$(CMD_DIR)
	GOOS=linux GOARCH=mipsle GOMIPS=softfloat $(GO) build $(GOFLAGS_NO_GOOLM) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle ./$(CMD_DIR)
	$(call PATCH_MIPS_FLAGS,$(BUILD_DIR)/$(BINARY_NAME)-linux-mipsle)
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-linux-armv7 ./$(CMD_DIR)
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-darwin-arm64 ./$(CMD_DIR)
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-windows-amd64.exe ./$(CMD_DIR)
	GOOS=netbsd GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-netbsd-amd64 ./$(CMD_DIR)
	GOOS=netbsd GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME)-netbsd-arm64 ./$(CMD_DIR)
	@echo "All builds complete"

## install: Install khunquant to system and copy builtin skills
install: build build-launcher
	@echo "Installing $(BINARY_NAME)..."
	@mkdir -p $(INSTALL_BIN_DIR)
	# Copy binary with temporary suffix to ensure atomic update
	@cp $(BUILD_DIR)/$(BINARY_NAME) $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@chmod +x $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@mv -f $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX) $(INSTALL_BIN_DIR)/$(BINARY_NAME)
	@echo "Installed binary to $(INSTALL_BIN_DIR)/$(BINARY_NAME)"
	@cp $(BUILD_DIR)/khunquant-launcher $(INSTALL_BIN_DIR)/khunquant-launcher$(INSTALL_TMP_SUFFIX)
	@chmod +x $(INSTALL_BIN_DIR)/khunquant-launcher$(INSTALL_TMP_SUFFIX)
	@mv -f $(INSTALL_BIN_DIR)/khunquant-launcher$(INSTALL_TMP_SUFFIX) $(INSTALL_BIN_DIR)/khunquant-launcher
	@echo "Installed binary to $(INSTALL_BIN_DIR)/khunquant-launcher"
	@echo "Installation complete!"

## sync-bin: Copy the last build to ~/.local/bin without rebuilding
sync-bin:
	@mkdir -p $(INSTALL_BIN_DIR)
	@cp $(BINARY_PATH) $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@chmod +x $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX)
	@mv -f $(INSTALL_BIN_DIR)/$(BINARY_NAME)$(INSTALL_TMP_SUFFIX) $(INSTALL_BIN_DIR)/$(BINARY_NAME)
	@echo "Synced $(BINARY_PATH) → $(INSTALL_BIN_DIR)/$(BINARY_NAME)"

## sync-workspace: Sync all .md files from workspace/ to WORKSPACE_DIR (overwrites duplicates)
sync-workspace:
	@echo "Syncing workspace .md files to $(WORKSPACE_DIR)..."
	@mkdir -p $(WORKSPACE_DIR)
	@cd $(CURDIR)/workspace && find . -name "*.md" | while read f; do \
		dest="$(WORKSPACE_DIR)/$$f"; \
		mkdir -p "$$(dirname $$dest)"; \
		cp "$$f" "$$dest"; \
		echo "  synced: $$f"; \
	done
	@echo "Sync complete → $(WORKSPACE_DIR)"

## uninstall: Remove khunquant from system
uninstall:
	@echo "Uninstalling $(BINARY_NAME)..."
	@rm -f $(INSTALL_BIN_DIR)/$(BINARY_NAME)
	@echo "Removed binary from $(INSTALL_BIN_DIR)/$(BINARY_NAME)"
	@echo "Note: Only the executable file has been deleted."
	@echo "If you need to delete all configurations (config.json, workspace, etc.), run 'make uninstall-all'"

## uninstall-all: Remove khunquant and all data
uninstall-all:
	@echo "Removing workspace and skills..."
	@rm -rf $(KHUNQUANT_HOME)
	@echo "Removed workspace: $(KHUNQUANT_HOME)"
	@echo "Complete uninstallation done!"

## release-local: Tag and publish using slim config (5 platforms, low RAM). Use this on a dev machine.
## Usage: make release-local RELEASE_TAG=v0.2.0-rc.1
release-local:
	@if [ -z "$(RELEASE_TAG)" ]; then echo "ERROR: set RELEASE_TAG, e.g. make release-local RELEASE_TAG=v0.2.0-rc.1"; exit 1; fi
	@if [ -z "$$GITHUB_TOKEN" ]; then echo "ERROR: GITHUB_TOKEN is not set"; exit 1; fi
	@which goreleaser > /dev/null 2>&1 || (echo "ERROR: goreleaser not installed. Run: brew install goreleaser"; exit 1)
	@echo "Tagging $(RELEASE_TAG)..."
	git tag -a $(RELEASE_TAG) -m "Release $(RELEASE_TAG)"
	git push origin $(RELEASE_TAG)
	@echo "Running GoReleaser (slim build, 2 parallel jobs)..."
	GOVERSION=$$(go version | awk '{print $$3}') goreleaser release --config .goreleaser.local.yaml --clean --parallelism 2 --skip=sign,announce

## release: Tag and publish a full release via GoReleaser (all platforms — needs a powerful CI machine).
## Usage: make release RELEASE_TAG=v0.2.0
release:
	@if [ -z "$(RELEASE_TAG)" ]; then echo "ERROR: set RELEASE_TAG, e.g. make release RELEASE_TAG=v0.2.0"; exit 1; fi
	@if [ -z "$$GITHUB_TOKEN" ]; then echo "ERROR: GITHUB_TOKEN is not set"; exit 1; fi
	@which goreleaser > /dev/null 2>&1 || (echo "ERROR: goreleaser not installed. Run: brew install goreleaser"; exit 1)
	@echo "Tagging $(RELEASE_TAG)..."
	git tag -a $(RELEASE_TAG) -m "Release $(RELEASE_TAG)"
	git push origin $(RELEASE_TAG)
	@echo "Running GoReleaser for $(RELEASE_TAG)..."
	GOVERSION=$$(go version | awk '{print $$3}') goreleaser release --clean --parallelism 2 --skip=docker,sign,announce

## clean: Remove build artifacts
clean:
	@echo "Cleaning build artifacts..."
	@rm -rf $(BUILD_DIR)
	@echo "Clean complete"

## vet: Run go vet for static analysis
vet: generate
	@packages="$$(go list ./...)" && \
		$(GO) vet $$(printf '%s\n' "$$packages" | grep -v '^github.com/sipeed/picoclaw/web/')
	@cd web/backend && $(WEB_GO) vet ./...

## test: Test Go code
test: generate
	@$(GO) test ./...

## fmt: Format Go code
fmt:
	@$(GOLANGCI_LINT) fmt

## lint: Run linters
lint:
	@$(GOLANGCI_LINT) run

## fix: Fix linting issues
fix:
	@$(GOLANGCI_LINT) run --fix

## deps: Download dependencies
deps:
	@$(GO) mod download
	@$(GO) mod verify

## update-deps: Update dependencies
update-deps:
	@$(GO) get -u ./...
	@$(GO) mod tidy

## check: Run vet, fmt, and verify dependencies
check: deps fmt vet test

## run: Build and run khunquant
run: build
	@$(BUILD_DIR)/$(BINARY_NAME) $(ARGS)

## docker-build: Build Docker image (minimal Alpine-based)
docker-build:
	@echo "Building minimal Docker image (Alpine-based)..."
	docker compose -f docker/docker-compose.yml build khunquant-agent khunquant-gateway

## docker-build-full: Build Docker image with full MCP support (Node.js 24)
docker-build-full:
	@echo "Building full-featured Docker image (Node.js 24)..."
	docker compose -f docker/docker-compose.full.yml build khunquant-agent khunquant-gateway

## docker-test: Test MCP tools in Docker container
docker-test:
	@echo "Testing MCP tools in Docker..."
	@chmod +x scripts/test-docker-mcp.sh
	@./scripts/test-docker-mcp.sh

## docker-run: Run khunquant gateway in Docker (Alpine-based)
docker-run:
	docker compose -f docker/docker-compose.yml --profile gateway up

## docker-run-full: Run khunquant gateway in Docker (full-featured)
docker-run-full:
	docker compose -f docker/docker-compose.full.yml --profile gateway up

## docker-run-agent: Run khunquant agent in Docker (interactive, Alpine-based)
docker-run-agent:
	docker compose -f docker/docker-compose.yml run --rm khunquant-agent

## docker-run-agent-full: Run khunquant agent in Docker (interactive, full-featured)
docker-run-agent-full:
	docker compose -f docker/docker-compose.full.yml run --rm khunquant-agent

## docker-clean: Clean Docker images and volumes
docker-clean:
	docker compose -f docker/docker-compose.yml down -v
	docker compose -f docker/docker-compose.full.yml down -v
	docker rmi khunquant:latest khunquant:full 2>/dev/null || true

## help: Show this help message
help:
	@echo "khunquant Makefile"
	@echo ""
	@echo "Usage:"
	@echo "  make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sort | awk -F': ' '{printf "  %-16s %s\n", substr($$1, 4), $$2}'
	@echo ""
	@echo "Examples:"
	@echo "  make build              # Build for current platform"
	@echo "  make install            # Install to ~/.local/bin"
	@echo "  make uninstall          # Remove from /usr/local/bin"
	@echo "  make sync-workspace     # Sync workspace .md files to ~/.khunquant/workspace"
	@echo "  make docker-build       # Build minimal Docker image"
	@echo "  make docker-test        # Test MCP tools in Docker"
	@echo ""
	@echo "Environment Variables:"
	@echo "  INSTALL_PREFIX          # Installation prefix (default: ~/.local)"
	@echo "  WORKSPACE_DIR           # Workspace directory (default: ~/.khunquant/workspace)"
	@echo "  VERSION                 # Version string (default: git describe)"
	@echo ""
	@echo "Current Configuration:"
	@echo "  Platform: $(PLATFORM)/$(ARCH)"
	@echo "  Binary: $(BINARY_PATH)"
	@echo "  Install Prefix: $(INSTALL_PREFIX)"
	@echo "  Workspace: $(WORKSPACE_DIR)"
