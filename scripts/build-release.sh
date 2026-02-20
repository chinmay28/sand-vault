#!/usr/bin/env bash
# build-release.sh — Build SAND release binaries for all platforms.
#
# Usage:
#   ./scripts/build-release.sh [version]
#
# Produces dist/ with:
#   sand-<version>-linux-amd64
#   sand-<version>-linux-arm64
#   sand-<version>-darwin-amd64
#   sand-<version>-darwin-arm64
#   sand-<version>-windows-amd64.exe
#
# Requirements: Go 1.22+, Node.js 18+

set -euo pipefail

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo "dev")}"
DIST="dist"
LDFLAGS="-s -w -X main.version=${VERSION}"

echo "==> SAND release build  version=${VERSION}"

# ── 1. Frontend ──────────────────────────────────────────────────────────────
echo "==> Building React frontend…"
(cd web && npm ci && npm run build)
echo "    frontend built → internal/server/dist/"

# ── 2. Create dist dir ───────────────────────────────────────────────────────
rm -rf "${DIST}"
mkdir -p "${DIST}"

# ── 3. Cross-compile ─────────────────────────────────────────────────────────
build() {
    local GOOS="$1" GOARCH="$2"
    local OUT="${DIST}/sand-${VERSION}-${GOOS}-${GOARCH}"
    [[ "${GOOS}" == "windows" ]] && OUT="${OUT}.exe"

    echo "    compiling ${GOOS}/${GOARCH}…"
    CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" \
        go build -ldflags="${LDFLAGS}" -o "${OUT}" ./cmd/sand
}

build linux   amd64
build linux   arm64
build darwin  amd64
build darwin  arm64
build windows amd64

# ── 4. Checksums ─────────────────────────────────────────────────────────────
echo "==> Generating checksums…"
(cd "${DIST}" && sha256sum sand-* > SHA256SUMS)

echo ""
echo "==> Done. Artifacts in ${DIST}/:"
ls -lh "${DIST}/"
