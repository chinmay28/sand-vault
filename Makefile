.PHONY: build build-web build-go icons icons-check version release test test-cover test-e2e test-e2e-slow test-all clean

# Build everything: frontend + Go binary
build: build-web build-go

# Build React frontend
build-web:
	cd web && npm ci && npm run build

# Redraw the home-screen icon PNGs from web/public/icon.svg. The PNGs are
# committed, so run this whenever the SVG changes and commit the result.
icons:
	node scripts/make-icons.mjs

# Fail if a committed PNG no longer matches the SVG it is drawn from.
icons-check:
	node scripts/make-icons.mjs --check

# Build Go binary (static, no CGO)
# On Windows the output is sand.exe; on Unix it is sand.
ifeq ($(OS),Windows_NT)
BUILD_OUT := sand.exe
else
BUILD_OUT := sand
endif

# The patch number is the repo's commit count and the release line is the
# commit's month, both of which only exist at build time — stamp them in (see
# internal/version and scripts/version.mjs). A bare `go build` leaves the patch
# at 0, which reads as "unstamped development build".
VERSION_PKG := github.com/chinmay28/sand-vault/internal/version
PATCH := $(shell node scripts/version.mjs --patch 2>/dev/null || echo 0)
DATE := $(shell node scripts/version.mjs --date 2>/dev/null || echo "")

build-go:
	CGO_ENABLED=0 go build -trimpath \
		-ldflags="-s -w -X $(VERSION_PKG).Patch=$(PATCH) -X $(VERSION_PKG).Date=$(DATE)" \
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

# Clean build artifacts.
#
# internal/server/dist is deliberately NOT removed: it is the built web client
# the Go binary embeds (go:embed in internal/server/server.go), and it is
# committed, so that `go build` and scripts/quickstart.sh work from a bare
# checkout with no Node. Deleting it broke the next `make build-go` (go:embed
# refuses a pattern that matches nothing) until `make build-web` had run, and
# left `git status` showing a hundred deleted files. `make build-web` rewrites
# it from source; `git checkout -- internal/server/dist` puts the committed one
# back.
clean:
	rm -f sand sand.exe coverage.out coverage.html
	rm -rf web/node_modules web/dist

# Quick development (build Go only, assumes web is already built)
dev: build-go
	./$(BUILD_OUT) serve --port 8123
