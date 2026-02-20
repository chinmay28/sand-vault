.PHONY: build build-web build-go release test test-cover test-e2e test-e2e-slow test-all clean

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

build-go:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BUILD_OUT) ./cmd/sand

# Cross-compile release binaries for all platforms (requires Linux/macOS host)
release: build-web
	./scripts/build-release.sh

# Run all Go unit tests
test:
	go test -v -count=1 ./internal/...

# Run Go tests with coverage report
test-cover:
	go test -v -count=1 -coverprofile=coverage.out ./internal/...
	go tool cover -html=coverage.out -o coverage.html

# Run Python e2e tests (CLI + API + browser)
# Skips slow tests (large files) and GUI tests if React frontend is not built
test-e2e: build-go
	cd tests && python -m pytest -v -m "not slow"

# Run all e2e tests including slow ones
test-e2e-slow: build-go
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
