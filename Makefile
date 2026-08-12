.PHONY: build build-web build-go version release test test-cover test-e2e test-e2e-slow test-all clean

# Build everything: frontend + Go binary
build: build-web build-go

# Build React frontend
build-web:
	cd web && npm ci && npm run build

# Build Go binary (static, no CGO)
# On Windows the output is sand.exe; on Unix it is sand.
ifeq ($(OS),Windows_NT)
BUILD_OUT := sand.exe
else
BUILD_OUT := sand
endif

# The patch number is the repo's commit count, which only exists at build time —
# stamp it in (see internal/version and scripts/version.mjs). A bare `go build`
# leaves it at 0, which reads as "unstamped development build".
VERSION_PKG := github.com/chinmay28/sand-vault/internal/version
PATCH := $(shell node scripts/version.mjs --patch 2>/dev/null || echo 0)

build-go:
	CGO_ENABLED=0 go build -trimpath \
		-ldflags="-s -w -X $(VERSION_PKG).Patch=$(PATCH)" \
		-o $(BUILD_OUT) ./cmd/sand

# Print the version this tree would build as
version:
	@node scripts/version.mjs

# Cross-compile release binaries for all platforms (requires Linux/macOS host)
release: build-web
	./scripts/build-release.sh

# Run all Go unit tests
test:
	go test -v -count=1 ./...

# Run Go tests with coverage report
test-cover:
	go test -v -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

# Run Python e2e tests (CLI + API + browser)
# Needs the frontend built so the browser tests have a real app to drive.
# Skips slow tests (large files).
# If Playwright cannot launch its bundled Chromium, point it at one you have:
#   PLAYWRIGHT_CHROMIUM_EXECUTABLE=/path/to/chrome make test-e2e
test-e2e: build
	cd tests && python -m pytest -v -m "not slow"

# Run all e2e tests including slow ones
test-e2e-slow: build
	cd tests && python -m pytest -v

# Run everything: Go unit tests + e2e tests
test-all: test test-e2e

# Install Python test dependencies
test-deps:
	pip install -r tests/requirements.txt
	python -m playwright install chromium

# Clean build artifacts
clean:
	rm -f sand sand.exe coverage.out coverage.html
	rm -rf internal/server/dist
	rm -rf web/node_modules web/dist

# Quick development (build Go only, assumes web is already built)
dev: build-go
	./$(BUILD_OUT) serve --port 8080
