.PHONY: dev dev-frontend dev-backend build test lint clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LD_FLAGS := -X github.com/khunquant/khunquant/pkg/config.Version=$(VERSION)

# Run both frontend and backend dev servers
dev:
	@if [ ! -f backend/khunquant-web ] || [ ! -d backend/dist ]; then \
		echo "Build artifacts not found, building..."; \
		$(MAKE) build; \
	fi
	@echo "Starting backend and frontend dev servers..."
	@$(MAKE) dev-backend & $(MAKE) dev-frontend

# Start frontend dev server (Vite, with proxy to backend)
dev-frontend:
	cd frontend && pnpm dev

# Start backend dev server
dev-backend:
	cd backend && go run -ldflags "$(LD_FLAGS)" .

# Build frontend and embed into Go binary
build:
	cd frontend && pnpm build:backend
	cd backend && go build -ldflags "$(LD_FLAGS)" -o khunquant-web .

# Run all tests
test:
	cd backend && go test ./...
	cd frontend && pnpm lint

# Lint and format
lint:
	cd backend && go vet ./...
	cd frontend && pnpm check

# Clean build artifacts
clean:
	rm -rf frontend/dist backend/dist backend/khunquant-web
	mkdir -p backend/dist && touch backend/dist/.gitkeep
