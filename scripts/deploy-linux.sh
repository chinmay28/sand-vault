#!/usr/bin/env bash
# deploy-linux.sh — Install SAND on a Linux server as a systemd service.
#
# Usage (run as root or with sudo):
#   sudo ./scripts/deploy-linux.sh [binary] [port] [bind]
#
# Defaults:
#   binary  = ./sand  (or sand.exe will be tried)
#   port    = 8080
#   bind    = 127.0.0.1   (change to 0.0.0.0 to expose publicly)
#
# After running:
#   systemctl status sand
#   journalctl -u sand -f

set -euo pipefail

BINARY="${1:-}"
PORT="${2:-8080}"
BIND="${3:-127.0.0.1}"

# ── Locate binary ────────────────────────────────────────────────────────────
if [[ -z "${BINARY}" ]]; then
    for candidate in ./sand ./sand.exe dist/sand-*-linux-amd64; do
        if [[ -f "${candidate}" ]]; then
            BINARY="${candidate}"
            break
        fi
    done
fi

if [[ -z "${BINARY}" || ! -f "${BINARY}" ]]; then
    echo "ERROR: sand binary not found. Build it first:"
    echo "  make build"
    echo "  # or for a specific target:"
    echo "  ./scripts/build-release.sh"
    exit 1
fi

echo "==> Deploying SAND"
echo "    binary : ${BINARY}"
echo "    port   : ${PORT}"
echo "    bind   : ${BIND}"

# ── Install binary ───────────────────────────────────────────────────────────
install -m 755 "${BINARY}" /usr/local/bin/sand
echo "    installed → /usr/local/bin/sand"

# ── Create service user ──────────────────────────────────────────────────────
if ! id -u sand &>/dev/null; then
    useradd --system --no-create-home --shell /usr/sbin/nologin sand
    echo "    created system user 'sand'"
fi

# ── Write systemd unit ───────────────────────────────────────────────────────
cat > /etc/systemd/system/sand.service <<EOF
[Unit]
Description=SAND — Secure Archival Network Distribution
After=network.target

[Service]
Type=simple
User=sand
Group=sand
ExecStart=/usr/local/bin/sand serve --port ${PORT} --bind ${BIND}
Restart=on-failure
RestartSec=5s
# Security hardening
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/tmp

[Install]
WantedBy=multi-user.target
EOF

echo "    wrote /etc/systemd/system/sand.service"

# ── Enable and start ─────────────────────────────────────────────────────────
systemctl daemon-reload
systemctl enable sand
systemctl restart sand

echo ""
echo "==> SAND is running!"
echo "    status : systemctl status sand"
echo "    logs   : journalctl -u sand -f"
echo "    url    : http://${BIND}:${PORT}"
if [[ "${BIND}" == "127.0.0.1" ]]; then
    echo ""
    echo "    NOTE: bound to localhost only. To expose publicly either:"
    echo "      - re-run with bind=0.0.0.0"
    echo "      - place a reverse proxy (nginx/caddy) in front"
fi
